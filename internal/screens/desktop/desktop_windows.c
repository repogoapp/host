//go:build windows && cgo

// See desktop_windows.h.
#define COBJMACROS
#include <windows.h>
#include <dwmapi.h>
#include <shellapi.h>
#include <shobjidl.h>
#include <shlobj.h>
#include <propsys.h>
#include <stdlib.h>
#include <string.h>

#include "desktop_windows.h"
#include "_cgo_export.h"

// The shell's IDs this needs, as their own constants: no INITGUID, so they
// can't collide with another package's copies.
static const GUID wdIShellItem = {0x43826d1e, 0xe718, 0x42ee, {0xbc, 0x55, 0xa1, 0xe2, 0x61, 0xc3, 0x7b, 0xfe}};
static const GUID wdIEnumShellItems = {0x70629033, 0xe363, 0x4a28, {0xa5, 0x67, 0x0d, 0xb7, 0x80, 0x06, 0xe6, 0xd7}};
static const GUID wdIShellItemImageFactory = {0xbcc18b79, 0xba16, 0x442f, {0x80, 0xc4, 0x8a, 0x59, 0xc3, 0x0c, 0x46, 0x3b}};
static const GUID wdIPropertyStore = {0x886d8eeb, 0x8cf2, 0x4446, {0x8d, 0x02, 0xcd, 0xba, 0x1d, 0xbd, 0xcf, 0x99}};
static const GUID wdAppsFolder = {0x1e87508d, 0x89c2, 0x42f0, {0x8a, 0x7e, 0x64, 0x5a, 0x0f, 0x50, 0xca, 0x58}};
static const GUID wdEnumItems = {0x94f60519, 0x2850, 0x4924, {0xaa, 0x5a, 0xd1, 0x5e, 0x84, 0x86, 0x80, 0x39}};
static const PROPERTYKEY wdAppUserModelID = {{0x9f4c2855, 0x9f79, 0x4b39, {0xa8, 0xd0, 0xe1, 0xd4, 0x2d, 0xe1, 0xd5, 0xf3}}, 5};

// MARK: - Helpers

// A window's ID as the phone has it, and back: handles are 32-bit values,
// sign-extended on 64-bit Windows.
static HWND fromID(uint32_t id) { return (HWND)(intptr_t)(int32_t)id; }
static uint32_t toID(HWND h) { return (uint32_t)(uintptr_t)h; }

static void toUTF8(const wchar_t *w, char *out, int size) {
  out[0] = 0;
  if (!w || !WideCharToMultiByte(CP_UTF8, 0, w, -1, out, size, NULL, NULL)) out[0] = 0;
  out[size - 1] = 0;
}

static char *dupUTF8(const wchar_t *w) {
  int n = WideCharToMultiByte(CP_UTF8, 0, w, -1, NULL, 0, NULL, NULL);
  if (n <= 0) return NULL;
  char *s = malloc(n);
  if (s) WideCharToMultiByte(CP_UTF8, 0, w, -1, s, n, NULL, NULL);
  return s;
}

static wchar_t *dupWide(const char *s) {
  int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
  if (n <= 0) return NULL;
  wchar_t *w = malloc(n * sizeof(wchar_t));
  if (w) MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n);
  return w;
}

// isPath: a program's path (C:\…, \\server\…) rather than an AppUserModelID.
static int isPath(const char *key) {
  return (key[0] && key[1] == ':') || (key[0] == '\\' && key[1] == '\\');
}

// shellPath is what the shell parses for an app: its path, or its entry
// in the Start menu's apps folder. malloc'd.
static wchar_t *shellPath(const char *key) {
  wchar_t *w = dupWide(key);
  if (!w || isPath(key)) return w;
  static const wchar_t prefix[] = L"shell:AppsFolder\\";
  size_t n = wcslen(prefix) + wcslen(w) + 1;
  wchar_t *full = malloc(n * sizeof(wchar_t));
  if (full) {
    wcscpy(full, prefix);
    wcscat(full, w);
  }
  free(w);
  return full;
}

// comBegin starts COM on this thread for the shell; comEnd ends it when
// comBegin did.
static int comBegin(void) { return SUCCEEDED(CoInitializeEx(NULL, COINIT_APARTMENTTHREADED)); }
static void comEnd(int began) {
  if (began) CoUninitialize();
}

static int classIs(HWND h, const wchar_t *name) {
  wchar_t cls[64];
  return GetClassNameW(h, cls, 64) && wcscmp(cls, name) == 0;
}

