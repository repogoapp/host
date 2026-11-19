//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework CoreGraphics -framework ImageIO -framework ApplicationServices
#include <stdlib.h>
#include <dlfcn.h>
#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ImageIO/ImageIO.h>
#import <ApplicationServices/ApplicationServices.h>
// Only for the declarations: AppKit itself is loaded at run time.
#import <AppKit/AppKit.h>

extern void goChanged(void);

// rectOf is an element's frame on screen.
static bool rectOf(AXUIElementRef el, CGRect *rect) {
	CFTypeRef v = NULL;
	if (AXUIElementCopyAttributeValue(el, CFSTR("AXFrame"), &v) != kAXErrorSuccess || !v) return false;
	bool ok = CFGetTypeID(v) == AXValueGetTypeID() && AXValueGetValue((AXValueRef)v, kAXValueTypeCGRect, rect);
	CFRelease(v);
	return ok;
}

// webAnchor is the frame of what sits at a web page's middle: it moves
// when the page scrolls, and stays put at the page's ends.
static bool webAnchor(AXUIElementRef system, AXUIElementRef page, CGRect *anchor) {
	CGRect view;
	if (!rectOf(page, &view) || view.size.height < 1) return false;
	AXUIElementRef el = NULL;
	if (AXUIElementCopyElementAtPosition(system, CGRectGetMidX(view), CGRectGetMidY(view), &el) != kAXErrorSuccess || !el) return false;
	bool ok = rectOf(el, anchor);
	CFRelease(el);
	return ok;
}

// The vertical scroll under the cursor: its nearest scroll area that has a
// vertical bar, read as 1 at the top, 2 at the bottom, 0 between, or -1 when
// nothing there has one. A web page (AXWebArea, as Chrome's) has none: then
// *anchor is the frame of what sits at its middle, for the caller to read
// whether the page moved.
static int scrollEdgeAtCursor(CGRect *anchor) {
	CGEventRef event = CGEventCreate(NULL);
	if (!event) return -1;
	CGPoint at = CGEventGetLocation(event);
	CFRelease(event);
	AXUIElementRef system = AXUIElementCreateSystemWide();
	// An app that hangs mustn't hold up the stream's input.
	AXUIElementSetMessagingTimeout(system, 0.1);
	AXUIElementRef el = NULL;
	AXError err = AXUIElementCopyElementAtPosition(system, at.x, at.y, &el);
	if (err != kAXErrorSuccess || !el) {
		CFRelease(system);
		return -1;
	}
	int edge = -1;
	bool web = false;
	for (int depth = 0; el && depth < 60; depth++) {
		CFTypeRef role = NULL;
		AXUIElementCopyAttributeValue(el, kAXRoleAttribute, &role);
		bool area = role && CFEqual(role, kAXScrollAreaRole);
		if (!web && role && CFEqual(role, CFSTR("AXWebArea"))) web = webAnchor(system, el, anchor);
		if (role) CFRelease(role);
		if (area) {
			CFTypeRef bar = NULL;
			double value = -1;
			if (AXUIElementCopyAttributeValue(el, kAXVerticalScrollBarAttribute, &bar) == kAXErrorSuccess && bar) {
				CFTypeRef v = NULL;
				if (AXUIElementCopyAttributeValue((AXUIElementRef)bar, kAXValueAttribute, &v) == kAXErrorSuccess && v) {
					if (CFGetTypeID(v) == CFNumberGetTypeID()) CFNumberGetValue((CFNumberRef)v, kCFNumberDoubleType, &value);
					CFRelease(v);
				}
				CFRelease(bar);
			}
			// An area without a vertical bar doesn't scroll up and down: the
			// one that does is further out.
			if (value >= 0) {
				edge = value <= 0.001 ? 1 : value >= 0.999 ? 2 : 0;
				break;
			}
		}
		CFTypeRef parent = NULL;
		AXUIElementCopyAttributeValue(el, kAXParentAttribute, &parent);
		CFRelease(el);
		el = (AXUIElementRef)parent;
	}
	if (el) CFRelease(el);
	CFRelease(system);
	return edge;
}

