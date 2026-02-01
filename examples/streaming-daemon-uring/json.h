/*
 * json.h - JSON parsing helpers using jsmn
 */
#ifndef JSON_H
#define JSON_H

/* Only include jsmn declarations in header */
#define JSMN_HEADER
#include "jsmn.h"
#undef JSMN_HEADER
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>

/* Maximum tokens for handoff JSON (object + ~10 key-value pairs) */
#define MAX_JSON_TOKENS 32

/* Compare token string with a C string */
bool json_tok_eq(const char *json, const jsmntok_t *tok, const char *s);

/* Get token string length */
int json_tok_len(const jsmntok_t *tok);

/* Copy token string to buffer (null-terminated) */
int json_tok_copy(const char *json, const jsmntok_t *tok,
                  char *buf, size_t buflen);

/* Parse token as integer (returns 0 on error) */
int64_t json_tok_int(const char *json, const jsmntok_t *tok);

/* Find key in object, returns token index of value or -1 */
int json_find_key(const char *json, const jsmntok_t *tokens,
                  int num_tokens, const char *key);

/* Parse JSON object and extract values
 * Returns 0 on success, -1 on error
 */
int json_parse(const char *json, size_t len,
               jsmntok_t *tokens, int max_tokens);

#endif /* JSON_H */
