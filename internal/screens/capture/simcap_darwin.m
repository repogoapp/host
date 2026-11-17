// Live view of an iOS simulator through Xcode's private SimulatorKit, the way
// Simulator.app and idb read it: the device's framebuffer IOSurface as it
// renders (no window, no Screen Recording permission), encoded to H.264 with
// VideoToolbox, and touches sent back as native HID events.
// Adapted from serve-sim (Apache-2.0): https://github.com/expo/serve-sim

#import <Accelerate/Accelerate.h>
#import <Carbon/Carbon.h>
#import <CoreGraphics/CoreGraphics.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Foundation/Foundation.h>
#import <IOSurface/IOSurfaceRef.h>
#import <VideoToolbox/VideoToolbox.h>
#import <dlfcn.h>
#import <mach/mach.h>
#import <os/lock.h>
#import <stdatomic.h>
#import <objc/message.h>
#import <objc/runtime.h>
#import <unistd.h>

#include "simcap_darwin.h"
#include "uhid_darwin.h"

// The stream's shape: the old host's tuning for remote control. A short side
// over 720 px costs encode and decode time for little gain in legibility.
// A Mac screen (simcap_screen_start) keeps more: its text is smaller.
static int gMaxShortSide = 720;
static const int64_t kMinFrameGapNs = 1000000000LL / 30;
static int gBitRate = 4000000;

static dispatch_queue_t gQueue;        // every capture and encoder field below
static int gOut = -1;
static NSObject *gIO;
static NSArray<NSObject *> *gDescriptors;
static NSMutableDictionary<NSValue *, NSUUID *> *gCallbacks;
static uint32_t gLastSeed;
static BOOL gHaveSeed;
static int64_t gLastEncodeNs;
static BOOL gTrailingPending;
static VTCompressionSessionRef gSession;
static VTPixelTransferSessionRef gTransfer;
static size_t gSourceW, gSourceH, gTargetW, gTargetH;
// Each framebuffer descriptor's screen ID, by its index in gDescriptors.
static NSArray<NSNumber *> *gScreenIDs;
// The panel CoreDevice reports active: captured, and where touches go. 0
// until it reports (or on a runtime without display info): the largest.
static _Atomic uint32_t gActiveScreen;
// Writes to stdout come from the encoder and the display poll.
static os_unfair_lock gWriteLock = OS_UNFAIR_LOCK_INIT;
// gForceKeyframe makes the next frame a keyframe; gLastPixels is the last
// frame of pixels (a Mac screen's, an emulator's), to encode again for one.
static BOOL gForceKeyframe;
static CVPixelBufferRef gLastPixels;
static void capture(void);

static int64_t nowNs(void) { return (int64_t)clock_gettime_nsec_np(CLOCK_UPTIME_RAW); }

static int fail(char *err, int len, int code, NSString *why) {
  snprintf(err, len, "%s", why.UTF8String);
  return code;
}

static NSObject *findDevice(NSString *developerDir, NSString *udid) {
  Class contextClass = NSClassFromString(@"SimServiceContext");
  if (!contextClass) return nil;
  NSError *error = nil;
  id context = ((id (*)(id, SEL, id, NSError **))objc_msgSend)(
      contextClass, NSSelectorFromString(@"sharedServiceContextForDeveloperDir:error:"), developerDir, &error);
  if (!context) return nil;
  id set = ((id (*)(id, SEL, NSError **))objc_msgSend)(
      context, NSSelectorFromString(@"defaultDeviceSetWithError:"), &error);
  for (NSObject *device in [set valueForKey:@"devices"]) {
    if ([[(NSUUID *)[device valueForKey:@"UDID"] UUIDString] isEqualToString:udid]) return device;
  }
  return nil;
}

// MARK: - Encoding

static void writeAll(NSData *data) {
  os_unfair_lock_lock(&gWriteLock);
  const uint8_t *bytes = data.bytes;
  size_t left = data.length;
  while (left > 0) {
    ssize_t n = write(gOut, bytes, left);
    if (n <= 0) _exit(0);  // the host closed the stream
    bytes += n;
    left -= (size_t)n;
  }
  os_unfair_lock_unlock(&gWriteLock);
}

// A message for the host rather than a frame: JSON, its length's top bit set.
static void tellHost(NSDictionary *message) {
  NSData *json = [NSJSONSerialization dataWithJSONObject:message options:0 error:nil];
  if (!json) return;
  NSMutableData *out = [NSMutableData data];
  uint32_t length = CFSwapInt32HostToBig((uint32_t)json.length | 0x80000000u);
  [out appendBytes:&length length:4];
  [out appendData:json];
  writeAll(out);
}

