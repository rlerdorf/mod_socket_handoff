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

/* Static buffer for first two dynamic messages */
static __thread char dynamic_msg1[256];
static __thread char dynamic_msg2[128];

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
    snprintf(truncated, sizeof(truncated), "%.50s...", prompt);
    return truncated;
}

const char *stream_next_message(connection_t *conn, size_t *len) {
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