// The desktop and taskbar: windows, but not apps.
static int isShell(HWND h) {
  return classIs(h, L"Progman") || classIs(h, L"WorkerW") || classIs(h, L"Shell_TrayWnd") ||
         classIs(h, L"Shell_SecondaryTrayWnd");
}

// packageAppID reads a packaged (Store) process's AppUserModelID; it's
// left empty for any other. GetApplicationUserModelId, looked up so a
// Windows without it still loads this.
static void packageAppID(HANDLE process, char *out, int size) {
  typedef LONG(WINAPI * appID)(HANDLE, UINT32 *, PWSTR);
  static appID get;
  static int looked;
  if (!looked) {
    HMODULE kernel = GetModuleHandleW(L"kernel32.dll");
    get = kernel ? (appID)(void *)GetProcAddress(kernel, "GetApplicationUserModelId") : NULL;
    looked = 1;
  }
  wchar_t id[256];
  UINT32 n = 256;
  if (get && get(process, &n, id) == ERROR_SUCCESS) toUTF8(id, out, size);
}

// describe fills w from a window: its title, process, app ID and program.
static void describe(HWND h, wd_window *w) {
  memset(w, 0, sizeof *w);
  w->hwnd = toID(h);
  DWORD pid = 0;
  GetWindowThreadProcessId(h, &pid);
  w->pid = pid;
  LONG_PTR style = GetWindowLongPtrW(h, GWL_STYLE);
  w->minimized = IsIconic(h) != 0;
  w->maximized = IsZoomed(h) != 0;
  w->canMinimize = (style & WS_MINIMIZEBOX) != 0;
  w->canMaximize = (style & WS_MAXIMIZEBOX) != 0;
  wchar_t title[512];
  // Read without a message to the window, so a hung app can't stall this.
  InternalGetWindowText(h, title, 512);
  toUTF8(title, w->title, sizeof w->title);
  IPropertyStore *store = NULL;
  if (SUCCEEDED(SHGetPropertyStoreForWindow(h, &wdIPropertyStore, (void **)&store))) {
    PROPVARIANT v;
    PropVariantInit(&v);
    if (SUCCEEDED(IPropertyStore_GetValue(store, &wdAppUserModelID, &v)) && v.vt == VT_LPWSTR) {
      toUTF8(v.pwszVal, w->app, sizeof w->app);
    }
    PropVariantClear(&v);
    IPropertyStore_Release(store);
  }
  HANDLE p = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid);
  if (p) {
    wchar_t exe[MAX_PATH];
    DWORD n = MAX_PATH;
    if (QueryFullProcessImageNameW(p, 0, exe, &n)) toUTF8(exe, w->exe, sizeof w->exe);
    // A Store app whose window doesn't say: its package's ID.
    if (!w->app[0]) packageAppID(p, w->app, sizeof w->app);
    CloseHandle(p);
  }
}

// MARK: - Windows

void wd_init(void) {
  // Window frames and work areas in physical pixels, as the screen is captured.
  HMODULE user32 = GetModuleHandleW(L"user32.dll");
  typedef BOOL(WINAPI * setAwareness)(HANDLE);
  setAwareness aware = user32 ? (setAwareness)(void *)GetProcAddress(user32, "SetProcessDpiAwarenessContext") : NULL;
  if (aware) aware((HANDLE)-4);  // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
}

typedef struct {
  wd_window *out;
  int max, count;
} collected;

// isAppWindow: a window Alt+Tab shows. Visible, top-level, not a tool
// window, owned only if it asks to be shown, not cloaked (a suspended
// Store app, another virtual desktop), titled, and not the shell's.
static int isAppWindow(HWND h) {
  if (!IsWindowVisible(h) || GetAncestor(h, GA_ROOT) != h || isShell(h)) return 0;
  LONG_PTR ex = GetWindowLongPtrW(h, GWL_EXSTYLE);
  if (ex & WS_EX_TOOLWINDOW) return 0;
  if ((GetWindow(h, GW_OWNER) || (ex & WS_EX_NOACTIVATE)) && !(ex & WS_EX_APPWINDOW)) return 0;
  int cloaked = 0;
  if (SUCCEEDED(DwmGetWindowAttribute(h, DWMWA_CLOAKED, &cloaked, sizeof cloaked)) && cloaked) return 0;
  return GetWindowTextLengthW(h) > 0;
}

static BOOL CALLBACK collect(HWND h, LPARAM param) {
  collected *c = (collected *)param;
  if (c->count >= c->max) return FALSE;
  if (isAppWindow(h)) describe(h, &c->out[c->count++]);
  return TRUE;
}

