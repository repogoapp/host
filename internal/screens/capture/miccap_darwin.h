#ifndef MICCAP_DARWIN_H
#define MICCAP_DARWIN_H

// Makes a phone's microphone an input on this Mac, the device uid named
// name: plays the 16-bit mono 24 kHz PCM read from in (each chunk behind a
// 4-byte big-endian length) muted, and taps it into a public device. Says
// it's ready on out, as the capture children's host messages. Returns when
// in closes, removing the device; non-zero with why on failure.
int miccap_run(int in, int out, const char *uid, const char *name, char *why, int whyLength);

// Removes every device whose ID starts with prefix.
void miccap_remove(const char *prefix);

#endif