// SkyLight's private calls, the data Mission Control draws.
typedef int CGSConnectionID;
extern CGSConnectionID CGSMainConnectionID(void);
extern CFArrayRef CGSCopyManagedDisplaySpaces(CGSConnectionID cid);
extern CFArrayRef CGSCopySpacesForWindows(CGSConnectionID cid, int mask, CFArrayRef windows);
extern AXError _AXUIElementGetWindow(AXUIElementRef el, CGWindowID *wid);

// AppKit is loaded only once the desktop is read, so the host doesn't link
// it; its classes are looked up by name.
static Class appKit(NSString *name) {
	static dispatch_once_t once;
	dispatch_once(&once, ^{ dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", RTLD_LAZY); });
	return NSClassFromString(name);
}

static NSRunningApplication *runningApp(int pid) {
	return [appKit(@"NSRunningApplication") runningApplicationWithProcessIdentifier:pid];
}

static char *toJSON(id obj) {
	NSData *d = [NSJSONSerialization dataWithJSONObject:obj options:0 error:nil];
	if (!d) return NULL;
	return strdup([[NSString alloc] initWithData:d encoding:NSUTF8StringEncoding].UTF8String);
}

// ── Session ──

static int isLocked(void) {
	CFDictionaryRef session = CGSessionCopyCurrentDictionary();
	if (!session) return 0;
	CFBooleanRef locked = CFDictionaryGetValue(session, CFSTR("CGSSessionScreenIsLocked"));
	int out = locked && CFBooleanGetValue(locked);
	CFRelease(session);
	return out;
}

static int isTrusted(void) { return AXIsProcessTrusted(); }

// ── Spaces and windows ──

static NSString *mainDisplayUUID(void) {
	CFUUIDRef uuid = CGDisplayCreateUUIDFromDisplayID(CGMainDisplayID());
	if (!uuid) return @"";
	NSString *s = CFBridgingRelease(CFUUIDCreateString(NULL, uuid));
	CFRelease(uuid);
	return s;
}

// readDesktop returns {spaces, current, windows} as JSON: the main
// display's spaces in swipe order, and the Dock apps' windows.
static char *readDesktop(void) {
	@autoreleasepool {
		CGSConnectionID cid = CGSMainConnectionID();
		NSArray *list = CFBridgingRelease(CGWindowListCopyWindowInfo(
			kCGWindowListOptionAll | kCGWindowListExcludeDesktopElements, kCGNullWindowID));
		NSMutableDictionary *bundles = [NSMutableDictionary dictionary]; // pid → bundle, or NSNull
		NSMutableDictionary *ownerOf = [NSMutableDictionary dictionary];  // window → bundle
		NSMutableArray *windows = [NSMutableArray array];
		for (NSDictionary *w in list) {
			NSNumber *pid = w[(id)kCGWindowOwnerPID], *wid = w[(id)kCGWindowNumber];
			if (!pid || !wid || [w[(id)kCGWindowLayer] intValue] != 0) continue;
			id bundle = bundles[pid];
			if (!bundle) {
				NSRunningApplication *app = runningApp(pid.intValue);
				NSString *b = app.activationPolicy == NSApplicationActivationPolicyRegular ? app.bundleIdentifier : nil;
				bundle = b ?: (id)[NSNull null];
				bundles[pid] = bundle;
			}
			if (bundle == [NSNull null]) continue;
			ownerOf[wid] = bundle;
			NSDictionary *bounds = w[(id)kCGWindowBounds];
			if ([w[(id)kCGWindowAlpha] doubleValue] <= 0 ||
			    [bounds[@"Width"] doubleValue] < 50 || [bounds[@"Height"] doubleValue] < 50) continue;
			NSArray *spaces = CFBridgingRelease(CGSCopySpacesForWindows(cid, 7, (__bridge CFArrayRef)@[wid]));
			// A window on no space is a hidden helper or a minimized one.
			if (spaces.count == 0 && ![w[(id)kCGWindowIsOnscreen] boolValue]) continue;
			[windows addObject:@{
				@"id": wid, @"bundle": bundle,
				@"app": runningApp(pid.intValue).localizedName ?: w[(id)kCGWindowOwnerName] ?: @"",
				@"title": w[(id)kCGWindowName] ?: @"",
				@"space": spaces.firstObject ?: @0,
			}];
		}

		NSArray *displays = CFBridgingRelease(CGSCopyManagedDisplaySpaces(cid));
		NSString *main = mainDisplayUUID();
		NSDictionary *display = displays.firstObject;
		for (NSDictionary *d in displays) {
			NSString *name = d[@"Display Identifier"];
			if ([name isEqual:@"Main"] || [name caseInsensitiveCompare:main] == NSOrderedSame) { display = d; break; }
		}
		NSNumber *currentID = display[@"Current Space"][@"id64"];
		NSMutableArray *spaces = [NSMutableArray array];
		int current = 0;
		for (NSDictionary *s in display[@"Spaces"]) {
			NSNumber *sid = s[@"id64"] ?: s[@"ManagedSpaceID"];
			if (!sid) continue;
			if ([sid isEqual:currentID]) current = (int)spaces.count;
			NSMutableArray *apps = [NSMutableArray array];
			// Type 4 is a full-screen space: one tile per app side by side.
			if ([s[@"type"] intValue] == 4) {
				for (NSDictionary *t in s[@"TileLayoutManager"][@"TileSpaces"]) {
					NSString *b = ownerOf[t[@"TileWindowID"] ?: @0];
					if (b && ![apps containsObject:b]) [apps addObject:b];
				}
				NSString *b = ownerOf[s[@"fs_wid"] ?: @0];
				if (apps.count == 0 && b) [apps addObject:b];
			}
			[spaces addObject:@{@"id": sid, @"apps": apps}];
		}
		return toJSON(@{@"spaces": spaces, @"current": @(current), @"windows": windows});
	}
}

// The whole list: "including window" alone leaves out a window that
// isn't on screen (minimized, or on another space).
static int windowPID(unsigned int wid) {
	@autoreleasepool {
		NSArray *list = CFBridgingRelease(CGWindowListCopyWindowInfo(kCGWindowListOptionAll, kCGNullWindowID));
		for (NSDictionary *w in list) {
			if ([w[(id)kCGWindowNumber] unsignedIntValue] == wid) return [w[(id)kCGWindowOwnerPID] intValue];
		}
		return 0;
	}
}

// ── Apps ──

// iconPNG draws the app's icon, as the Dock does, into a px-wide PNG.
static void *iconPNG(const char *bundle, int px, int *length) {
	@autoreleasepool {
		*length = 0;
		NSWorkspace *workspace = [appKit(@"NSWorkspace") sharedWorkspace];
		NSURL *url = [workspace URLForApplicationWithBundleIdentifier:[NSString stringWithUTF8String:bundle]];
		if (!url) return NULL;
		NSImage *image = [workspace iconForFile:url.path];
		NSRect rect = NSMakeRect(0, 0, px, px);
		CGImageRef cg = [image CGImageForProposedRect:&rect context:nil hints:nil];
		if (!cg) return NULL;
		CGColorSpaceRef rgb = CGColorSpaceCreateDeviceRGB();
		CGContextRef ctx = CGBitmapContextCreate(NULL, px, px, 8, 0, rgb, (CGBitmapInfo)kCGImageAlphaPremultipliedLast);
		CGColorSpaceRelease(rgb);
		if (!ctx) return NULL;
		CGContextSetInterpolationQuality(ctx, kCGInterpolationHigh);
		CGContextDrawImage(ctx, CGRectMake(0, 0, px, px), cg);
		CGImageRef scaled = CGBitmapContextCreateImage(ctx);
		CGContextRelease(ctx);
		NSMutableData *data = [NSMutableData data];
		CGImageDestinationRef dest = CGImageDestinationCreateWithData((__bridge CFMutableDataRef)data, CFSTR("public.png"), 1, NULL);
		CGImageDestinationAddImage(dest, scaled, NULL);
		BOOL ok = CGImageDestinationFinalize(dest);
		CFRelease(dest);
		CGImageRelease(scaled);
		if (!ok || data.length == 0) return NULL;
		void *out = malloc(data.length);
		memcpy(out, data.bytes, data.length);
		*length = (int)data.length;
		return out;
	}
}

// bundleAt returns {bundle, name} JSON for the app at path, or NULL.
static char *bundleAt(const char *path) {
	@autoreleasepool {
		NSString *p = [NSString stringWithUTF8String:path];
		NSString *bundle = [NSBundle bundleWithPath:p].bundleIdentifier;
		if (!bundle) return NULL;
		NSString *name = [[NSFileManager defaultManager] displayNameAtPath:p];
		if ([name hasSuffix:@".app"]) name = [name substringToIndex:name.length - 4];
		return toJSON(@{@"bundle": bundle, @"name": name});
	}
}

static void quitApp(const char *bundle) {
	@autoreleasepool {
		for (NSRunningApplication *app in [appKit(@"NSRunningApplication")
			     runningApplicationsWithBundleIdentifier:[NSString stringWithUTF8String:bundle]]) {
			[app terminate];
		}
	}
}

// ── The focused window, by Accessibility ──

static int flag(AXUIElementRef el, CFStringRef attr) {
	CFTypeRef v = NULL;
	if (AXUIElementCopyAttributeValue(el, attr, &v) != kAXErrorSuccess || !v) return -1;
	int out = CFGetTypeID(v) == CFBooleanGetTypeID() ? CFBooleanGetValue(v) : -1;
	CFRelease(v);
	return out;
}

static NSString *text(AXUIElementRef el, CFStringRef attr) {
	CFTypeRef v = NULL;
	if (AXUIElementCopyAttributeValue(el, attr, &v) != kAXErrorSuccess || !v) return @"";
	if (CFGetTypeID(v) != CFStringGetTypeID()) { CFRelease(v); return @""; }
	return CFBridgingRelease(v);
}

static CFStringRef buttonAttribute(NSString *name) {
	if ([name isEqual:@"close"]) return kAXCloseButtonAttribute;
	if ([name isEqual:@"minimize"]) return kAXMinimizeButtonAttribute;
	return NULL;
}

// axWindow finds pid's window wid, or its focused window for 0. Caller releases.
static AXUIElementRef axWindow(int pid, unsigned wid) {
	AXUIElementRef app = AXUIElementCreateApplication(pid);
	AXUIElementRef out = NULL;
	if (wid == 0) {
		CFTypeRef w = NULL;
		if (AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute, &w) == kAXErrorSuccess) out = (AXUIElementRef)w;
	} else {
		CFArrayRef wins = NULL;
		if (AXUIElementCopyAttributeValue(app, kAXWindowsAttribute, (CFTypeRef *)&wins) == kAXErrorSuccess) {
			for (CFIndex i = 0; i < CFArrayGetCount(wins); i++) {
				AXUIElementRef w = CFArrayGetValueAtIndex(wins, i);
				CGWindowID id = 0;
				if (_AXUIElementGetWindow(w, &id) == kAXErrorSuccess && id == wid) { out = CFRetain(w); break; }
			}
			CFRelease(wins);
		}
	}
	CFRelease(app);
	return out;
}

// focusJSON describes pid's focused window, or NULL when it has none.
static char *focusJSON(int pid) {
	@autoreleasepool {
		AXUIElementRef w = axWindow(pid, 0);
		if (!w) return NULL;
		CGWindowID id = 0;
		_AXUIElementGetWindow(w, &id);
		BOOL full = flag(w, CFSTR("AXFullScreen")) == 1;
		NSMutableArray *buttons = [NSMutableArray array];
		// In full screen the title bar is hidden and its buttons say they're
		// off; full screen still works, through the window's own setting.
		for (NSString *name in @[@"close", @"minimize"]) {
			CFTypeRef b = NULL;
			if (AXUIElementCopyAttributeValue(w, buttonAttribute(name), &b) == kAXErrorSuccess && b) {
				if (flag(b, kAXEnabledAttribute) == 1) [buttons addObject:name];
				CFRelease(b);
			}
		}
		if (full || flag(w, CFSTR("AXFullScreen")) == 0) [buttons addObject:@"full_screen"];
		NSDictionary *d = @{
			@"window": @(id), @"title": text(w, kAXTitleAttribute),
			@"full_screen": @(full), @"buttons": buttons,
		};
		CFRelease(w);
		return toJSON(d);
	}
}

// setFullScreen sets the window's full screen and waits, up to 5 s, for
// the slide to finish: a change asked for mid-slide is dropped.
static int setFullScreen(AXUIElementRef w, int on) {
	if (AXUIElementSetAttributeValue(w, CFSTR("AXFullScreen"), on ? kCFBooleanTrue : kCFBooleanFalse) != kAXErrorSuccess) return 0;
	for (int i = 0; i < 50; i++) {
		usleep(100000);
		if (flag(w, CFSTR("AXFullScreen")) == on) {
			usleep(800000); // the slide ends a beat after the setting flips
			return 1;
		}
	}
	return 0;
}

// pressButton presses a title-bar button on pid's focused window: 0 done,
// 1 no window, 2 no such button, 3 the press failed.
static int pressButton(int pid, const char *name) {
	@autoreleasepool {
		AXUIElementRef w = axWindow(pid, 0);
		if (!w) return 1;
		NSString *button = [NSString stringWithUTF8String:name];
		int full = flag(w, CFSTR("AXFullScreen")) == 1;
		int rc = 0;
		if ([button isEqual:@"full_screen"]) {
			rc = setFullScreen(w, !full) ? 0 : 3;
		} else if ([button isEqual:@"close"] && full && !setFullScreen(w, 0)) {
			rc = 3;
		} else if ([button isEqual:@"minimize"] || [button isEqual:@"close"]) {
			CFTypeRef b = NULL;
			rc = 2;
			if (AXUIElementCopyAttributeValue(w, buttonAttribute(button), &b) == kAXErrorSuccess && b) {
				rc = AXUIElementPerformAction(b, kAXPressAction) == kAXErrorSuccess ? 0 : 3;
				CFRelease(b);
			}
		} else {
			rc = 2;
		}
		CFRelease(w);
		return rc;
	}
}

// menuItem finds the menu item with identifier under el, up to depth
// menus down, preferring one that's on: an app can carry a hidden copy of
// an item that's off. Caller releases.
static AXUIElementRef menuItem(AXUIElementRef el, NSString *identifier, int depth) {
	CFArrayRef kids = NULL;
	if (depth < 0 || AXUIElementCopyAttributeValue(el, kAXChildrenAttribute, (CFTypeRef *)&kids) != kAXErrorSuccess) return NULL;
	AXUIElementRef off = NULL;
	AXUIElementRef on = NULL;
	for (CFIndex i = 0; i < CFArrayGetCount(kids) && !on; i++) {
		AXUIElementRef kid = CFArrayGetValueAtIndex(kids, i);
		AXUIElementRef found = [text(kid, CFSTR("AXIdentifier")) isEqual:identifier]
			? (AXUIElementRef)CFRetain(kid) : menuItem(kid, identifier, depth - 1);
		if (!found) continue;
		if (flag(found, kAXEnabledAttribute) == 1) {
			on = found;
		} else if (!off) {
			off = found;
		} else {
			CFRelease(found);
		}
	}
	CFRelease(kids);
	if (on) {
		if (off) CFRelease(off);
		return on;
	}
	return off;
}

// pressMenuItem presses pid's menu bar item with identifier (a Window
// menu tile, _zoomLeft:): 0 done, 1 no such item, 2 it's off now, 3 the
// press failed. The identifiers are AppKit's selectors, the same in every
// language. Menu bar, top menu, its menu, item, submenu, item: depth 5.
static int pressMenuItem(int pid, const char *identifier) {
	@autoreleasepool {
		AXUIElementRef app = AXUIElementCreateApplication(pid);
		CFTypeRef bar = NULL;
		AXUIElementRef item = NULL;
		if (AXUIElementCopyAttributeValue(app, kAXMenuBarAttribute, &bar) == kAXErrorSuccess && bar) {
			item = menuItem((AXUIElementRef)bar, [NSString stringWithUTF8String:identifier], 5);
			CFRelease(bar);
		}
		CFRelease(app);
		if (!item) return 1;
		int rc = flag(item, kAXEnabledAttribute) != 1 ? 2
			: AXUIElementPerformAction(item, kAXPressAction) == kAXErrorSuccess ? 0 : 3;
		CFRelease(item);
		return rc;
	}
}

// ── The Dock's space swipe ──

// Undocumented event fields, from reverse-engineered headers, tried on
// macOS 26.6.
enum {
	kEventTypeField = 55,       // the event's type, settable
	kEventTypeGesture = 29,     // NSEventTypeGesture
	kEventTypeDockControl = 30, // the Dock's gesture events
	kGestureHIDType = 110,
	kHIDDockSwipe = 23,         // kIOHIDEventTypeDockSwipe
	kSwipeMotion = 123,         // 1 horizontal
	kSwipeProgress = 124,
	kSwipeVelocityX = 129,
	kSwipeVelocityY = 130,
	kGesturePhase = 132,        // 1 began, 2 changed, 4 ended, 8 cancelled
	kScrollFlagBits = 135,
	kZoomDeltaX = 139,
};

// postSwipe posts one step of a three-finger space swipe, as a trackpad
// does: the Dock's control event, then a gesture event. The Dock moves
// the space by progress, live. Neither the flag bits nor the smallest
// float in the zoom field can be left out: without either, nothing moves.
static void postSwipe(int phase, double progress, double velocity) {
	CGEventRef dock = CGEventCreate(NULL);
	CGEventSetIntegerValueField(dock, kEventTypeField, kEventTypeDockControl);
	CGEventSetIntegerValueField(dock, kGestureHIDType, kHIDDockSwipe);
	CGEventSetIntegerValueField(dock, kGesturePhase, phase);
	CGEventSetIntegerValueField(dock, kSwipeMotion, 1);
	CGEventSetIntegerValueField(dock, kScrollFlagBits, 1);
	CGEventSetDoubleValueField(dock, kSwipeProgress, progress);
	CGEventSetDoubleValueField(dock, kZoomDeltaX, 1.401298464e-45);
	CGEventSetDoubleValueField(dock, kSwipeVelocityX, velocity);
	CGEventSetDoubleValueField(dock, kSwipeVelocityY, 0);
	CGEventRef gesture = CGEventCreate(NULL);
	CGEventSetIntegerValueField(gesture, kEventTypeField, kEventTypeGesture);
	CGEventPost(kCGSessionEventTap, dock);
	CGEventPost(kCGSessionEventTap, gesture);
	CFRelease(dock);
	CFRelease(gesture);
}

// raiseWindow raises one window through Accessibility, when the host is
// trusted with it; windows on another space are hidden from it.
static void raiseWindow(int pid, unsigned wid) {
	if (!AXIsProcessTrusted()) return;
	AXUIElementRef w = axWindow(pid, wid);
	if (!w) return;
	AXUIElementPerformAction(w, kAXRaiseAction);
	AXUIElementSetAttributeValue(w, kAXMainAttribute, kCFBooleanTrue);
	CFRelease(w);
}

// ── Watching for changes ──

static CFRunLoopRef watchLoop;
static AXObserverRef observer;
static pid_t observed;

static void axChanged(AXObserverRef o, AXUIElementRef el, CFStringRef note, void *ctx) { goChanged(); }

// observe moves the Accessibility observer to pid's app. Registered on
// the app's element, it hears all its windows.
static void observe(pid_t pid) {
	if (pid == observed || pid <= 0 || !AXIsProcessTrusted()) return;
	if (observer) {
		CFRunLoopRemoveSource(watchLoop, AXObserverGetRunLoopSource(observer), kCFRunLoopDefaultMode);
		CFRelease(observer);
		observer = NULL;
	}
	observed = pid;
	if (AXObserverCreate(pid, axChanged, &observer) != kAXErrorSuccess) return;
	AXUIElementRef app = AXUIElementCreateApplication(pid);
	CFStringRef notes[] = {
		kAXFocusedWindowChangedNotification, kAXTitleChangedNotification, kAXWindowMiniaturizedNotification,
		kAXWindowDeminiaturizedNotification, kAXWindowResizedNotification, kAXWindowCreatedNotification,
		kAXUIElementDestroyedNotification,
	};
	for (size_t i = 0; i < sizeof notes / sizeof *notes; i++) AXObserverAddNotification(observer, app, notes[i], NULL);
	CFRelease(app);
	CFRunLoopAddSource(watchLoop, AXObserverGetRunLoopSource(observer), kCFRunLoopDefaultMode);
}

// The notices are named by their values, which are their names: the
// constants are AppKit symbols, and the host doesn't link AppKit.
//
// runWatch runs the calling thread's run loop forever. NSWorkspace's
// notices reach it though it isn't the main thread; each app that comes to
// the front gets the window observer.
static void runWatch(void) {
	watchLoop = CFRunLoopGetCurrent();
	NSNotificationCenter *center = [[appKit(@"NSWorkspace") sharedWorkspace] notificationCenter];
	for (NSString *name in @[@"NSWorkspaceDidLaunchApplicationNotification", @"NSWorkspaceDidTerminateApplicationNotification",
	                         @"NSWorkspaceActiveSpaceDidChangeNotification"]) {
		[center addObserverForName:name object:nil queue:nil usingBlock:^(NSNotification *n) { goChanged(); }];
	}
	[center addObserverForName:@"NSWorkspaceDidActivateApplicationNotification" object:nil queue:nil
	                usingBlock:^(NSNotification *n) {
		pid_t pid = [n.userInfo[@"NSWorkspaceApplicationKey"] processIdentifier];
		CFRunLoopPerformBlock(watchLoop, kCFRunLoopDefaultMode, ^{ observe(pid); });
		CFRunLoopWakeUp(watchLoop);
		goChanged();
	}];
	observe([[appKit(@"NSWorkspace") sharedWorkspace] frontmostApplication].processIdentifier);
	// A source that never fires keeps the loop running with nothing observed.
	CFRunLoopSourceContext ctx = {0};
	CFRunLoopAddSource(watchLoop, CFRunLoopSourceCreate(NULL, 0, &ctx), kCFRunLoopDefaultMode);
	CFRunLoopRun();
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"runtime"
	"unsafe"
)

func locked() bool  { return C.isLocked() != 0 }
func trusted() bool { return C.isTrusted() != 0 }

func scrollEdge() (int, [4]float64, bool) {
	page := C.CGRect{size: C.CGSize{width: -1}}
	edge := int(C.scrollEdgeAtCursor(&page))
	anchor := [4]float64{float64(page.origin.x), float64(page.origin.y), float64(page.size.width), float64(page.size.height)}
	return edge, anchor, page.size.width >= 0
}

func read() ([]Space, int, []rawWindow) {
	var raw struct {
		Spaces  []Space     `json:"spaces"`
		Current int         `json:"current"`
		Windows []rawWindow `json:"windows"`
	}
	if !fromJSON(C.readDesktop(), &raw) {
		return nil, 0, nil
	}
	return raw.Spaces, raw.Current, raw.Windows
}

func focusOf(pid int) *Focus {
	var f Focus
	if !fromJSON(C.focusJSON(C.int(pid)), &f) {
		return nil
	}
	return &f
}

func press(pid int, button string) error {
	name := C.CString(button)
	defer C.free(unsafe.Pointer(name))
	switch C.pressButton(C.int(pid), name) {
	case 0:
		return nil
	case 1:
		return errors.New("That window has closed.")
	case 2:
		return errors.New("That window has no " + button + " button.")
	}
	return errors.New("The Mac didn't take the press.")
}

func pressMenu(pid int, identifier string) error {
	c := C.CString(identifier)
	defer C.free(unsafe.Pointer(c))
	switch C.pressMenuItem(C.int(pid), c) {
	case 0:
		return nil
	case 1:
		return errors.New("This app has no Move & Resize in its Window menu.")
	case 2:
		return errors.New("The window can't move there now.")
	}
	return errors.New("The Mac didn't take the press.")
}

func swipe(phase int, progress, velocity float64) {
	C.postSwipe(C.int(phase), C.double(progress), C.double(velocity))
}

func icon(bundle string, px int) []byte {
	c := C.CString(bundle)
	defer C.free(unsafe.Pointer(c))
	var n C.int
	p := C.iconPNG(c, C.int(px), &n)
	if p == nil {
		return nil
	}
	defer C.free(p)
	return C.GoBytes(p, n)
}

func bundleAt(path string) (App, bool) {
	c := C.CString(path)
	defer C.free(unsafe.Pointer(c))
	var a App
	return a, fromJSON(C.bundleAt(c), &a)
}

func quit(bundle string) {
	c := C.CString(bundle)
	defer C.free(unsafe.Pointer(c))
	C.quitApp(c)
}

func raise(pid int, window uint32) { C.raiseWindow(C.int(pid), C.uint(window)) }

func pidOf(window uint32) int { return int(C.windowPID(C.uint(window))) }

// watch runs the change watcher's run loop on a thread of its own: the
// host's main thread is Go's.
func watch() {
	go func() {
		runtime.LockOSThread()
		C.runWatch()
	}()
}

//export goChanged
func goChanged() { changed() }

// fromJSON decodes and frees a JSON string the C side made; false for NULL.
func fromJSON(p *C.char, v any) bool {
	if p == nil {
		return false
	}
	defer C.free(unsafe.Pointer(p))
	return json.Unmarshal([]byte(C.GoString(p)), v) == nil
}