int wd_windows(wd_window *out, int max) {
  int began = comBegin();
  collected c = {out, max, 0};
  EnumWindows(collect, (LPARAM)&c);
  comEnd(began);
  return c.count;
}

int wd_foreground(wd_window *out) {
  HWND h = GetForegroundWindow();
  if (!h) return 0;
  h = GetAncestor(h, GA_ROOT);
  int began = comBegin();
  describe(h, out);
  comEnd(began);
  // The desktop or taskbar in front: File Explorer, with no window of its own.
  return isAppWindow(h) ? 1 : 2;
}

// wd_locked: the input desktop isn't the user's, as on the lock screen, a
// UAC prompt, or a session with no screen. The host can't open it then.
int wd_locked(void) {
  HDESK d = OpenInputDesktop(0, FALSE, DESKTOP_READOBJECTS);
  if (!d) return 1;
  wchar_t name[64] = {0};
  GetUserObjectInformationW(d, UOI_NAME, name, sizeof name, NULL);
  CloseDesktop(d);
  return _wcsicmp(name, L"Default") != 0;
}

// MARK: - Apps

// programName is a program's FileDescription, from its version info.
static char *programName(const char *key) {
  wchar_t *path = dupWide(key);
  if (!path) return NULL;
  char *name = NULL;
  DWORD ignored = 0, size = GetFileVersionInfoSizeW(path, &ignored);
  void *info = size ? malloc(size) : NULL;
  if (info && GetFileVersionInfoW(path, 0, size, info)) {
    struct {
      WORD language, codePage;
    } *tr = NULL;
    UINT len = 0;
    if (VerQueryValueW(info, L"\\VarFileInfo\\Translation", (void **)&tr, &len) && len >= sizeof *tr) {
      wchar_t sub[64];
      wsprintfW(sub, L"\\StringFileInfo\\%04x%04x\\FileDescription", tr->language, tr->codePage);
      wchar_t *value = NULL;
      if (VerQueryValueW(info, sub, (void **)&value, &len) && len > 1 && value[0]) name = dupUTF8(value);
    }
  }
  free(info);
  free(path);
  return name;
}

char *wd_name(const char *key) {
  if (isPath(key)) return programName(key);
  wchar_t *path = shellPath(key);
  if (!path) return NULL;
  char *name = NULL;
  int began = comBegin();
  IShellItem *item = NULL;
  if (SUCCEEDED(SHCreateItemFromParsingName(path, NULL, &wdIShellItem, (void **)&item))) {
    wchar_t *display = NULL;
    if (SUCCEEDED(IShellItem_GetDisplayName(item, SIGDN_NORMALDISPLAY, &display))) {
      name = dupUTF8(display);
      CoTaskMemFree(display);
    }
    IShellItem_Release(item);
  }
  comEnd(began);
  free(path);
  return name;
}

// A growing UTF-8 buffer.
typedef struct {
  char *s;
  size_t len, cap;
} buffer;

static void put(buffer *b, const char *s) {
  size_t n = strlen(s);
  if (b->len + n + 1 > b->cap) {
    size_t cap = (b->cap ? b->cap * 2 : 4096) + n;
    char *grown = realloc(b->s, cap);
    if (!grown) return;
    b->s = grown, b->cap = cap;
  }
  memcpy(b->s + b->len, s, n + 1);
  b->len += n;
}

// putField adds s with tabs and line breaks made spaces.
static void putField(buffer *b, const wchar_t *w) {
  char *s = dupUTF8(w);
  if (!s) return;
  for (char *p = s; *p; p++)
    if (*p == '\t' || *p == '\n' || *p == '\r') *p = ' ';
  put(b, s);
  free(s);
}

// programPath turns a program the Start menu names by its known folder
// ({1AC14E77-…}\notepad.exe) into its path, as a running window gives it;
// NULL for any other ID. malloc'd.
static wchar_t *programPath(const wchar_t *id) {
  const wchar_t *close = id[0] == L'{' ? wcschr(id, L'}') : NULL;
  if (!close || close[1] != L'\\' || close - id != 37) return NULL;
  wchar_t guid[39];
  wcsncpy(guid, id, 38);
  guid[38] = 0;
  GUID folder;
  wchar_t *base = NULL, *path = NULL;
  if (SUCCEEDED(CLSIDFromString(guid, &folder)) && SUCCEEDED(SHGetKnownFolderPath(&folder, 0, NULL, &base))) {
    size_t n = wcslen(base) + wcslen(close + 1) + 1;
    path = malloc(n * sizeof(wchar_t));
    if (path) {
      wcscpy(path, base);
      wcscat(path, close + 1);
    }
  }
  CoTaskMemFree(base);
  return path;
}

