/*
 * json.c - JSON parsing helpers using jsmn
 */
/* Include jsmn implementation (only in this file) */
#include "jsmn.h"

#include "json.h"
#include <string.h>
#include <stdlib.h>

bool json_tok_eq(const char *json, const jsmntok_t *tok, const char *s) {
    if (tok->type != JSMN_STRING) {
        return false;
    }
    int len = tok->end - tok->start;
    return (int)strlen(s) == len && strncmp(json + tok->start, s, len) == 0;
}

int json_tok_len(const jsmntok_t *tok) {
    return tok->end - tok->start;
}

int json_tok_copy(const char *json, const jsmntok_t *tok,
                  char *buf, size_t buflen) {
    int len = tok->end - tok->start;
    if ((size_t)len >= buflen) {
        return -1;
    }
    memcpy(buf, json + tok->start, len);
    buf[len] = '\0';
    return len;
}

int64_t json_tok_int(const char *json, const jsmntok_t *tok) {
    if (tok->type != JSMN_PRIMITIVE) {
        return 0;
    }
    char buf[32];
    int len = tok->end - tok->start;
    if (len >= (int)sizeof(buf)) {
        return 0;
    }
    memcpy(buf, json + tok->start, len);
    buf[len] = '\0';
    return strtoll(buf, NULL, 10);
}

int json_find_key(const char *json, const jsmntok_t *tokens,
                  int num_tokens, const char *key) {
    if (num_tokens < 1 || tokens[0].type != JSMN_OBJECT) {
        return -1;
    }

    /* Iterate through key-value pairs */
    int i = 1;
    while (i < num_tokens) {
        if (json_tok_eq(json, &tokens[i], key)) {
            /* Return index of value (next token) */
            if (i + 1 < num_tokens) {
                return i + 1;
            }
            return -1;
        }

        /* Skip to next key
         * Simple case: skip key and its value (assuming primitive or string)
         * For nested objects/arrays, we'd need to count size properly
         */
        i++;
        if (i < num_tokens) {
            /* Skip value - for primitives/strings just skip one token */
            if (tokens[i].type == JSMN_OBJECT || tokens[i].type == JSMN_ARRAY) {
                /* Skip entire nested structure by counting all children */
                int to_skip = tokens[i].size * 2; /* rough estimate */
                i += 1 + to_skip;
            } else {
                i++;
            }
        }
    }
    return -1;
}

int json_parse(const char *json, size_t len,
               jsmntok_t *tokens, int max_tokens) {
    jsmn_parser parser;
    jsmn_init(&parser);
    int r = jsmn_parse(&parser, json, len, tokens, max_tokens);
    if (r < 0) {
        return -1;
    }
    return r;
}
