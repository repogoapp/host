// What's on a simulator's screen, by label: its accessibility tree through
// Xcode's private AccessibilityPlatformTranslation, the way idb reads it
// (MIT): each request the translator makes goes to the device through
// -[SimDevice sendAccessibilityRequestAsync:completionQueue:completionHandler:].
// Runs in the simcap child, which already holds the device, on its own
// serial queue, never the encoder's.

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#import <dlfcn.h>
#import <objc/message.h>
#import <objc/runtime.h>

#include "simax_darwin.h"
#include "simcap_darwin.h"

static id gAXDevice;
static NSString *gAXDeveloperDir;
static NSString *gToken;
static id gTranslator;
static dispatch_queue_t gAXQueue;
// gSwitchedOnAt is when accessibility was last switched on: each app
// launched after starts with it off again.
static CFAbsoluteTime gSwitchedOnAt;
// gDeadline ends the read under way, whatever is left: the tree, the probe,
// switching accessibility on.
static CFAbsoluteTime gDeadline;
static const CFAbsoluteTime kReadBudget = 6;

static BOOL late(void) { return CFAbsoluteTimeGetCurrent() > gDeadline; }

// waitFor waits on a semaphore no longer than the read has left, nor limit.
static BOOL waitFor(dispatch_semaphore_t s, double limit) {
  double left = MIN(limit, gDeadline - CFAbsoluteTimeGetCurrent());
  if (left <= 0) return NO;
  return dispatch_semaphore_wait(s, dispatch_time(DISPATCH_TIME_NOW, (int64_t)(left * NSEC_PER_SEC))) == 0;
}

// The translator's bridge: it asks, the device answers.
@interface RGAXBridge : NSObject
@end

@implementation RGAXBridge
- (id)accessibilityTranslationDelegateBridgeCallbackWithToken:(NSString *)token {
  return ^id(id request) {
    __block id response = nil;
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    ((void (*)(id, SEL, id, dispatch_queue_t, id))objc_msgSend)(
        gAXDevice, NSSelectorFromString(@"sendAccessibilityRequestAsync:completionQueue:completionHandler:"), request,
        dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^(id r) {
          response = r;
          dispatch_semaphore_signal(done);
        });
    waitFor(done, 3);
    if (response) return response;
    Class empty = NSClassFromString(@"AXPTranslatorResponse");
    return ((id (*)(id, SEL))objc_msgSend)(empty, NSSelectorFromString(@"emptyResponse"));
  };
}
- (CGRect)accessibilityTranslationConvertPlatformFrameToSystem:(CGRect)rect withToken:(NSString *)token {
  return rect;
}
- (id)accessibilityTranslationRootParentWithToken:(NSString *)token {
  return nil;
}
@end

static RGAXBridge *gBridge;

// attribute reads one by name: the old API, which these elements still
// answer for what has no property (a description, a placeholder).
static id attribute(NSAccessibilityElement *e, NSString *name) {
  SEL read = NSSelectorFromString(@"accessibilityAttributeValue:");
  if (![e respondsToSelector:read]) return nil;
  return ((id (*)(id, SEL, NSString *))objc_msgSend)(e, read, name);
}

int simax_start(void *device, const char *developer_dir) {
  gAXDevice = (__bridge id)device;
  gAXDeveloperDir = @(developer_dir);
  if (![gAXDevice respondsToSelector:NSSelectorFromString(
                                         @"sendAccessibilityRequestAsync:completionQueue:completionHandler:")])
    return -1;
  if (!dlopen("/System/Library/PrivateFrameworks/AccessibilityPlatformTranslation.framework/"
              "AccessibilityPlatformTranslation",
              RTLD_NOW))
    return -2;
  Class translatorClass = NSClassFromString(@"AXPTranslator");
  if (!translatorClass) return -3;
  gTranslator = ((id (*)(id, SEL))objc_msgSend)(translatorClass, NSSelectorFromString(@"sharedInstance"));
  gBridge = [RGAXBridge new];
  [gTranslator setValue:gBridge forKey:@"bridgeTokenDelegate"];
  [gTranslator setValue:@YES forKey:@"supportsDelegateTokens"];
  [gTranslator setValue:@YES forKey:@"accessibilityEnabled"];
  gToken = NSUUID.UUID.UUIDString;
  gAXQueue = dispatch_queue_create("app.repogo.simax", DISPATCH_QUEUE_SERIAL);
  return 0;
}

