/*
 * streaming.c - SSE streaming to client
 */
#include "streaming.h"
#include <stdio.h>
#include <string.h>

/* Demo messages matching Go/Rust daemon behavior.
 * Total: 2 dynamic + 16 static = 18 messages.
 */
static const char *demo_messages[] = {
    "This daemon uses io_uring for async I/O.",
    "Single-threaded event loop with high concurrency.",
    "Expected capacity: 100,000+ concurrent connections.",
    "Memory per connection: ~68 KB (64KB handoff + 4KB write buffer).",
    "The Apache worker was freed immediately after handoff.",
    "Replace this mock backend with your LLM API integration.",
    "io_uring provides true async I/O with kernel polling.",
    "Zero-copy operations minimize memory bandwidth.",
    "Multishot accept reduces syscall overhead.",
    "This is pure C with no runtime overhead.",
    "This is message 13 of 18.",
    "This is message 14 of 18.",
    "This is message 15 of 18.",
    "This is message 16 of 18.",
    "This is message 17 of 18.",
    "[DONE-CONTENT]",
};

#define NUM_DEMO_MESSAGES (sizeof(demo_messages) / sizeof(demo_messages[0]))

/* Target chunk size for LLM response streaming (~100-200 chars like demo messages) */
#define LLM_CHUNK_TARGET_SIZE 150

/* Static buffer for first two dynamic messages */
static __thread char dynamic_msg1[256];
static __thread char dynamic_msg2[128];

/* Static buffer for LLM response chunks */
static __thread char llm_chunk_buf[LLM_CHUNK_TARGET_SIZE + 64];

const char **stream_get_messages(int *count) {
    *count = NUM_DEMO_MESSAGES;
    return demo_messages;
}

static const char *truncate_prompt(const char *prompt, size_t max_len) {
    static __thread char truncated[64];
    if (!prompt) {
        return "(none)";
    }
    size_t len = strlen(prompt);
    if (len <= max_len) {
        return prompt;
    }
    /* Clamp max_len to ensure space for ellipsis and null terminator */
    if (max_len > sizeof(truncated) - 4) {
        max_len = sizeof(truncated) - 4;
    }
    snprintf(truncated, sizeof(truncated), "%.*s...", (int)max_len, prompt);
    return truncated;
}

const char *stream_next_message(connection_t *conn, size_t *len) {
    /* If streaming LLM response, chunk the accumulated content */
    if (conn->use_llm_response) {
        size_t remaining = conn->handoff_len - conn->llm_response_pos;
        if (remaining == 0) {
            /* Done streaming LLM response */
            *len = 0;
            return NULL;
        }

        /* Determine chunk size - try to break on word boundaries */
        size_t chunk_size = remaining < LLM_CHUNK_TARGET_SIZE ? remaining : LLM_CHUNK_TARGET_SIZE;

        /* If not at end, try to find a word boundary */
        if (chunk_size < remaining) {
            const char *start = conn->handoff_buf + conn->llm_response_pos;
            size_t i = chunk_size;
            /* Look backward for a space */
            while (i > 0 && start[i] != ' ' && start[i] != '\n') {
                i--;
            }
            /* Use word boundary if found, otherwise use full chunk */
            if (i > 0) {
                chunk_size = i + 1; /* Include the space */
            }
        }

        /* Copy chunk to buffer */
        memcpy(llm_chunk_buf, conn->handoff_buf + conn->llm_response_pos, chunk_size);
        llm_chunk_buf[chunk_size] = '\0';

        conn->llm_response_pos += chunk_size;
        conn->stream_index++;
        *len = chunk_size;
        return llm_chunk_buf;
    }

    /* Demo mode: use static messages */
    int index = conn->stream_index;

    if (index == 0) {
        /* First message: greeting with prompt preview */
        snprintf(dynamic_msg1, sizeof(dynamic_msg1),
                 "Hello from C io_uring daemon! Prompt: %s",
                 truncate_prompt(conn->data.prompt, 50));
        *len = strlen(dynamic_msg1);
        conn->stream_index++;
        return dynamic_msg1;
    }

    if (index == 1) {
        /* Second message: user ID */
        snprintf(dynamic_msg2, sizeof(dynamic_msg2),
                 "User ID: %ld", (long)conn->data.user_id);
        *len = strlen(dynamic_msg2);
        conn->stream_index++;
        return dynamic_msg2;
    }

    /* Static messages (index 2 to 17) */
    int static_index = index - 2;
    if (static_index >= 0 && (size_t)static_index < NUM_DEMO_MESSAGES) {
        *len = strlen(demo_messages[static_index]);
        conn->stream_index++;
        return demo_messages[static_index];
    }

    /* Done */
    *len = 0;
    return NULL;
}

int stream_format_sse(char *buf, size_t buflen, const char *content) {
    /* Format: data: {"content":"..."}\n\n
     * We need to escape special characters in JSON
     */
    char escaped[4096];
    char *out = escaped;
    size_t remaining = sizeof(escaped) - 1;

    while (*content && remaining > 6) { /* Room for \uXXXX */
        char c = *content++;
        switch (c) {
        case '"':
            *out++ = '\\';
            *out++ = '"';
            remaining -= 2;
            break;
        case '\\':
            *out++ = '\\';
            *out++ = '\\';
            remaining -= 2;
            break;
        case '\n':
            *out++ = '\\';
            *out++ = 'n';
            remaining -= 2;
            break;
        case '\r':
            *out++ = '\\';
            *out++ = 'r';
            remaining -= 2;
            break;
        case '\t':
            *out++ = '\\';
            *out++ = 't';
            remaining -= 2;
            break;
        default:
            if ((unsigned char)c < 32) {
                /* Control character - escape as \uXXXX */
                int n = snprintf(out, remaining + 1, "\\u%04x", (unsigned char)c);
                out += n;
                remaining -= n;
            } else {
                *out++ = c;
                remaining--;
            }
            break;
        }
    }
    *out = '\0';

    int n = snprintf(buf, buflen, "data: {\"content\":\"%s\"}\n\n", escaped);
    if (n < 0 || (size_t)n >= buflen) {
        return -1;
    }
    return n;
}

int stream_start(connection_t *conn) {
    /* Headers are sent separately via ring_submit_write */
    conn->stream_index = 0;
    return 0;
}
