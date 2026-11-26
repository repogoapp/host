#include <stdint.h>

// Starts capturing the booted simulator udid, writing H.264 frames to fd out,
// each a 4-byte big-endian length, the capture time (8 bytes, big-endian
// microseconds), then its length-prefixed NAL units; 0 on
// success, else a negative code with why in err.
// universal: touches and keys go through Universal HID (iOS 27), not Indigo.
// headless: no frames, only input and the screen's elements.
int simcap_start(const char *developer_dir, const char *udid, int universal, int headless, int out, char *err,
                 int err_len);
// The panel CoreDevice reports active, by screen ID; 0 until it says.
uint32_t simcap_active_screen(void);
// A panel's size in points, upright, from the device type; 0 on success.
// Screen 0 is the largest.
int simcap_panel_size(uint32_t screen, double *width, double *height);
// phase: 0 down, 1 move, 2 up. x and y are fractions of the screen.
void simcap_touch(int phase, double x, double y);
// An Indigo button by its event source (0 home, 1 lock, 0x400002 Siri, 0xbb8
// side, 0x3f0 software keyboard), held hold_ms before release.
void simcap_button(int source, int hold_ms);
void simcap_hid(int page, int usage);
// A hardware button by its HID page and usage, through the simulator's
// dtuhidd (Xcode 27), held hold_ms before release; 0 on success, negative
// where the simulator has no dtuhidd.
int simcap_service_button(int page, int usage, int hold_ms);
void simcap_key(int usage, int down);
int simcap_rotate(int orientation);
// A foldable's pose: closed, open, book or laptop. 0 on success.
int simcap_fold(const char *pose);
// A Mac screen's frames: start the encoder on fd out, then hand it BGRA pixels.
void simcap_screen_start(int out, int maxShortSide, int bitRate);
void simcap_encode_pixels(const void *pixels, int width, int height, int stride, int rgba);
// Writes a JSON message for the host, on stdout beside the frames.
void simcap_tell(const char *json, int len);
// Makes the next frame a keyframe, and encodes the screen again even if it
// hasn't changed, so a phone whose decoder failed recovers at once.
void simcap_keyframe(void);
// Turns frames, from the next, by quarter turns clockwise (0 to 3) to show
// the screen upright; touches still come as fractions of the framebuffer.
void simcap_turn(int turns);
// The turn the frames go out at now: the panel's own, read at start, until
// the host sends one.
int simcap_current_turn(void);
void simcap_credit(int frames);
void simcap_bitrate(int bitsPerSecond);
void simcap_memory_warning(void);
// Shows (on) or hides the on-screen keyboard; 0 on success.
int simcap_onscreen_keyboard(int on);
void simcap_ca_debug(const char *name, int enabled);
void simcap_stop(void);