// switchOn turns the simulator's app accessibility on, which a freshly
// launched app leaves off (its tree is an empty root): a short XCTest
// automation session asks it to load. It stays on after the session ends.
static BOOL switchOn(void) {
  NSString *shared = [[gAXDeveloperDir stringByAppendingPathComponent:@"../SharedFrameworks"] stringByStandardizingPath];
  for (NSString *name in @[ @"XCTDaemonControl", @"XCUIAutomation" ]) {
    NSString *path = [NSString stringWithFormat:@"%@/%@.framework/%@", shared, name, name];
    if (!dlopen(path.fileSystemRepresentation, RTLD_NOW)) return NO;
  }
  Class providerClass = NSClassFromString(@"XCUIDeviceRemoteDaemonConnectionProvider");
  Class sessionClass = NSClassFromString(@"XCUIDeviceRemoteAutomationSession");
  SEL providerFor = NSSelectorFromString(@"connectionProviderForSimDevice:");
  SEL request = NSSelectorFromString(@"requestSessionWithDaemonConnectionProvider:completion:");
  if (![providerClass respondsToSelector:providerFor] || ![sessionClass respondsToSelector:request]) return NO;
  id provider = ((id (*)(id, SEL, id))objc_msgSend)(providerClass, providerFor, gAXDevice);
  if (!provider) return NO;
  __block id session = nil;
  dispatch_semaphore_t got = dispatch_semaphore_create(0);
  ((void (*)(id, SEL, id, id))objc_msgSend)(sessionClass, request, provider, ^(id s, NSError *e) {
    session = s;
    dispatch_semaphore_signal(got);
  });
  if (!waitFor(got, 3) || !session) return NO;
  SEL enable = NSSelectorFromString(@"enableAutomationModeWithError:");
  if ([session respondsToSelector:enable]) {
    NSError *error = nil;
    if (!((BOOL (*)(id, SEL, NSError **))objc_msgSend)(session, enable, &error)) return NO;
  }
  SEL load = NSSelectorFromString(@"loadAccessibilityWithTimeout:reply:");
  if (![session respondsToSelector:load]) return NO;
  __block BOOL loaded = NO;
  dispatch_semaphore_t done = dispatch_semaphore_create(0);
  ((void (*)(id, SEL, double, id))objc_msgSend)(session, load, 3.0, ^(BOOL ok, NSError *e) {
    loaded = ok;
    dispatch_semaphore_signal(done);
  });
  waitFor(done, 3.5);
  SEL invalidate = NSSelectorFromString(@"invalidate");
  if ([session respondsToSelector:invalidate]) ((void (*)(id, SEL))objc_msgSend)(session, invalidate);
  return loaded;
}

static NSAccessibilityElement *element(id translation) {
  if (!translation) return nil;
  [translation setValue:gToken forKey:@"bridgeDelegateToken"];
  NSAccessibilityElement *e = ((id (*)(id, SEL, id))objc_msgSend)(
      gTranslator, NSSelectorFromString(@"macPlatformElementFromTranslation:"), translation);
  [[e valueForKey:@"translation"] setValue:gToken forKey:@"bridgeDelegateToken"];
  return e;
}

static NSAccessibilityElement *frontmost(void) {
  id translation = ((id (*)(id, SEL, unsigned, id))objc_msgSend)(
      gTranslator, NSSelectorFromString(@"frontmostApplicationWithDisplayId:bridgeDelegateToken:"), 0, gToken);
  return element(translation);
}

static NSAccessibilityElement *at(CGPoint point, uint32_t display) {
  id translation = ((id (*)(id, SEL, CGPoint, unsigned, id))objc_msgSend)(
      gTranslator, NSSelectorFromString(@"objectAtPoint:displayId:bridgeDelegateToken:"), point, display, gToken);
  return element(translation);
}

static NSString *labelOf(NSAccessibilityElement *e) {
  if (e.accessibilityLabel.length) return e.accessibilityLabel;
  if (e.accessibilityTitle.length) return e.accessibilityTitle;
  id description = attribute(e, NSAccessibilityDescriptionAttribute);
  return [description isKindOfClass:NSString.class] ? description : @"";
}

static BOOL isField(NSString *role) {
  return [role isEqual:@"AXTextField"] || [role isEqual:@"AXSecureTextField"] || [role isEqual:@"AXSearchField"] ||
         [role isEqual:@"AXTextArea"];
}

// isControl is whether a role is one a touch acts on, which can be disabled.
static BOOL isControl(NSString *role) {
  return isField(role) || [role isEqual:@"AXButton"] || [role isEqual:@"AXCheckBox"] ||
         [role isEqual:@"AXSwitch"] || [role isEqual:@"AXRadioButton"] || [role isEqual:@"AXSlider"] ||
         [role isEqual:@"AXPopUpButton"] || [role isEqual:@"AXMenuButton"] || [role isEqual:@"AXLink"];
}

