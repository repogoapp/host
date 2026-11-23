#include <stdint.h>

// Sets up reading the device's screen (a SimDevice) by label; 0 on success,
// negative where this Xcode or macOS lacks the frameworks.
int simax_start(void *device, const char *developer_dir);
// What's on the screen as JSON, {"app","width","height","elements":[...]}
// or {"error"}, in points of the frontmost app as it's turned; display is the
// active panel's screen ID, for elements found only by point, and sideways
// whether the screen shows turned a quarter. Free the answer.
char *simax_elements(uint32_t display, int sideways);
