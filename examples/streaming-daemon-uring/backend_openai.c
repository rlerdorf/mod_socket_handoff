/*
 * backend_openai.c - OpenAI API backend using libcurl
 *
 * Makes streaming requests to OpenAI-compatible APIs and forwards
 * SSE responses to the client in real-time as they arrive.
 * Called from thread pool workers.
 *
 * Build with: make BACKEND=openai
 * Requires: libcurl-dev
 *
 * Environment variables:
 *   OPENAI_API_KEY      - Required. API key for authentication
 *   OPENAI_API_BASE     - Optional. Base URL (default: https://api.openai.com/v1)
 *   OPENAI_API_SOCKET   - Optional. Unix socket path for API connections
 *   OPENAI_MODEL        - Optional. Model to use (default: gpt-4o-mini)
 */
#include "backend.h"
#include "streaming.h"
#include <curl/curl.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <unistd.h>
#include <errno.h>

/* Configuration */
static char *api_key = NULL;
static char *api_base = NULL;
static char *api_socket = NULL;  /* Unix socket path for API connections */
static char *default_model = NULL;

/* Response context for curl callbacks - streams directly to client */
typedef struct {
    connection_t *conn;
    int client_fd;              /* Client TCP socket for direct writes */
    char *line_buf;             /* Buffer for partial SSE lines */
    size_t line_len;
    size_t line_capacity;
    int chunk_count;
    size_t bytes_sent;
    int error;
    bool headers_sent;          /* Track if HTTP headers sent */
} response_ctx_t;