// The read in progress: what was found, where, and which grid cells it covers.
typedef struct {
  NSMutableArray<NSDictionary *> *found;
  NSMutableSet<NSString *> *seen;
  CGSize size;
} Read;

// add records e when it says something, and answers its frame.
static void add(Read *r, NSAccessibilityElement *e, NSString *source) {
  NSString *role = e.accessibilityRole ?: @"";
  NSString *label = labelOf(e);
  NSString *ident = e.accessibilityIdentifier ?: @"";
  NSRect f = e.accessibilityFrame;
  if ([role isEqual:@"AXApplication"] || f.size.width < 1 || f.size.height < 1) return;
  if (!label.length && !ident.length && !isField(role)) return;
  NSString *key = [NSString stringWithFormat:@"%@|%@|%.0f,%.0f,%.0f,%.0f", role, label, f.origin.x, f.origin.y,
                                             f.size.width, f.size.height];
  if ([r->seen containsObject:key]) return;
  [r->seen addObject:key];
  id value = e.accessibilityValue;
  NSString *valueText = value ? [value description] : @"";
  if (isField(role) && !valueText.length) {
    id placeholder = attribute(e, NSAccessibilityPlaceholderValueAttribute);
    if ([placeholder isKindOfClass:NSString.class]) valueText = placeholder;
  }
  NSMutableDictionary *found = [@{
    @"role" : [role hasPrefix:@"AX"] ? [role substringFromIndex:2] : role,
    @"label" : label,
    @"value" : valueText,
    @"id" : ident,
    @"x" : @(round(f.origin.x)),
    @"y" : @(round(f.origin.y)),
    @"w" : @(round(f.size.width)),
    @"h" : @(round(f.size.height)),
    @"source" : source,
  } mutableCopy];
  // Each attribute is a round trip to the device: only controls are asked.
  if (isControl(role)) {
    id enabled = attribute(e, NSAccessibilityEnabledAttribute);
    if ([enabled isKindOfClass:NSNumber.class] && ![enabled boolValue]) found[@"disabled"] = @YES;
  }
  if (isField(role)) {
    id focused = attribute(e, NSAccessibilityFocusedAttribute);
    if ([focused isKindOfClass:NSNumber.class] && [focused boolValue]) found[@"focused"] = @YES;
  }
  // A password field is its own role, or a text field's subrole.
  if ([role isEqual:@"AXSecureTextField"] ||
      (isField(role) && [e.accessibilitySubrole isEqual:@"AXSecureTextField"]))
    found[@"secure"] = @YES;
  [r->found addObject:found];
}

// walk adds e and what's under it. Each leaf's frame goes in covering, for
// the probe to skip, unless it spans most of the screen (an overlay that
// stands for nothing); each empty group's goes in hollow: SwiftUI's tab bars
// come back as one group with no children, their tabs reachable only by point.
static void walk(Read *r, NSAccessibilityElement *e, int depth, NSMutableArray<NSValue *> *covering,
                 NSMutableArray<NSValue *> *hollow) {
  if (!e || depth > 64 || r->found.count > 2000 || late()) return;
  add(r, e, @"tree");
  NSArray *children = e.accessibilityChildren;
  if (!children.count) children = attribute(e, @"AXChildrenInNavigationOrder");
  NSRect f = e.accessibilityFrame;
  if (!children.count && depth > 0) {
    if ([e.accessibilityRole isEqual:@"AXGroup"]) {
      [hollow addObject:[NSValue valueWithRect:f]];
    } else if (f.size.width * f.size.height < 0.4 * r->size.width * r->size.height) {
      [covering addObject:[NSValue valueWithRect:f]];
    }
  }
  for (NSAccessibilityElement *child in children) walk(r, child, depth + 1, covering, hollow);
}

// probeAt reads one point, adding what's there; it answers the frame the
// point is now known to be part of.
static NSRect probeAt(Read *r, CGPoint p, uint32_t display, CGFloat cell) {
  NSAccessibilityElement *hit = at(p, display);
  NSRect f = hit.accessibilityFrame;
  NSUInteger before = r->found.count;
  if (hit) add(r, hit, @"point");
  BOOL found = r->found.count > before && f.size.width < r->size.width && f.size.height < r->size.height;
  return found ? f : NSMakeRect(p.x - cell / 2, p.y - cell / 2, cell, cell);
}

static BOOL inAny(CGPoint p, NSArray<NSValue *> *rects) {
  for (NSValue *v in rects) {
    if (NSPointInRect(p, v.rectValue)) return YES;
  }
  return NO;
}

