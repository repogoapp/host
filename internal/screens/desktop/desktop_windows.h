// The Windows desktop for desktop_windows.go: the windows the taskbar and
// Alt+Tab show, the front one's buttons, the Start menu's apps and their
// icons, and notice of changes. Strings are UTF-8.
#pragma once
#include <stdint.h>

typedef struct {
  uint32_t hwnd;
  uint32_t pid;
  int minimized;
  int maximized;
  int canMinimize;
  int canMaximize;
  char title[512];
  // app is the window's AppUserModelID when it has one; exe its program.
  char app[256];
  char exe[520];
} wd_window;

void wd_init(void);
// wd_windows fills out with up to max windows, front to back; their count.
int wd_windows(wd_window *out, int max);
// wd_foreground describes the front window into out: 1 for an app's
// window, 2 for another (the desktop, the taskbar), 0 for none.
int wd_foreground(wd_window *out);
int wd_locked(void);

// wd_name is an app's name: an AppUserModelID's from the Start menu, a
// program's from its version info. malloc'd; NULL when unknown.
char *wd_name(const char *key);
// wd_apps lists the Start menu's apps as "name\tid\n" lines. malloc'd.
char *wd_apps(void);
// wd_icon draws an app's icon into out, px×px premultiplied BGRA; 0 on success.
int wd_icon(const char *key, int px, uint8_t *out);
// wd_launch opens an AppUserModelID or a program; 0 on success.
int wd_launch(const char *key);

// Window moves: 0 close, 1 minimize, 2 maximize or restore.
int wd_button(uint32_t hwnd, int button);
void wd_close(uint32_t hwnd);
void wd_raise(uint32_t hwnd);
// wd_frame reads a window's visible frame and its monitor's work area;
// wd_place moves its visible frame there, out of maximized first.
int wd_frame(uint32_t hwnd, int32_t *frame, int32_t *work);
void wd_place(uint32_t hwnd, int32_t x, int32_t y, int32_t w, int32_t h);

// wd_watch starts telling goChanged of the front window changing and of
// windows showing, hiding, minimizing or retitling.
void wd_watch(void);