// Writes one encoded frame as a message: its length (4 bytes, big-endian),
// its capture time (8 bytes, big-endian microseconds on the uptime clock),
// then its NAL units each behind a 4-byte length, parameter sets first on a
// keyframe. The host passes the message on as it is, so a frame never waits
// for the next to show it ended.
static void appendLength(NSMutableData *out, uint32_t length) {
  uint32_t big = CFSwapInt32HostToBig(length);
  [out appendBytes:&big length:4];
}

static void encoded(void *ref, void *frameRef, OSStatus status, VTEncodeInfoFlags flags, CMSampleBufferRef sample) {
  if (status != noErr || !sample || !CMSampleBufferDataIsReady(sample)) return;
  NSMutableData *out = [NSMutableData dataWithLength:12];
  CMTime pts = CMSampleBufferGetPresentationTimeStamp(sample);
  uint64_t capturedUS = CMTIME_IS_VALID(pts) ? (uint64_t)(CMTimeGetSeconds(pts) * 1000000.0) : 0;
  uint64_t bigTime = CFSwapInt64HostToBig(capturedUS);
  memcpy((uint8_t *)out.mutableBytes + 4, &bigTime, 8);
  CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sample, false);
  BOOL keyframe = YES;
  if (attachments && CFArrayGetCount(attachments) > 0) {
    CFDictionaryRef first = CFArrayGetValueAtIndex(attachments, 0);
    keyframe = !CFDictionaryContainsKey(first, kCMSampleAttachmentKey_NotSync);
  }
  if (keyframe) {
    CMFormatDescriptionRef format = CMSampleBufferGetFormatDescription(sample);
    size_t count = 0;
    CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, 0, NULL, NULL, &count, NULL);
    for (size_t i = 0; i < count; i++) {
      const uint8_t *set = NULL;
      size_t size = 0;
      if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, i, &set, &size, NULL, NULL) == noErr) {
        appendLength(out, (uint32_t)size);
        [out appendBytes:set length:size];
      }
    }
  }
  // VideoToolbox's output is already NAL units behind 4-byte lengths.
  CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
  size_t total = CMBlockBufferGetDataLength(block);
  NSUInteger at = out.length;
  [out increaseLengthBy:total];
  if (CMBlockBufferCopyDataBytes(block, 0, total, (uint8_t *)out.mutableBytes + at) != noErr) return;
  uint32_t length = CFSwapInt32HostToBig((uint32_t)(out.length - 4));
  memcpy(out.mutableBytes, &length, 4);
  writeAll(out);
}

static void endSession(void) {
  if (gSession) {
    VTCompressionSessionInvalidate(gSession);
    CFRelease(gSession);
    gSession = NULL;
  }
}