// probe reads, point by point, what the tree left out: first inside each
// empty group, then the cells of a grid nothing covered.
static void probe(Read *r, NSMutableArray<NSValue *> *covering, NSArray<NSValue *> *hollow, uint32_t display) {
  const CGFloat cell = 40;
  CFAbsoluteTime deadline = MIN(CFAbsoluteTimeGetCurrent() + 0.8, gDeadline);
  NSMutableArray<NSValue *> *known = [NSMutableArray array];
  for (NSValue *v in hollow) {
    NSRect g = NSIntersectionRect(v.rectValue, NSMakeRect(0, 0, r->size.width, r->size.height));
    for (CGFloat y = NSMinY(g) + MIN(cell, g.size.height) / 2; y < NSMaxY(g); y += cell) {
      for (CGFloat x = NSMinX(g) + MIN(cell, g.size.width) / 2; x < NSMaxX(g); x += cell) {
        CGPoint p = CGPointMake(x, y);
        if (inAny(p, known)) continue;
        if (CFAbsoluteTimeGetCurrent() > deadline) return;
        [known addObject:[NSValue valueWithRect:probeAt(r, p, display, cell)]];
      }
    }
  }
  [covering addObjectsFromArray:known];
  for (CGFloat y = cell / 2; y < r->size.height; y += cell) {
    for (CGFloat x = cell / 2; x < r->size.width; x += cell) {
      CGPoint p = CGPointMake(x, y);
      if (inAny(p, covering)) continue;
      if (CFAbsoluteTimeGetCurrent() > deadline) return;
      [covering addObject:[NSValue valueWithRect:probeAt(r, p, display, cell)]];
    }
  }
}

static NSDictionary *readScreen(uint32_t display, int sideways, BOOL *empty) {
  NSAccessibilityElement *root = frontmost();
  NSRect frame = root.accessibilityFrame;
  NSArray *children = root.accessibilityChildren;
  *empty = !root || (frame.size.width < 1 && !children.count);
  if (*empty) return nil;
  // The screen is the active panel, as the device type sizes it: an app's
  // root can say another's size (SpringBoard on the open Duo says the
  // cover's), and keeps its upright size while its elements lie as the
  // screen is turned. Only without one does the root decide.
  double panelWidth = 0, panelHeight = 0;
  if (simcap_panel_size(display, &panelWidth, &panelHeight) == 0) frame.size = CGSizeMake(panelWidth, panelHeight);
  CGFloat shortSide = MIN(frame.size.width, frame.size.height), longSide = MAX(frame.size.width, frame.size.height);
  frame.size = sideways ? CGSizeMake(longSide, shortSide) : CGSizeMake(shortSide, longSide);
  Read r = {.found = [NSMutableArray array], .seen = [NSMutableSet set], .size = frame.size};
  NSMutableArray<NSValue *> *covering = [NSMutableArray array], *hollow = [NSMutableArray array];
  walk(&r, root, 0, covering, hollow);
  probe(&r, covering, hollow, display);
  id pid = [[root valueForKey:@"translation"] valueForKey:@"pid"];
  return @{
    @"pid" : [pid isKindOfClass:NSNumber.class] ? pid : @0,
    @"app" : labelOf(root),
    @"width" : @(round(frame.size.width)),
    @"height" : @(round(frame.size.height)),
    @"elements" : r.found,
  };
}

char *simax_elements(uint32_t display, int sideways) {
  // Without the frameworks (simax_start failed) there's nothing to read with.
  if (!gAXQueue || !gTranslator) return NULL;
  __block char *answer = NULL;
  dispatch_sync(gAXQueue, ^{
    @autoreleasepool {
      gDeadline = CFAbsoluteTimeGetCurrent() + kReadBudget;
      BOOL empty = NO;
      NSDictionary *screen = readScreen(display, sideways, &empty);
      // An empty tree: an app launched since accessibility was switched on,
      // or one still launching. Switch it on (not more than every few
      // seconds), then give the app a moment.
      for (int attempt = 0; empty && attempt < 3 && !late(); attempt++) {
        if (CFAbsoluteTimeGetCurrent() - gSwitchedOnAt > 3) {
          gSwitchedOnAt = CFAbsoluteTimeGetCurrent();
          switchOn();
        } else {
          usleep(300 * 1000);
        }
        screen = readScreen(display, sideways, &empty);
      }
      if (!screen) screen = @{@"error" : @"Nothing on the screen to read: is an app open?"};
      NSData *json = [NSJSONSerialization dataWithJSONObject:screen options:0 error:nil];
      if (!json) return;
      answer = malloc(json.length + 1);
      memcpy(answer, json.bytes, json.length);
      answer[json.length] = 0;
    }
  });
  return answer;
}
