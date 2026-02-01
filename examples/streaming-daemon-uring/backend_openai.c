/*
 * backend_openai.c - OpenAI API backend using libcurl
 *
 * Makes streaming requests to OpenAI-compatible APIs and parses
 * SSE responses. Called from thread pool workers.
 *
 * Build with: make BACKEND=openai
 * Requires: libcurl-dev
 *
 * Environment variables:
 *   OPENAI_API_KEY      - Required. API key for authentication
 *   OPENAI_API_BASE     - Optional. Base URL (default: https://api.openai.com/v1)
 *   OPENAI_MODEL        - Optional. Model to use (default: gpt-4o-mini)
 */
#include "backend.h"
#include "streaming.h"
#include <curl/curl.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

/* Configuration */
static char *api_key = NULL;
static char *api_base = NULL;
static char *default_model = NULL;

/* Response buffer for curl callbacks */
typedef struct {
    connection_t *conn;
    char *buffer;
    size_t size;
    size_t capacity;
    int chunk_count;
    int error;
} response_ctx_t;

/* Curl write callback - accumulates SSE chunks */
static size_t write_callback(char *ptr, size_t size, size_t nmemb, void *userdata) {
    response_ctx_t *ctx = (response_ctx_t *)userdata;
    size_t bytes = size * nmemb;

    /* Expand buffer if needed */
    if (ctx->size + bytes + 1 > ctx->capacity) {
        size_t new_cap = ctx->capacity * 2;
        if (new_cap < ctx->size + bytes + 1) {
            new_cap = ctx->size + bytes + 1;
        }
        char *new_buf = realloc(ctx->buffer, new_cap);
        if (!new_buf) {
            ctx->error = 1;
            return 0;
        }
        ctx->buffer = new_buf;
        ctx->capacity = new_cap;
    }

    memcpy(ctx->buffer + ctx->size, ptr, bytes);
    ctx->size += bytes;
    ctx->buffer[ctx->size] = '\0';

    return bytes;
}

/* Parse SSE data line and extract content */
static int parse_sse_chunk(const char *line, char *content, size_t content_size) {
    /* SSE format: data: {"choices":[{"delta":{"content":"..."}}]} */
    if (strncmp(line, "data: ", 6) != 0) {
        return -1;
    }

    const char *json = line + 6;

    /* Check for [DONE] marker */
    if (strncmp(json, "[DONE]", 6) == 0) {
        return 0; /* End of stream */
    }

    /* Simple JSON parsing - find "content":" */
    const char *content_key = strstr(json, "\"content\":\"");
    if (!content_key) {
        return -1; /* No content field */
    }

    content_key += 11; /* Skip "content":" */

    /* Extract content until closing quote, handling escapes */
    size_t i = 0;
    while (*content_key && *content_key != '"' && i < content_size - 1) {
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

    return (i > 0) ? 1 : -1;
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

    default_model = getenv("OPENAI_MODEL");
    if (!default_model) {
        default_model = "gpt-4o-mini";
    }

    fprintf(stderr, "OpenAI backend initialized (base: %s, model: %s)\n",
            api_base, default_model);

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

    /* Build URL */
    char url[512];
    snprintf(url, sizeof(url), "%s/chat/completions", api_base);

    /* Build request body */
    const char *model = conn->data.model ? conn->data.model : default_model;
    const char *prompt = conn->data.prompt ? conn->data.prompt : "Hello";
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

    /* Response context */
    response_ctx_t resp = {
        .conn = conn,
        .buffer = malloc(4096),
        .size = 0,
        .capacity = 4096,
        .chunk_count = 0,
        .error = 0
    };

    if (!resp.buffer) {
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
    curl_easy_setopt(curl, CURLOPT_TIMEOUT, 120L);
    curl_easy_setopt(curl, CURLOPT_CONNECTTIMEOUT, 10L);

    /* Perform request */
    CURLcode res = curl_easy_perform(curl);

    long http_code = 0;
    curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &http_code);

    curl_slist_free_all(headers);
    curl_easy_cleanup(curl);

    if (res != CURLE_OK || resp.error) {
        fprintf(stderr, "Curl error: %s\n", curl_easy_strerror(res));
        free(resp.buffer);
        return -1;
    }

    if (http_code != 200) {
        fprintf(stderr, "API error %ld: %.200s\n", http_code, resp.buffer);
        free(resp.buffer);
        return -1;
    }

    /* Parse SSE response and build message list */
    /* For now, we'll store the accumulated content and stream it */
    char accumulated[65536];
    size_t acc_len = 0;

    char *line = resp.buffer;
    while (line && *line) {
        char *next = strstr(line, "\n");
        if (next) {
            *next = '\0';
            next++;
            /* Skip empty lines and \r */
            while (*next == '\n' || *next == '\r') next++;
        }

        char content[4096];
        int parse_result = parse_sse_chunk(line, content, sizeof(content));
        if (parse_result == 1) {
            /* Got content */
            size_t clen = strlen(content);
            if (acc_len + clen < sizeof(accumulated) - 1) {
                memcpy(accumulated + acc_len, content, clen);
                acc_len += clen;
            }
            resp.chunk_count++;
        } else if (parse_result == 0) {
            /* [DONE] */
            break;
        }

        line = next;
    }
    accumulated[acc_len] = '\0';

    free(resp.buffer);

    /* Store accumulated response for streaming */
    /* We'll use the connection's handoff_buf to store it since we're done with handoff data */
    if (acc_len > 0 && acc_len < MAX_HANDOFF_DATA_SIZE) {
        memcpy(conn->handoff_buf, accumulated, acc_len + 1);
        conn->handoff_len = acc_len;
    } else {
        /* Fallback: truncate */
        size_t copy_len = acc_len < MAX_HANDOFF_DATA_SIZE - 1 ? acc_len : MAX_HANDOFF_DATA_SIZE - 1;
        memcpy(conn->handoff_buf, accumulated, copy_len);
        conn->handoff_buf[copy_len] = '\0';
        conn->handoff_len = copy_len;
    }

    /* Initialize streaming - we'll need to modify streaming.c to support this */
    stream_start(conn);

    return 0;
}
