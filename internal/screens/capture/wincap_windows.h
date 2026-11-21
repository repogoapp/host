// wincap: this Windows PC's screen as H.264, for `repogo screencap`. Desktop
// Duplication reads the main monitor, the cursor is drawn in, and Media
// Foundation's encoder makes the frames, written to out the way simcap writes
// them. Input goes in through SendInput.
#include <stdint.h>

// wincap_start opens the screen and the encoder and starts capturing on a
// thread of its own; nonzero with why in err when the screen can't be read.
int wincap_start(uintptr_t out, int maxShortSide, int bitRate, char *err, int errLen);
void wincap_stop(void);

// Pacing from the host: frames allowed now, the bitrate, a keyframe next.
void wincap_credit(int frames);
void wincap_bitrate(int bitsPerSecond);
void wincap_keyframe(void);

// Input, the pointer as a fraction of the main monitor.
void wincap_pointer(double x, double y);
void wincap_button(int button, int down);  // 0 left, 1 middle, 2 right
// wincap_wheel turns the wheel by delta, 120 a whole click (WHEEL_DELTA).
void wincap_wheel(int vertical, int delta);
void wincap_vk(int vk, int down, int extended);
void wincap_unicode(uint16_t unit);
// wincap_vk_scan is the key that types ch on this keyboard: VkKeyScanW.
int wincap_vk_scan(uint16_t ch);

// wincap_now_us is the clock frames are stamped with, in microseconds.
int64_t wincap_now_us(void);
