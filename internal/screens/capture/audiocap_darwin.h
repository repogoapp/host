#ifndef AUDIOCAP_DARWIN_H
#define AUDIOCAP_DARWIN_H

// Captures everything this Mac plays through a Core Audio process tap,
// encodes it to AAC-LC (48 kHz stereo) and writes it to fd as the capture
// children write frames. mute silences the Mac's own output while it runs;
// excludePID, when above 0, is a process left out (the iPhone mic's).
// Blocks until the process ends; returns non-zero with why on failure.
int audiocap_run(int fd, int mute, int excludePID, char *why, int whyLength);

#endif
