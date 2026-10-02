#ifndef REPOGO_IMPORT_H
#define REPOGO_IMPORT_H

#include <stddef.h>
#include <stdint.h>

#define REPOGO_AGENT_CLAUDE 0
#define REPOGO_AGENT_CODEX 1

// Parses every complete line of a transcript segment into a JSON array of
// events. Returns NULL when the file cannot be read. Release with repogo_free.
uint8_t *repogo_parse_file(uint32_t agent, const char *path, const char *session_id, size_t *out_len);
// Reads every complete line of a transcript for its token usage per request.
// Returns NULL when the file cannot be read. Release with repogo_free.
uint8_t *repogo_scan_usage(uint32_t agent, const char *path, size_t *out_len);
void repogo_free(uint8_t *ptr, size_t len);

#endif