char *wd_apps(void) {
  buffer b = {0};
  put(&b, "");
  int began = comBegin();
  IShellItem *folder = NULL;
  IEnumShellItems *items = NULL;
  if (SUCCEEDED(SHGetKnownFolderItem(&wdAppsFolder, KF_FLAG_DEFAULT, NULL, &wdIShellItem, (void **)&folder)) &&
      SUCCEEDED(IShellItem_BindToHandler(folder, NULL, &wdEnumItems, &wdIEnumShellItems, (void **)&items))) {
    IShellItem *item = NULL;
    while (IEnumShellItems_Next(items, 1, &item, NULL) == S_OK) {
      wchar_t *name = NULL, *id = NULL;
      if (SUCCEEDED(IShellItem_GetDisplayName(item, SIGDN_NORMALDISPLAY, &name)) &&
          SUCCEEDED(IShellItem_GetDisplayName(item, SIGDN_PARENTRELATIVEPARSING, &id))) {
        putField(&b, name);
        put(&b, "\t");
        wchar_t *path = programPath(id);
        putField(&b, path ? path : id);
        free(path);
        put(&b, "\n");
      }
      CoTaskMemFree(name);
      CoTaskMemFree(id);
      IShellItem_Release(item);
    }
  }
  if (items) IEnumShellItems_Release(items);
  if (folder) IShellItem_Release(folder);
  comEnd(began);
  return b.s;
}

int wd_icon(const char *key, int px, uint8_t *out) {
  wchar_t *path = shellPath(key);
  if (!path) return 1;
  int result = 1;
  int began = comBegin();
  IShellItem *item = NULL;
  IShellItemImageFactory *factory = NULL;
  HBITMAP bitmap = NULL;
  SIZE size = {px, px};
  if (SUCCEEDED(SHCreateItemFromParsingName(path, NULL, &wdIShellItem, (void **)&item)) &&
      SUCCEEDED(IShellItem_QueryInterface(item, &wdIShellItemImageFactory, (void **)&factory)) &&
      SUCCEEDED(IShellItemImageFactory_GetImage(factory, size, SIIGBF_ICONONLY, &bitmap))) {
    BITMAP bm;
    GetObjectW(bitmap, sizeof bm, &bm);
    int w = bm.bmWidth, h = bm.bmHeight;
    uint8_t *pixels = w > 0 && h > 0 ? malloc((size_t)w * h * 4) : NULL;
    BITMAPINFO bi = {0};
    bi.bmiHeader.biSize = sizeof bi.bmiHeader;
    bi.bmiHeader.biWidth = w;
    bi.bmiHeader.biHeight = -h;  // top-down
    bi.bmiHeader.biPlanes = 1;
    bi.bmiHeader.biBitCount = 32;
    bi.bmiHeader.biCompression = BI_RGB;
    HDC dc = GetDC(NULL);
    if (pixels && GetDIBits(dc, bitmap, 0, h, pixels, &bi, DIB_RGB_COLORS) == h) {
      // Nearest to px×px, should the shell have drawn another size.
      int opaque = 0;
      for (int y = 0; y < px; y++) {
        for (int x = 0; x < px; x++) {
          memcpy(out + ((size_t)y * px + x) * 4, pixels + ((size_t)(y * h / px) * w + x * w / px) * 4, 4);
        }
      }
      for (size_t i = 3; i < (size_t)px * px * 4; i += 4) opaque |= out[i];
      // An old icon with no alpha at all is opaque, not invisible.
      if (!opaque)
        for (size_t i = 3; i < (size_t)px * px * 4; i += 4) out[i] = 255;
      result = 0;
    }
    ReleaseDC(NULL, dc);
    free(pixels);
  }
  if (bitmap) DeleteObject(bitmap);
  if (factory) IShellItemImageFactory_Release(factory);
  if (item) IShellItem_Release(item);
  comEnd(began);
  free(path);
  return result;
}

int wd_launch(const char *key) {
  wchar_t *path = shellPath(key);
  if (!path) return 1;
  int began = comBegin();
  // Let the app it opens take the front, as a click on the taskbar would.
  AllowSetForegroundWindow(ASFW_ANY);
  HINSTANCE r = ShellExecuteW(NULL, L"open", path, NULL, NULL, SW_SHOWNORMAL);
  comEnd(began);
  free(path);
  return (INT_PTR)r > 32 ? 0 : 1;
}