// An encoder for the source's size, scaled so its short side is at most 720 px.
static BOOL ensureSession(size_t width, size_t height) {
  if (gSession && width == gSourceW && height == gSourceH) return YES;
  endSession();
  double scale = MIN(1.0, (double)gMaxShortSide / (double)MIN(width, height));
  gTargetW = ((size_t)(width * scale)) & ~1;
  gTargetH = ((size_t)(height * scale)) & ~1;
  gSourceW = width;
  gSourceH = height;
  NSDictionary *spec = @{(__bridge NSString *)kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder: @YES};
  if (VTCompressionSessionCreate(NULL, (int32_t)gTargetW, (int32_t)gTargetH, kCMVideoCodecType_H264,
                                 (__bridge CFDictionaryRef)spec, NULL, NULL, encoded, NULL, &gSession) != noErr) {
    gSession = NULL;
    return NO;
  }
  VTSessionSetProperty(gSession, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
  // Reordering would hold each frame for the next: latency a live screen doesn't need.
  VTSessionSetProperty(gSession, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
  VTSessionSetProperty(gSession, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_Main_AutoLevel);
  VTSessionSetProperty(gSession, kVTCompressionPropertyKey_AverageBitRate, (__bridge CFNumberRef)@(gBitRate));
  VTSessionSetProperty(gSession, kVTCompressionPropertyKey_ExpectedFrameRate, (__bridge CFNumberRef)@30);
  VTSessionSetProperty(gSession, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, (__bridge CFNumberRef)@10);
  VTCompressionSessionPrepareToEncodeFrames(gSession);
  return YES;
}

// Encodes one BGRA frame, scaled to the session's size; consumes source.
static void encodePixelBuffer(CVPixelBufferRef source) {
  size_t width = CVPixelBufferGetWidth(source), height = CVPixelBufferGetHeight(source);
  if (width == 0 || height == 0 || !ensureSession(width, height)) {
    CVPixelBufferRelease(source);
    return;
  }
  CVPixelBufferRef frame = source;
  if (gTargetW != width || gTargetH != height) {
    CVPixelBufferRef scaled = NULL;
    CVPixelBufferPoolRef pool = VTCompressionSessionGetPixelBufferPool(gSession);
    if (!pool || CVPixelBufferPoolCreatePixelBuffer(NULL, pool, &scaled) != kCVReturnSuccess) {
      CVPixelBufferRelease(source);
      return;
    }
    if (!gTransfer) VTPixelTransferSessionCreate(NULL, &gTransfer);
    VTPixelTransferSessionTransferImage(gTransfer, source, scaled);
    CVPixelBufferRelease(source);
    frame = scaled;
  }
  NSDictionary *options = nil;
  if (gForceKeyframe) {
    options = @{(__bridge NSString *)kVTEncodeFrameOptionKey_ForceKeyFrame: @YES};
    gForceKeyframe = NO;
  }
  VTCompressionSessionEncodeFrame(gSession, frame, CMTimeMake(nowNs(), 1000000000), kCMTimeInvalid,
                                  (__bridge CFDictionaryRef)options, NULL, NULL);
  CVPixelBufferRelease(frame);
}

static void encodeSurface(IOSurfaceRef surface) {
  CVPixelBufferRef source = NULL;
  NSDictionary *attrs = @{(__bridge NSString *)kCVPixelBufferPixelFormatTypeKey: @(kCVPixelFormatType_32BGRA)};
  if (CVPixelBufferCreateWithIOSurface(NULL, surface, (__bridge CFDictionaryRef)attrs, &source) != kCVReturnSuccess) return;
  encodePixelBuffer(source);
}

// MARK: - Mac screen

// Starts the encoder alone, for frames that come as pixels (a Mac screen
// over VNC, an Android emulator's display) rather than from a simulator's
// framebuffer.
void simcap_screen_start(int out, int maxShortSide, int bitRate) {
  gOut = out;
  gMaxShortSide = maxShortSide;
  gBitRate = bitRate;
  gQueue = dispatch_queue_create("app.repogo.screencap", dispatch_queue_attr_make_with_qos_class(
                                                            DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0));
}

// Encodes one frame of pixels, stride bytes a row: BGRA, or RGBA when rgba
// (an emulator's), swapped to BGRA as they're copied.
void simcap_encode_pixels(const void *pixels, int width, int height, int stride, int rgba) {
  CVPixelBufferRef buffer = NULL;
  NSDictionary *attrs = @{(__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey: @{}};
  if (CVPixelBufferCreate(NULL, width, height, kCVPixelFormatType_32BGRA, (__bridge CFDictionaryRef)attrs, &buffer) !=
      kCVReturnSuccess)
    return;
  CVPixelBufferLockBaseAddress(buffer, 0);
  uint8_t *to = CVPixelBufferGetBaseAddress(buffer);
  size_t toStride = CVPixelBufferGetBytesPerRow(buffer);
  if (rgba) {
    vImage_Buffer from = {(void *)pixels, (vImagePixelCount)height, (vImagePixelCount)width, (size_t)stride};
    vImage_Buffer into = {to, (vImagePixelCount)height, (vImagePixelCount)width, toStride};
    const uint8_t toBGRA[4] = {2, 1, 0, 3};
    vImagePermuteChannels_ARGB8888(&from, &into, toBGRA, kvImageNoFlags);
  } else {
    for (int row = 0; row < height; row++) memcpy(to + row * toStride, (const uint8_t *)pixels + row * stride, width * 4);
  }
  CVPixelBufferUnlockBaseAddress(buffer, 0);
  dispatch_sync(gQueue, ^{
    if (gLastPixels) CVPixelBufferRelease(gLastPixels);
    gLastPixels = CVPixelBufferRetain(buffer);
    encodePixelBuffer(buffer);
  });
}

void simcap_keyframe(void) {
  if (!gQueue) return;
  dispatch_async(gQueue, ^{
    gForceKeyframe = YES;
    if (gDescriptors.count > 0) {
      // A simulator: its surface, encoded again though its seed hasn't moved.
      gHaveSeed = NO;
      capture();
    } else if (gLastPixels) {
      encodePixelBuffer(CVPixelBufferRetain(gLastPixels));
    }
  });
}

// MARK: - Capture

// The screen to show: the active panel's surface when CoreDevice has said
// which (a foldable keeps both alive), else the largest, the main screen.
static IOSurfaceRef currentSurface(void) {
  IOSurfaceRef best = NULL, active = NULL;
  size_t bestArea = 0;
  uint32_t activeScreen = atomic_load(&gActiveScreen);
  for (NSUInteger i = 0; i < gDescriptors.count; i++) {
    id surface = ((id (*)(id, SEL))objc_msgSend)(gDescriptors[i], NSSelectorFromString(@"framebufferSurface"));
    if (!surface) continue;
    IOSurfaceRef ref = (__bridge IOSurfaceRef)surface;
    size_t area = IOSurfaceGetWidth(ref) * IOSurfaceGetHeight(ref);
    if (area == 0) continue;
    if (activeScreen && gScreenIDs[i].unsignedIntValue == activeScreen) active = ref;
    if (area > bestArea) {
      best = ref;
      bestArea = area;
    }
  }
  return active ?: best;
}

// A descriptor's screen ID from its screenProperties; 0 when it has none.
static uint32_t screenID(NSObject *descriptor) {
  SEL props = NSSelectorFromString(@"screenProperties"), idSel = NSSelectorFromString(@"screenID");
  if (![descriptor respondsToSelector:props]) return 0;
  NSObject *properties = ((id (*)(id, SEL))objc_msgSend)(descriptor, props);
  if (![properties respondsToSelector:idSel]) return 0;
  return ((uint32_t (*)(id, SEL))objc_msgSend)(properties, idSel);
}

// Encodes the screen when it changed, at most 30 times a second; a change
// inside the gap is encoded when the gap ends, so the last frame of an
// animation is never lost.
static void capture(void) {
  IOSurfaceRef surface = currentSurface();
  if (!surface) return;
  uint32_t seed = IOSurfaceGetSeed(surface);
  if (gHaveSeed && seed == gLastSeed) return;
  int64_t wait = gLastEncodeNs + kMinFrameGapNs - nowNs();
  if (wait > 0) {
    if (!gTrailingPending) {
      gTrailingPending = YES;
      dispatch_after(dispatch_time(DISPATCH_TIME_NOW, wait), gQueue, ^{
        gTrailingPending = NO;
        capture();
      });
    }
    return;
  }
  gLastSeed = seed;
  gHaveSeed = YES;
  gLastEncodeNs = nowNs();
  encodeSurface(surface);
}

static int wireFramebuffer(char *err, int len) {
  ((void (*)(id, SEL))objc_msgSend)(gIO, NSSelectorFromString(@"updateIOPorts"));
  NSMutableArray *found = [NSMutableArray array];
  SEL pidSel = NSSelectorFromString(@"portIdentifier"), descSel = NSSelectorFromString(@"descriptor");
  for (NSObject *port in [gIO valueForKey:@"deviceIOPorts"]) {
    if (![port respondsToSelector:pidSel] || ![port respondsToSelector:descSel]) continue;
    id pid = ((id (*)(id, SEL))objc_msgSend)(port, pidSel);
    if (![[pid description] isEqualToString:@"com.apple.framebuffer.display"]) continue;
    NSObject *desc = ((id (*)(id, SEL))objc_msgSend)(port, descSel);
    if ([desc respondsToSelector:NSSelectorFromString(@"framebufferSurface")]) [found addObject:desc];
  }
  if (found.count == 0) return fail(err, len, -5, @"no framebuffer display on the simulator");
  gDescriptors = found;
  NSMutableArray *ids = [NSMutableArray array];
  for (NSObject *desc in found) [ids addObject:@(screenID(desc))];
  gScreenIDs = ids;
  gCallbacks = [NSMutableDictionary dictionary];
  SEL reg = NSSelectorFromString(
      @"registerScreenCallbacksWithUUID:callbackQueue:frameCallback:surfacesChangedCallback:propertiesChangedCallback:");
  for (NSObject *desc in found) {
    if (![desc respondsToSelector:reg]) return fail(err, len, -6, @"this Xcode's SimulatorKit has no screen callbacks");
    NSUUID *uuid = [NSUUID UUID];
    gCallbacks[[NSValue valueWithNonretainedObject:desc]] = uuid;
    void (^changed)(void) = ^{ dispatch_async(gQueue, ^{ capture(); }); };
    void (^ignored)(void) = ^{};
    ((void (*)(id, SEL, id, id, id, id, id))objc_msgSend)(desc, reg, uuid, gQueue, changed, changed, ignored);
  }
  return 0;
}

// MARK: - Input

typedef void *(*IndigoMouseFn)(CGPoint *, CGPoint *, uint32_t, int32_t, CGSize, uint32_t);
typedef void *(*IndigoButtonFn)(int32_t, int32_t, int32_t);
typedef void *(*IndigoHIDFn)(uint32_t, uint32_t, uint32_t, uint32_t);
typedef void *(*IndigoKeyFn)(uint32_t, uint32_t);
static IndigoMouseFn gMouse;
static IndigoButtonFn gButton;
static IndigoHIDFn gArbitrary;
static IndigoKeyFn gKey;
static NSObject *gHID;
static NSObject *gDevice;
static uint32_t gEdge;
// Universal HID capabilities (iOS 27); NULL when touches and keys go through
// Indigo. gVendor carries Device Hub's controls: hinge, physical orientation.
static void *gDigitizer, *gKeyboard, *gVendor;
static const char *gUDID;
// Each integrated panel's mounting rotation in degrees, by screen ID, from
// the device type; two panels make a foldable.
static NSDictionary<NSNumber *, NSNumber *> *gNativeRotations;
static NSUInteger gIntegratedPanels;

// Touch input is best-effort: a SimulatorKit without it still streams, and
// says why on stderr.
static void setUpHID(NSObject *device) {
  gMouse = (IndigoMouseFn)dlsym(RTLD_DEFAULT, "IndigoHIDMessageForMouseNSEvent");
  gButton = (IndigoButtonFn)dlsym(RTLD_DEFAULT, "IndigoHIDMessageForButton");
  gArbitrary = (IndigoHIDFn)dlsym(RTLD_DEFAULT, "IndigoHIDMessageForHIDArbitrary");
  gKey = (IndigoKeyFn)dlsym(RTLD_DEFAULT, "IndigoHIDMessageForKeyboardArbitrary");
  Class hidClass = NSClassFromString(@"_TtC12SimulatorKit24SimDeviceLegacyHIDClient");
  if (!gMouse || !hidClass) {
    fprintf(stderr, "simcap: no touch input (IndigoHIDMessageForMouseNSEvent %s, SimDeviceLegacyHIDClient %s)\n",
            gMouse ? "found" : "missing", hidClass ? "found" : "missing");
    return;
  }
  NSError *error = nil;
  gHID = ((id (*)(id, SEL, id, NSError **))objc_msgSend)(
      [hidClass alloc], NSSelectorFromString(@"initWithDevice:error:"), device, &error);
  if (!gHID) fprintf(stderr, "simcap: no touch input: %s\n", error.localizedDescription.UTF8String ?: "no HID client");
}

static void sendHID(void *message) {
  if (!message || !gHID) return;
  ((void (*)(id, SEL, void *, BOOL, id, id))objc_msgSend)(
      gHID, NSSelectorFromString(@"sendWithMessage:freeWhenDone:completionQueue:completion:"), message, YES, nil, nil);
}

void simcap_touch(int phase, double x, double y) {
  // A touch that starts at the very bottom is the home-indicator swipe.
  if (phase == 0) gEdge = y > 0.97 ? 3 : 0;
  if (gDigitizer) {
    struct SSCoreDeviceTouch contact = {MIN(MAX(x, 0), 1), MIN(MAX(y, 0), 1)};
    // A panel's touch service is 0x100 plus its screen ID; the main screen's is 1.
    uint32_t screen = atomic_load(&gActiveScreen);
    SSCoreDeviceSendTouches(gDigitizer, 0x100 + (screen ? screen : 1), &contact, 1, phase != 2, gEdge);
    return;
  }
  if (!gMouse) return;
  CGPoint point = CGPointMake(MIN(MAX(x, 0), 1), MIN(MAX(y, 0), 1));
  // 1 is a press or a move, 2 a release; a size of 1×1 makes the point a fraction.
  sendHID(gMouse(&point, NULL, 0x32, phase == 2 ? 2 : 1, CGSizeMake(1, 1), gEdge));
}

void simcap_button(int source, int hold_ms) {
  if (!gButton) return;
  sendHID(gButton(source, 1, 0x33));
  if (hold_ms > 0) usleep((useconds_t)hold_ms * 1000);
  sendHID(gButton(source, 2, 0x33));
}

// A hardware button by its HID page and usage, as DeviceKit's chrome.json
// names them (volume, power), sent to the digitizer target.
void simcap_hid(int page, int usage) {
  if (!gArbitrary) return;
  sendHID(gArbitrary(0x32, (uint32_t)page, (uint32_t)usage, 1));
  usleep(50 * 1000);
  sendHID(gArbitrary(0x32, (uint32_t)page, (uint32_t)usage, 2));
}

// One keyboard key by its HID usage (page 7), pressed or released.
void simcap_key(int usage, int down) {
  if (gKeyboard) {
    SSCoreDeviceSendKey(gKeyboard, (uint32_t)usage, down != 0);
    return;
  }
  if (!gKey) return;
  sendHID(gKey((uint32_t)usage, down ? 1 : 2));
}

// Device Hub's physical orientation for a requested UIDeviceOrientation on a
// panel mounted at nativeRotation; nil for none. From serve-sim's
// SimulatorScreenOrientation.vendorControlValue.
static const char *physicalOrientation(int orientation, int nativeRotation) {
  int requested;
  switch (orientation) {
    case 1: requested = 0; break;
    case 2: requested = 180; break;
    case 3: requested = 270; break;
    case 4: requested = 90; break;
    default: return NULL;
  }
  switch (((requested + nativeRotation) % 360 + 360) % 360) {
    case 0: return "portrait";
    case 90: return "landscape-right";
    case 180: return "pud";
    case 270: return "landscape-left";
  }
  return NULL;
}

static BOOL sendControl(void *data) { return data && simcap_uhid_control(gVendor, data); }

// Turns the device. Through Device Hub's orientation control when CoreDevice
// carries it, which needs nothing on the Mac; else as Simulator.app's Rotate
// does, a GraphicsServices orientation event on the device's
// PurpleWorkspacePort, which exists only while Simulator.app runs.
// orientation is UIDeviceOrientation (1 portrait, 2 upside down, 3 landscape
// left, 4 landscape right); 0 on success.
int simcap_rotate(int orientation) {
  if (gVendor) {
    int native = gNativeRotations[@(atomic_load(&gActiveScreen))].intValue;
    const char *value = physicalOrientation(orientation, native);
    if (value && sendControl(SSCoreDeviceOrientationData(value))) return 0;
  }
  if (!gDevice) return -1;
  NSError *error = nil;
  mach_port_t port = ((mach_port_t (*)(id, SEL, NSString *, NSError **))objc_msgSend)(
      gDevice, NSSelectorFromString(@"lookup:error:"), @"PurpleWorkspacePort", &error);
  if (port == MACH_PORT_NULL) return -2;
  uint8_t buffer[112] = {0};
  mach_msg_header_t *header = (mach_msg_header_t *)buffer;
  header->msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, 0);
  header->msgh_size = 108;
  header->msgh_remote_port = port;
  header->msgh_local_port = MACH_PORT_NULL;
  header->msgh_id = 0x7B;
  uint32_t type = 50 | 0x20000;  // device orientation changed, from the host
  uint32_t size = 4, value = (uint32_t)orientation;
  memcpy(buffer + 0x18, &type, 4);
  memcpy(buffer + 0x48, &size, 4);
  memcpy(buffer + 0x4C, &value, 4);
  return mach_msg_send(header) == KERN_SUCCESS ? 0 : -3;
}

// Shows or hides the on-screen keyboard by disconnecting or connecting the
// device's hardware keyboard (Simulator's ⌘⇧K): iOS shows its own keyboard
// for a focused field only while none is connected. 0 on success.
int simcap_onscreen_keyboard(int on) {
  SEL sel = NSSelectorFromString(@"setHardwareKeyboardEnabled:keyboardType:error:");
  if (![gDevice respondsToSelector:sel]) return -1;
  NSError *error = nil;
  // A reconnected keyboard takes the Mac's own type; type 0 left an iOS 18
  // simulator ignoring Indigo key presses.
  BOOL ok = ((BOOL (*)(id, SEL, BOOL, NSInteger, NSError **))objc_msgSend)(gDevice, sel, on == 0, LMGetKbdType(), &error);
  return ok && !error ? 0 : -2;
}

void simcap_memory_warning(void) {
  SEL sel = NSSelectorFromString(@"simulateMemoryWarning");
  if ([gDevice respondsToSelector:sel]) ((void (*)(id, SEL))objc_msgSend)(gDevice, sel);
}

// A Core Animation debug option, as Simulator.app's Debug menu sets it
// (debug_slow_animations, debug_color_blended).
void simcap_ca_debug(const char *name, int enabled) {
  SEL sel = NSSelectorFromString(@"setCADebugOption:enabled:");
  if (![gDevice respondsToSelector:sel]) return;
  ((BOOL (*)(id, SEL, NSString *, BOOL))objc_msgSend)(gDevice, sel, @(name), enabled != 0);
}

// A foldable's pose: its hinge angle, then how it is held, as Device Hub sets
// them. Tent needs the table sensor and isn't offered. 0 on success.
int simcap_fold(const char *pose) {
  if (!gVendor || gIntegratedPanels != 2) return -1;
  double angle;
  const char *held;
  if (strcmp(pose, "closed") == 0) angle = 0, held = "portrait";
  else if (strcmp(pose, "open") == 0) angle = 180, held = "portrait";
  else if (strcmp(pose, "book") == 0) angle = 90, held = "portrait";
  else if (strcmp(pose, "laptop") == 0) angle = 90, held = "landscape-left";
  else return -2;
  if (!sendControl(SSCoreDeviceHingeData(angle))) return -3;
  return sendControl(SSCoreDeviceOrientationData(held)) ? 0 : -4;
}

void simcap_tell(const char *json, int len) {
  NSMutableData *out = [NSMutableData data];
  uint32_t length = CFSwapInt32HostToBig((uint32_t)len | 0x80000000u);
  [out appendBytes:&length length:4];
  [out appendBytes:json length:(NSUInteger)len];
  writeAll(out);
}

// MARK: - Displays

// Reads the panels' mounting rotations from the device type's displays.
static void readPanels(NSObject *device) {
  NSMutableDictionary *rotations = [NSMutableDictionary dictionary];
  NSUInteger integrated = 0;
  SEL typeSel = NSSelectorFromString(@"deviceType"), capsSel = NSSelectorFromString(@"capabilities");
  if ([device respondsToSelector:typeSel]) {
    NSObject *type = ((id (*)(id, SEL))objc_msgSend)(device, typeSel);
    NSDictionary *profile = [type respondsToSelector:capsSel] ? ((id (*)(id, SEL))objc_msgSend)(type, capsSel) : nil;
    NSArray *displays = [profile isKindOfClass:NSDictionary.class] ? profile[@"capabilities"][@"displays"] : nil;
    for (NSDictionary *display in [displays isKindOfClass:NSArray.class] ? displays : @[]) {
      if (![display isKindOfClass:NSDictionary.class] || ![display[@"displayType"] isEqual:@"integrated"]) continue;
      NSNumber *screen = display[@"screenID"], *rotation = display[@"nativeRotation"];
      if (![screen isKindOfClass:NSNumber.class]) continue;
      integrated++;
      rotations[screen] = [rotation isKindOfClass:NSNumber.class] ? rotation : @0;
    }
  }
  gNativeRotations = rotations;
  gIntegratedPanels = integrated;
}

// Follows the active panel and its turn, telling the host when either changes.
static void watchDisplays(void) {
  dispatch_queue_t poll = dispatch_queue_create("app.repogo.simcap.displays", DISPATCH_QUEUE_SERIAL);
  dispatch_source_t timer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, poll);
  dispatch_source_set_timer(timer, DISPATCH_TIME_NOW, 300 * NSEC_PER_MSEC, 50 * NSEC_PER_MSEC);
  __block int64_t lastScreen = -1, lastRotation = -1;
  dispatch_source_set_event_handler(timer, ^{
    uint32_t screen = 0;
    int32_t rotation = 0;
    if (!simcap_uhid_display(gUDID, &screen, &rotation)) return;
    if (screen == lastScreen && rotation == lastRotation) return;
    lastScreen = screen;
    lastRotation = rotation;
    atomic_store(&gActiveScreen, screen);
    // The new panel's surface may not have changed since its last frame.
    dispatch_async(gQueue, ^{
      gHaveSeed = NO;
      capture();
    });
    tellHost(@{@"display": @{@"screen": @(screen), @"rotation": @(rotation), @"foldable": gIntegratedPanels == 2 ? @YES : @NO}});
  });
  dispatch_resume(timer);
  // Lives as long as simcap does.
  static dispatch_source_t kept;
  kept = timer;
}

// MARK: - Lifecycle

// Finds the device's Universal HID digitizer and keyboard, saying on stderr
// when this build or this Xcode can't.
static void setUpUniversalHID(const char *udid) {
  if (SSCoreDeviceDigitizerAvailable()) {
    gDigitizer = simcap_uhid_capability(udid, "$s10CoreDevice29UniversalHIDServiceCapabilityVN",
                                        "$s10CoreDevice29UniversalHIDServiceCapabilityVAA0bE0AAWP");
  }
  if (SSCoreDeviceKeyboardAvailable()) {
    gKeyboard = simcap_uhid_capability(udid, "$s10CoreDevice21KeyboardHIDCapabilityVN",
                                       "$s10CoreDevice21KeyboardHIDCapabilityVAA0B10CapabilityAAWP");
  }
  if (!gDigitizer) fprintf(stderr, "simcap: no touch input: Universal HID has no digitizer for this device\n");
  gVendor = simcap_uhid_capability(udid, "$s10CoreDevice26VendorDefinedHIDCapabilityVN",
                                   "$s10CoreDevice26VendorDefinedHIDCapabilityVAA0B10CapabilityAAWP");
  if (SSCoreDeviceDisplayAvailable()) watchDisplays();
}

int simcap_start(const char *developer_dir, const char *udid, int universal, int out, char *err, int err_len) {
  @autoreleasepool {
    NSString *developerDir = @(developer_dir);
    if (!dlopen("/Library/Developer/PrivateFrameworks/CoreSimulator.framework/CoreSimulator", RTLD_NOW))
      return fail(err, err_len, -1, @"CoreSimulator isn't installed");
    // Older Xcodes keep SimulatorKit beside the developer tools, newer ones in SharedFrameworks.
    BOOL loaded = NO;
    for (NSString *path in @[@"Library/PrivateFrameworks/SimulatorKit.framework/SimulatorKit",
                             @"../SharedFrameworks/SimulatorKit.framework/SimulatorKit"]) {
      NSString *kit = [[developerDir stringByAppendingPathComponent:path] stringByStandardizingPath];
      if (dlopen(kit.fileSystemRepresentation, RTLD_NOW)) {
        loaded = YES;
        break;
      }
    }
    if (!loaded) return fail(err, err_len, -1, [NSString stringWithFormat:@"can't load SimulatorKit: %s", dlerror()]);
    NSObject *device = findDevice(developerDir, @(udid));
    if (!device) return fail(err, err_len, -2, @"no simulator with that id");
    if (![[device valueForKey:@"stateString"] isEqual:@"Booted"]) return fail(err, err_len, -3, @"the simulator isn't running");
    gIO = ((id (*)(id, SEL))objc_msgSend)(device, NSSelectorFromString(@"io"));
    if (!gIO) return fail(err, err_len, -4, @"no IO client for the simulator");
    gOut = out;
    gQueue = dispatch_queue_create("app.repogo.simcap", dispatch_queue_attr_make_with_qos_class(
                                                           DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0));
    gDevice = device;
    gUDID = strdup(udid);
    readPanels(device);
    // The active panel and its turn before the first frame: sent after it, the
    // phone painted the frame unturned (or the other panel's) and then flipped.
    uint32_t screen = 0;
    int32_t rotation = 0;
    if (universal && SSCoreDeviceDisplayAvailable() && simcap_uhid_display(udid, &screen, &rotation)) {
      atomic_store(&gActiveScreen, screen);
      tellHost(@{@"display": @{@"screen": @(screen), @"rotation": @(rotation), @"foldable": gIntegratedPanels == 2 ? @YES : @NO}});
    }
    __block int code = 0;
    dispatch_sync(gQueue, ^{
      code = wireFramebuffer(err, err_len);
      if (code == 0) capture();  // the first frame, before anything changes
    });
    if (code != 0) return code;
    setUpHID(device);
    if (universal) setUpUniversalHID(udid);
    return 0;
  }
}

void simcap_stop(void) {
  if (!gQueue) return;
  dispatch_sync(gQueue, ^{
    SEL unreg = NSSelectorFromString(@"unregisterScreenCallbacksWithUUID:");
    for (NSObject *desc in gDescriptors) {
      NSUUID *uuid = gCallbacks[[NSValue valueWithNonretainedObject:desc]];
      if (uuid && [desc respondsToSelector:unreg]) ((void (*)(id, SEL, id))objc_msgSend)(desc, unreg, uuid);
    }
    gDescriptors = nil;
    if (gSession) VTCompressionSessionCompleteFrames(gSession, kCMTimeInvalid);
    endSession();
  });
}