/* Write all bytes to fd, handling partial writes */
static ssize_t write_all(int fd, const void *buf, size_t len) {
    size_t written = 0;
    while (written < len) {
        ssize_t n = write(fd, (const char *)buf + written, len - written);
        if (n < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        written += n;
    }
    return (ssize_t)written;
}

/* Send HTTP headers to client (called on first data) */
static int send_headers(response_ctx_t *ctx) {
    if (ctx->headers_sent) return 0;

    ssize_t n = write_all(ctx->client_fd, SSE_HEADERS, strlen(SSE_HEADERS));
    if (n < 0) {
        ctx->error = 1;
        return -1;
    }
    ctx->bytes_sent += n;
    ctx->headers_sent = true;
    return 0;
}

/* Forward a content chunk to the client as SSE */
static int forward_chunk(response_ctx_t *ctx, const char *content) {
    char sse_buf[8192];
    int len = stream_format_sse(sse_buf, sizeof(sse_buf), content);
    if (len < 0) {
        return -1;
    }

    ssize_t n = write_all(ctx->client_fd, sse_buf, len);
    if (n < 0) {
        ctx->error = 1;
        return -1;
    }
    ctx->bytes_sent += n;
    ctx->chunk_count++;
    return 0;
}

/* Parse a single SSE data line and forward content to client */
static int process_sse_line(response_ctx_t *ctx, const char *line) {
    /* SSE format: data: {"choices":[{"delta":{"content":"..."}}]} */
    if (strncmp(line, "data: ", 6) != 0) {
        return 0; /* Not a data line, skip */
    }

    const char *json = line + 6;

    /* Check for [DONE] marker */
    if (strncmp(json, "[DONE]", 6) == 0) {
        return 0; /* Will send our own [DONE] after curl completes */
    }

    /* Simple JSON parsing - find "content":" */
    const char *content_key = strstr(json, "\"content\":\"");
    if (!content_key) {
        return 0; /* No content field (e.g., role delta) */
    }

    content_key += 11; /* Skip "content":" */

    /* Extract content until closing quote, handling escapes */
    char content[4096];
    size_t i = 0;
    while (*content_key && *content_key != '"' && i < sizeof(content) - 1) {
        if (*content_key == '\\' && *(content_key + 1)) {
            content_key++;
            switch (*content_key) {
            case 'n': content[i++] = '\n'; break;
            case 'r': content[i++] = '\r'; break;
            case 't': content[i++] = '\t'; break;
            case '"': content[i++] = '"'; break;
            case '\\': content[i++] = '\\'; break;
            default: content[i++] = *content_key; break;
            }
        } else {
            content[i++] = *content_key;
        }
        content_key++;
    }
    content[i] = '\0';

    if (i == 0) {
        return 0; /* Empty content */
    }

    /* Forward to client */
    return forward_chunk(ctx, content);
}

/* Curl write callback - parses SSE and forwards chunks in real-time */
static size_t write_callback(char *ptr, size_t size, size_t nmemb, void *userdata) {
    response_ctx_t *ctx = (response_ctx_t *)userdata;
    size_t bytes = size * nmemb;

    if (ctx->error) return 0;

    /* Send headers on first data received */
    if (!ctx->headers_sent) {
        if (send_headers(ctx) < 0) {
            return 0;
        }
    }

    /* Expand line buffer if needed */
    if (ctx->line_len + bytes + 1 > ctx->line_capacity) {
        size_t new_cap = ctx->line_capacity * 2;
        if (new_cap < ctx->line_len + bytes + 1) {
            new_cap = ctx->line_len + bytes + 1;
        }
        char *new_buf = realloc(ctx->line_buf, new_cap);
        if (!new_buf) {
            ctx->error = 1;
            return 0;
        }
        ctx->line_buf = new_buf;
        ctx->line_capacity = new_cap;
    }

    /* Append new data to line buffer */
    memcpy(ctx->line_buf + ctx->line_len, ptr, bytes);
    ctx->line_len += bytes;
    ctx->line_buf[ctx->line_len] = '\0';

    /* Process complete lines (ended by \n) */
    char *line_start = ctx->line_buf;
    char *newline;
    while ((newline = strchr(line_start, '\n')) != NULL) {
        *newline = '\0';

        /* Remove trailing \r if present */
        if (newline > line_start && *(newline - 1) == '\r') {
            *(newline - 1) = '\0';
        }

        /* Process this line */
        if (process_sse_line(ctx, line_start) < 0) {
            return 0; /* Client write error */
        }

        line_start = newline + 1;
    }

    /* Move any remaining partial line to start of buffer */
    if (line_start != ctx->line_buf) {
        size_t remaining = ctx->line_len - (line_start - ctx->line_buf);
        if (remaining > 0) {
            memmove(ctx->line_buf, line_start, remaining);
        }
        ctx->line_len = remaining;
        ctx->line_buf[ctx->line_len] = '\0';
    }

    return bytes;
}

int backend_init(void) {
    /* Initialize libcurl */
    if (curl_global_init(CURL_GLOBAL_DEFAULT) != 0) {
        fprintf(stderr, "Failed to initialize libcurl\n");
        return -1;
    }

    /* Get configuration from environment */
    api_key = getenv("OPENAI_API_KEY");
    if (!api_key || strlen(api_key) == 0) {
        fprintf(stderr, "Warning: OPENAI_API_KEY not set, API calls will fail\n");
    }

    api_base = getenv("OPENAI_API_BASE");
    if (!api_base) {
        api_base = "https://api.openai.com/v1";
    }

    api_socket = getenv("OPENAI_API_SOCKET");

    default_model = getenv("OPENAI_MODEL");
    if (!default_model) {
        default_model = "gpt-4o-mini";
    }

    if (api_socket) {
        fprintf(stderr, "OpenAI backend initialized (base: %s, socket: %s, model: %s)\n",
                api_base, api_socket, default_model);
    } else {
        fprintf(stderr, "OpenAI backend initialized (base: %s, model: %s)\n",
                api_base, default_model);
    }

    return 0;
}

void backend_cleanup(void) {
    curl_global_cleanup();
}

int backend_process(connection_t *conn) {
    if (!api_key) {
        fprintf(stderr, "No API key configured\n");
        return -1;
    }

    CURL *curl = curl_easy_init();
    if (!curl) {
        return -1;
    }

    /* Build URL - if api_base ends with /v1 or /v1/, use that; otherwise append /v1 */
    char url[512];
    size_t base_len = strlen(api_base);

    /* Check if api_base already ends with /v1 */
    if (base_len >= 3 && strcmp(api_base + base_len - 3, "/v1") == 0) {
        snprintf(url, sizeof(url), "%s/chat/completions", api_base);
    } else if (base_len >= 4 && strcmp(api_base + base_len - 4, "/v1/") == 0) {
        snprintf(url, sizeof(url), "%schat/completions", api_base);
    } else {
        snprintf(url, sizeof(url), "%s/v1/chat/completions", api_base);
    }

    /* Copy prompt and model to local buffers (they point into handoff_buf) */
    char prompt_copy[8192];
    char model_copy[128];
    const char *prompt = conn->data.prompt ? conn->data.prompt : "Hello";
    const char *model = conn->data.model ? conn->data.model : default_model;
    snprintf(prompt_copy, sizeof(prompt_copy), "%s", prompt);
    snprintf(model_copy, sizeof(model_copy), "%s", model);
    prompt = prompt_copy;
    model = model_copy;
    int max_tokens = conn->data.max_tokens > 0 ? conn->data.max_tokens : 1024;

    /* Escape prompt for JSON */
    char escaped_prompt[8192];
    char *out = escaped_prompt;
    size_t remaining = sizeof(escaped_prompt) - 1;
    const char *p = prompt;
    while (*p && remaining > 6) {
        switch (*p) {
        case '"':  *out++ = '\\'; *out++ = '"'; remaining -= 2; break;
        case '\\': *out++ = '\\'; *out++ = '\\'; remaining -= 2; break;
        case '\n': *out++ = '\\'; *out++ = 'n'; remaining -= 2; break;
        case '\r': *out++ = '\\'; *out++ = 'r'; remaining -= 2; break;
        case '\t': *out++ = '\\'; *out++ = 't'; remaining -= 2; break;
        default:
            if ((unsigned char)*p < 32) {
                int n = snprintf(out, remaining + 1, "\\u%04x", (unsigned char)*p);
                out += n;
                remaining -= n;
            } else {
                *out++ = *p;
                remaining--;
            }
            break;
        }
        p++;
    }
    *out = '\0';

    char body[16384];
    int body_len = snprintf(body, sizeof(body),
        "{"
        "\"model\":\"%s\","
        "\"messages\":[{\"role\":\"user\",\"content\":\"%s\"}],"
        "\"max_tokens\":%d,"
        "\"stream\":true"
        "}",
        model, escaped_prompt, max_tokens);

    if (body_len < 0 || (size_t)body_len >= sizeof(body)) {
        curl_easy_cleanup(curl);
        return -1;
    }

    /* Set up headers */
    struct curl_slist *headers = NULL;
    headers = curl_slist_append(headers, "Content-Type: application/json");

    char auth_header[256];
    snprintf(auth_header, sizeof(auth_header), "Authorization: Bearer %s", api_key);
    headers = curl_slist_append(headers, auth_header);

    /* Response context for real-time streaming to client */
    response_ctx_t resp = {
        .conn = conn,
        .client_fd = conn->client_fd,
        .line_buf = malloc(4096),
        .line_len = 0,
        .line_capacity = 4096,
        .chunk_count = 0,
        .bytes_sent = 0,
        .error = 0,
        .headers_sent = false
    };

    if (!resp.line_buf) {
        curl_slist_free_all(headers);
        curl_easy_cleanup(curl);
        return -1;
    }

    /* Configure curl */
    curl_easy_setopt(curl, CURLOPT_URL, url);
    curl_easy_setopt(curl, CURLOPT_HTTPHEADER, headers);
    curl_easy_setopt(curl, CURLOPT_POSTFIELDS, body);
    curl_easy_setopt(curl, CURLOPT_WRITEFUNCTION, write_callback);
    curl_easy_setopt(curl, CURLOPT_WRITEDATA, &resp);
    curl_easy_setopt(curl, CURLOPT_TIMEOUT, 300L);  /* Longer timeout for streaming */
    curl_easy_setopt(curl, CURLOPT_CONNECTTIMEOUT, 10L);

    /* Use Unix socket if configured (avoids ephemeral port limits) */
    if (api_socket) {
        curl_easy_setopt(curl, CURLOPT_UNIX_SOCKET_PATH, api_socket);
    }

    /* Perform request - write_callback streams chunks to client in real-time */
    CURLcode res = curl_easy_perform(curl);

    long http_code = 0;
    curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &http_code);

    curl_slist_free_all(headers);
    curl_easy_cleanup(curl);
    free(resp.line_buf);

    /* Check for errors */
    if (res != CURLE_OK || resp.error) {
        if (!resp.headers_sent) {
            /* No data sent yet, return error */
            fprintf(stderr, "Curl error: %s\n", curl_easy_strerror(res));
            return -1;
        }
        /* Partial data sent, still need to finish stream */
    }

    if (http_code != 200 && !resp.headers_sent) {
        fprintf(stderr, "API error %ld\n", http_code);
        return -1;
    }

    /* Send [DONE] marker if we sent headers */
    if (resp.headers_sent) {
        write_all(conn->client_fd, SSE_DONE, strlen(SSE_DONE));
        resp.bytes_sent += strlen(SSE_DONE);
    }

    /* Update connection stats */
    conn->bytes_sent = resp.bytes_sent;

    /* Mark streaming as done - no further io_uring streaming needed
     * since we already wrote everything directly in the callback
     */
    conn->use_llm_response = false;

    return resp.headers_sent ? 1 : -1;  /* Return 1 to indicate streaming done */
}