// MARK: - Moving windows

int wd_button(uint32_t id, int button) {
  HWND h = fromID(id);
  if (!IsWindow(h)) return 1;
  WPARAM command = SC_CLOSE;
  if (button == 1) command = SC_MINIMIZE;
  if (button == 2) command = IsZoomed(h) ? SC_RESTORE : SC_MAXIMIZE;
  return PostMessageW(h, WM_SYSCOMMAND, command, 0) ? 0 : 1;
}

void wd_close(uint32_t id) { PostMessageW(fromID(id), WM_CLOSE, 0, 0); }

// wd_raise brings a window to the front, restored if minimized. Windows
// keeps a background process from taking the front with
// SetForegroundWindow; switching to it as Alt+Tab does is allowed.
void wd_raise(uint32_t id) {
  HWND h = fromID(id);
  if (!IsWindow(h)) return;
  SwitchToThisWindow(h, TRUE);
}

// visibleFrame is a window's frame without the invisible resize borders
// Windows 10 and later draw around it.
static RECT visibleFrame(HWND h) {
  RECT r;
  if (FAILED(DwmGetWindowAttribute(h, DWMWA_EXTENDED_FRAME_BOUNDS, &r, sizeof r))) GetWindowRect(h, &r);
  return r;
}

int wd_frame(uint32_t id, int32_t *frame, int32_t *work) {
  HWND h = fromID(id);
  if (!IsWindow(h)) return 1;
  RECT r = visibleFrame(h);
  MONITORINFO mi = {sizeof mi};
  if (!GetMonitorInfoW(MonitorFromWindow(h, MONITOR_DEFAULTTONEAREST), &mi)) return 1;
  frame[0] = r.left, frame[1] = r.top, frame[2] = r.right - r.left, frame[3] = r.bottom - r.top;
  work[0] = mi.rcWork.left, work[1] = mi.rcWork.top;
  work[2] = mi.rcWork.right - mi.rcWork.left, work[3] = mi.rcWork.bottom - mi.rcWork.top;
  return 0;
}

void wd_place(uint32_t id, int32_t x, int32_t y, int32_t w, int32_t h) {
  HWND win = fromID(id);
  if (!IsWindow(win)) return;
  if (IsZoomed(win) || IsIconic(win)) ShowWindow(win, SW_RESTORE);
  // The borders measured once restored, which changes them.
  RECT outer, seen = visibleFrame(win);
  GetWindowRect(win, &outer);
  int left = seen.left - outer.left, top = seen.top - outer.top;
  int right = outer.right - seen.right, bottom = outer.bottom - seen.bottom;
  SetWindowPos(win, NULL, x - left, y - top, w + left + right, h + top + bottom, SWP_NOZORDER | SWP_NOACTIVATE);
}

// MARK: - Changes

static void CALLBACK onEvent(HWINEVENTHOOK hook, DWORD event, HWND h, LONG object, LONG child, DWORD thread,
                             DWORD time) {
  (void)hook, (void)thread, (void)time;
  if (event != EVENT_SYSTEM_FOREGROUND) {
    if (object != OBJID_WINDOW || child != CHILDID_SELF || !h || GetAncestor(h, GA_ROOT) != h) return;
  }
  goChanged();
}

static DWORD WINAPI watchLoop(LPVOID unused) {
  (void)unused;
  DWORD flags = WINEVENT_OUTOFCONTEXT | WINEVENT_SKIPOWNPROCESS;
  SetWinEventHook(EVENT_SYSTEM_FOREGROUND, EVENT_SYSTEM_FOREGROUND, NULL, onEvent, 0, 0, flags);
  SetWinEventHook(EVENT_SYSTEM_MINIMIZESTART, EVENT_SYSTEM_MINIMIZEEND, NULL, onEvent, 0, 0, flags);
  SetWinEventHook(EVENT_OBJECT_SHOW, EVENT_OBJECT_HIDE, NULL, onEvent, 0, 0, flags);
  SetWinEventHook(EVENT_OBJECT_NAMECHANGE, EVENT_OBJECT_NAMECHANGE, NULL, onEvent, 0, 0, flags);
  MSG m;
  while (GetMessageW(&m, NULL, 0, 0) > 0) {
    TranslateMessage(&m);
    DispatchMessageW(&m);
  }
  return 0;
}

void wd_watch(void) {
  HANDLE t = CreateThread(NULL, 0, watchLoop, NULL, 0, NULL);
  if (t) CloseHandle(t);
}
