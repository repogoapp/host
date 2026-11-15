// Universal HID through CoreDevice. Adapted from serve-sim's CoreDeviceShim
// (Apache-2.0): CoreDeviceShim.m, CoreDeviceDigitizerShim.m,
// CoreDeviceKeyboardShim.m and the value helpers of CoreDeviceDisplayShim.m.
#import <Foundation/Foundation.h>
#import <IOKit/IOCFSerialize.h>
#import <dlfcn.h>
#import <mach/mach_time.h>
#import <math.h>
#import <ptrauth.h>

#include "uhid_darwin.h"

// Targets for the register-preserving trampolines in uhid_darwin.S.
void *SSCDSharedTarget, *SSCDConnectionTarget, *SSCDCreateManagerTarget;
void *SSCDAllDevicesTarget, *SSCDInitializedTarget, *SSCDIdentifierTarget;
void *SSCDImplementationTarget, *SSCDVisibilityAllCasesTarget, *SSCDDisplayInfoTarget;
extern int32_t SSCDImplementationTu[2], SSCDDisplayInfoTu[2];
static void *framework, *sendTarget, *barrierTarget, *errorMetadataTarget;

static bool resolve(void **destination, const char *symbol) {
  *destination = dlsym(framework, symbol);
  return *destination != NULL;
}

bool SSCoreDeviceInitialize(void) {
  static dispatch_once_t once;
  static bool available;
  dispatch_once(&once, ^{
    framework = dlopen("/Library/Developer/PrivateFrameworks/CoreDevice.framework/CoreDevice", RTLD_NOW | RTLD_GLOBAL);
    if (!framework) return;
    bool found = true;
    found &= resolve(&SSCDSharedTarget, "$s10CoreDevice0B7ManagerC6sharedACvgZ");
    found &= resolve(&SSCDConnectionTarget, "$s10CoreDevice0aB7ServiceV16sharedConnectionAA0abcE0_pvgZ");
    found &= resolve(&SSCDCreateManagerTarget,
                     "$s10CoreDevice0B7ManagerC17serviceConnection07allowedB17VisibilityClassesAcA0ab7ServiceE0_p_ShyAA0bG5ClassOGtcfC");
    found &= resolve(&SSCDAllDevicesTarget, "$s10CoreDevice0B7ManagerC10allDevicesSayAA06RemoteB0CGyF");
    found &= resolve(&SSCDInitializedTarget, "$s10CoreDevice0B7ManagerC16fullyInitializedSbvgTj");
    found &= resolve(&SSCDIdentifierTarget, "$s10CoreDevice06RemoteB0C16deviceIdentifier10Foundation4UUIDVvgTj");
    found &= resolve(&SSCDVisibilityAllCasesTarget, "$s10CoreDevice0B15VisibilityClassO8allCasesSayACGvgZ");
    found &= resolve(&SSCDImplementationTarget,
                     "$s10CoreDevice06RemoteB0C17getImplementation3for12ProtocolTypeQzAA22CapabilityStaticMemberVyxG_tYaKAA0bI0RzlFTj");
    found &= resolve(&SSCDDisplayInfoTarget, "$s10CoreDevice06RemoteB0C11displayInfoAA07DisplayE0Vvg");
    found &= resolve(&sendTarget,
                     "$s10CoreDevice16HIDVendorDefinedP4send9usagePage0F07version4datays6UInt16V_AJs6UInt32V10Foundation4DataVtAA0aB5ErrorVYKFTj");
    found &= resolve(&barrierTarget, "$s10CoreDevice16HIDVendorDefinedP11sendBarrieryyFTj");
    found &= resolve(&errorMetadataTarget, "$s10CoreDevice0aB5ErrorVMa");
    int32_t *implementation = dlsym(framework,
        "$s10CoreDevice06RemoteB0C17getImplementation3for12ProtocolTypeQzAA22CapabilityStaticMemberVyxG_tYaKAA0bI0RzlFTjTu");
    int32_t *displayInfo = dlsym(framework, "$s10CoreDevice06RemoteB0C11displayInfoAA07DisplayE0VvgTu");
    if (!found || !implementation || !displayInfo) return;
    // Swift sizes the callee's async context from these descriptors; a
    // guessed size corrupts the task's allocation stack.
    SSCDImplementationTu[1] = implementation[1];
    SSCDDisplayInfoTu[1] = displayInfo[1];
    available = true;
  });
  return available;
}

void *SSCoreDeviceSymbol(const char *name) { return SSCoreDeviceInitialize() ? dlsym(framework, name) : NULL; }

void SSCoreDeviceRetainBridgeObject(void *object) {
  typedef void *(*Retain)(void *);
  ((Retain)dlsym(RTLD_DEFAULT, "swift_bridgeObjectRetain"))(object);
}

// Swift ABI value-witness entries, not offsets into a private framework's
// object layout: they keep resilient struct sizes dynamic.
static void **valueWitnesses(void *metadata) {
  return ptrauth_strip(((void **)metadata)[-1], ptrauth_key_process_independent_data);
}

uintptr_t SSCoreDeviceValueSize(void *metadata) { return ((uintptr_t *)valueWitnesses(metadata))[8]; }

typedef void (*DestroyFn)(void *, void *) __attribute__((swiftcall));
typedef unsigned (*SinglePayloadTagFn)(const void *, unsigned, void *) __attribute__((swiftcall));

void SSCoreDeviceDestroyValue(void *value, void *metadata) {
  void *entry = ptrauth_strip(valueWitnesses(metadata)[1], ptrauth_key_function_pointer);
  DestroyFn destroy = ptrauth_sign_unauthenticated(entry, ptrauth_key_function_pointer, 0);
  destroy(value, metadata);
}

bool SSCoreDeviceOptionalHasValue(const void *value, void *wrappedMetadata) {
  // getEnumTagSinglePayload: Optional has one empty case; tag 0 is a payload.
  void *entry = ptrauth_strip(valueWitnesses(wrappedMetadata)[6], ptrauth_key_function_pointer);
  SinglePayloadTagFn getTag = ptrauth_sign_unauthenticated(entry, ptrauth_key_function_pointer, 0);
  return getTag(value, 1, wrappedMetadata) == 0;
}

struct MetadataResponse { void *metadata; uintptr_t state; };
typedef struct MetadataResponse (*Metadata)(uintptr_t) __attribute__((swiftcall));
typedef struct MetadataResponse (*OptionalMetadata)(uintptr_t, void *) __attribute__((swiftcall));

// MARK: - Digitizer

// UniversalHID's frozen report and contact wrappers each hold a Foundation.Data:
// Apple's constructors and setters encode the descriptor.
struct DataWords { uint64_t first, second; };
typedef void (*DefaultInit)(void *__attribute__((swift_indirect_result)), void *, void *, void *) __attribute__((swiftcall));
typedef struct DataWords (*ContactInit)(void) __attribute__((swiftcall));
typedef void (*SetDouble)(double, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetInt)(intptr_t, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetByte)(uint8_t, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetBool)(bool, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetIdentity)(uint8_t, intptr_t, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetSwipe)(bool, intptr_t, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetTime)(uint64_t, bool, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SetContact)(uint64_t, uint64_t, intptr_t, void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef void (*SendReport)(uint64_t, uint64_t, void *, void *, void *, void *, void *__attribute__((swift_context)),
                           void **__attribute__((swift_error_result))) __attribute__((swiftcall));

static void *reportMetadata, *contactMetadata, *descriptorWitness, *reportWitness, *errorMetadata;
static DefaultInit initReport;
static ContactInit initContact;
static SetInt setIndex;
static SetByte setCount;
static SetDouble setX, setY;
static SetBool setTouch, setRange;
static SetContact setContact;
static SetIdentity setIdentity;
static SetTime setTime;
static SetSwipe setSwipe[5];
static SetSwipe setSwipeLocked;
static SendReport sendReport;

bool SSCoreDeviceDigitizerAvailable(void) {
  static dispatch_once_t once;
  static bool available;
  dispatch_once(&once, ^{
    if (!SSCoreDeviceInitialize()) return;
    bool found = true;
#define RESOLVE(variable, symbol)                       \
  do {                                                  \
    variable = (typeof(variable))dlsym(RTLD_DEFAULT, symbol); \
    found &= variable != NULL;                          \
  } while (0)
    RESOLVE(reportMetadata, "$s12UniversalHID15DigitizerReportVN");
    RESOLVE(contactMetadata, "$s12UniversalHID16DigitizerContactVN");
    RESOLVE(descriptorWitness, "$s12UniversalHID15DigitizerReportVAA05EventD18DescriptorProtocolAAWP");
    RESOLVE(reportWitness, "$s12UniversalHID15DigitizerReportVAA05EventD8ProtocolAAWP");
    RESOLVE(initReport, "$s12UniversalHID19EventReportProtocolPA2A0cd10DescriptorE0RzrlExycfC");
    RESOLVE(initContact, "$s12UniversalHID16DigitizerContactVACycfC");
    RESOLVE(setIndex, "$s12UniversalHID16DigitizerContactV5indexSivs");
    RESOLVE(setX, "$s12UniversalHID16DigitizerContactV1xSdvs");
    RESOLVE(setY, "$s12UniversalHID16DigitizerContactV1ySdvs");
    RESOLVE(setTouch, "$s12UniversalHID16DigitizerContactV5touchSbvs");
    RESOLVE(setRange, "$s12UniversalHID16DigitizerContactV5rangeSbvs");
    RESOLVE(setCount, "$s12UniversalHID15DigitizerReportV12contactCounts5UInt8Vvs");
    RESOLVE(setContact, "$s12UniversalHID15DigitizerReportV10setContact_7atIndexyAA0cF0V_SitF");
    RESOLVE(setIdentity, "$s12UniversalHID15DigitizerReportV18setContactIdentity_7atIndexys5UInt8V_SitF");
    RESOLVE(setTime, "$s12UniversalHID15DigitizerReportV15remoteTimestamps6UInt64VSgvs");
    // Edges left, top, bottom, right; the swipe runs inward from each.
    RESOLVE(setSwipe[1], "$s12UniversalHID15DigitizerReportV20setContactSwipeRight_7atIndexySb_SitF");
    RESOLVE(setSwipe[2], "$s12UniversalHID15DigitizerReportV19setContactSwipeDown_7atIndexySb_SitF");
    RESOLVE(setSwipe[3], "$s12UniversalHID15DigitizerReportV17setContactSwipeUp_7atIndexySb_SitF");
    RESOLVE(setSwipe[4], "$s12UniversalHID15DigitizerReportV19setContactSwipeLeft_7atIndexySb_SitF");
    RESOLVE(sendReport, "$s10CoreDevice19UniversalHIDServiceP4send6report2toy0C3HID9HIDReportV_AA0D2IDVtAA0aB5ErrorVYKFTj");
#undef RESOLVE
    // Improves system edge swipes on newer runtimes; touch works without it.
    setSwipeLocked = (SetSwipe)dlsym(RTLD_DEFAULT, "$s12UniversalHID15DigitizerReportV21setContactSwipeLocked_7atIndexySb_SitF");
    Metadata errorType = (Metadata)SSCoreDeviceSymbol("$s10CoreDevice0aB5ErrorVMa");
    if (errorType) errorMetadata = errorType(0).metadata;
    // The stack buffers and by-value calls below assume two words; a changed
    // layout is refused before any constructor or setter can use it.
    available = found && errorMetadata && SSCoreDeviceValueSize(reportMetadata) == sizeof(struct DataWords) &&
                SSCoreDeviceValueSize(contactMetadata) == sizeof(struct DataWords);
  });
  return available;
}

bool SSCoreDeviceSendTouches(void *capability, uint32_t serviceID, const struct SSCoreDeviceTouch *contacts,
                             uint8_t count, bool touching, uint32_t edge) {
  if (!capability || !contacts || count < 1 || count > 2 || edge > 4 || !SSCoreDeviceDigitizerAvailable()) return false;
  for (uint8_t i = 0; i < count; i++) {
    if (!isfinite(contacts[i].x) || !isfinite(contacts[i].y)) return false;
  }
  void *metadata = ((void **)capability)[3], *witness = ((void **)capability)[4];
  if (!metadata || !witness) return false;
  void *error = calloc(1, SSCoreDeviceValueSize(errorMetadata));
  if (!error) return false;

  struct DataWords report;
  initReport(&report, reportMetadata, descriptorWitness, reportWitness);
  setCount(count, &report);
  for (uint8_t i = 0; i < count; i++) {
    struct DataWords contact = initContact();
    setIndex(i + 1, &contact);
    setX(fmin(1, fmax(0, contacts[i].x)), &contact);
    setY(fmin(1, fmax(0, contacts[i].y)), &contact);
    setTouch(touching, &contact);
    setRange(touching, &contact);
    setContact(contact.first, contact.second, i, &report);
    SSCoreDeviceDestroyValue(&contact, contactMetadata);
    setIdentity(i + 1, i, &report);
    if (edge) {
      // Device Hub pairs the direction with swipeLocked, and keeps both on the
      // lift so SpringBoard can finish a Home or app-switcher swipe.
      if (setSwipeLocked) setSwipeLocked(true, i, &report);
      setSwipe[edge](true, i, &report);
    }
  }
  setTime(mach_absolute_time(), false, &report);
  // HIDServiceID is resilient: its address goes, not its value.
  uint64_t service = serviceID;
  void *errorFlag = NULL;
  sendReport(report.first, report.second, &service, error, metadata, witness, capability, &errorFlag);
  SSCoreDeviceDestroyValue(&report, reportMetadata);
  if (errorFlag) SSCoreDeviceDestroyValue(error, errorMetadata);
  free(error);
  return errorFlag == NULL;
}

// MARK: - Displays

typedef void *(*DisplaysFn)(void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef uint32_t (*DisplayIDFn)(void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef uint8_t (*DisplayActiveFn)(void *__attribute__((swift_context))) __attribute__((swiftcall));
typedef struct SSCoreDeviceSwiftString (*OrientationFn)(void *__attribute__((swift_context))) __attribute__((swiftcall));

static Metadata displayMetadata, infoMetadata;
static DisplaysFn displays;
static DisplayIDFn displayID;
static DisplayActiveFn displayActive;
static OrientationFn displayOrientation;

bool SSCoreDeviceDisplayAvailable(void) {
  static dispatch_once_t once;
  static bool available;
  dispatch_once(&once, ^{
    void *handle = dlopen("/Library/Developer/PrivateFrameworks/CoreDeviceUtilities.framework/CoreDeviceUtilities",
                          RTLD_NOW | RTLD_LOCAL);
    if (!handle) return;
    displayMetadata = (Metadata)dlsym(handle, "$s10CoreDevice7DisplayVMa");
    infoMetadata = (Metadata)dlsym(handle, "$s10CoreDevice11DisplayInfoVMa");
    displays = (DisplaysFn)dlsym(handle, "$s10CoreDevice11DisplayInfoV8displaysSayAA0C0VGvg");
    displayID = (DisplayIDFn)dlsym(handle, "$s10CoreDevice7DisplayV9displayIds6UInt32Vvg");
    displayActive = (DisplayActiveFn)dlsym(handle, "$s10CoreDevice7DisplayV6activeSbSgvg");
    displayOrientation = (OrientationFn)dlsym(handle, "$s10CoreDevice7DisplayV18currentOrientationSSvg");
    available = displayMetadata && infoMetadata && displays && displayID && displayActive && displayOrientation;
  });
  return available;
}

void *SSCoreDeviceDisplayMetadata(void) { return displayMetadata(0).metadata; }
void *SSCoreDeviceDisplayInfoMetadata(void) { return infoMetadata(0).metadata; }
void *SSCoreDeviceDisplays(void *info) { return displays(info); }
uint32_t SSCoreDeviceDisplayID(void *display) { return displayID(display); }
uint8_t SSCoreDeviceDisplayActive(void *display) { return displayActive(display); }
struct SSCoreDeviceSwiftString SSCoreDeviceDisplayOrientation(void *display) { return displayOrientation(display); }

// MARK: - Vendor controls

// Device Hub's controls use IOKit's binary serializer, not a plist.
static void *serialize(NSDictionary *command) { return (void *)IOCFSerialize((__bridge CFDictionaryRef)command, 1); }

void *SSCoreDeviceHingeData(double angle) {
  return serialize(@{@"provider": @"com.apple.Virtualization.VirtualMachines", @"source": @"hinge-slider-control",
                     @"type": @"range", @"value": @(angle)});
}

void *SSCoreDeviceOrientationData(const char *value) {
  if (!value) return NULL;
  return serialize(@{@"provider": @"com.apple.Virtualization.VirtualMachines", @"source": @"orientation-picker-control",
                     @"type": @"enum", @"value": @(value)});
}

typedef void (*SendControl)(uint16_t, uint16_t, uint32_t, uint64_t, uint64_t, void *, void *, void *,
                            void *__attribute__((swift_context)), void **__attribute__((swift_error_result)))
    __attribute__((swiftcall));
typedef void (*Barrier)(void *, void *, void *__attribute__((swift_context))) __attribute__((swiftcall));

bool SSCoreDeviceSendControl(void *capability, uint64_t data0, uint64_t data1) {
  if (!capability || !SSCoreDeviceInitialize()) return false;
  void *metadata = ((void **)capability)[3], *witness = ((void **)capability)[4];
  if (!metadata || !witness) return false;
  void *errorType = ((Metadata)errorMetadataTarget)(0).metadata;
  void *error = calloc(1, SSCoreDeviceValueSize(errorType));
  if (!error) return false;
  void *errorFlag = NULL;
  ((SendControl)sendTarget)(0xff61, 0x5b, 0, data0, data1, error, metadata, witness, capability, &errorFlag);
  if (errorFlag) SSCoreDeviceDestroyValue(error, errorType);
  else ((Barrier)barrierTarget)(metadata, witness, capability);
  free(error);
  return errorFlag == NULL;
}

// MARK: - Keyboard

typedef void (*InitKey)(void *__attribute__((swift_indirect_result)), uint16_t) __attribute__((swiftcall));
typedef void (*InitState)(void *__attribute__((swift_indirect_result)), uint8_t) __attribute__((swiftcall));
typedef void (*SendKey)(void *, void *, void *, void *, void *__attribute__((swift_context)),
                        void **__attribute__((swift_error_result))) __attribute__((swiftcall));
typedef void (*ReleaseError)(void *);

static void *keyMetadata, *stateMetadata, *optionalStateMetadata;
static InitKey initKey;
static InitState initState;
static SendKey sendKey;
static ReleaseError releaseError;

bool SSCoreDeviceKeyboardAvailable(void) {
  static dispatch_once_t once;
  static bool available;
  dispatch_once(&once, ^{
    if (!SSCoreDeviceInitialize()) return;
    bool found = true;
    Metadata keyType, stateType;
#define RESOLVE(variable, symbol)                                 \
  do {                                                            \
    variable = (typeof(variable))SSCoreDeviceSymbol(symbol);      \
    found &= variable != NULL;                                    \
  } while (0)
    RESOLVE(keyType, "$s10CoreDevice20HIDKeyboardUsageCodeVMa");
    RESOLVE(stateType, "$s10CoreDevice14HIDButtonStateOMa");
    RESOLVE(initKey, "$s10CoreDevice20HIDKeyboardUsageCodeV8rawValueACs6UInt16V_tcfC");
    RESOLVE(initState, "$s10CoreDevice14HIDButtonStateO8rawValueACSgs5UInt8V_tcfC");
    RESOLVE(sendKey, "$s10CoreDevice11HIDKeyboardP4send3key5stateyAA0C9UsageCodeV_AA14HIDButtonStateOtKFTj");
#undef RESOLVE
    OptionalMetadata optionalType = (OptionalMetadata)dlsym(RTLD_DEFAULT, "$sSqMa");
    releaseError = (ReleaseError)dlsym(RTLD_DEFAULT, "swift_errorRelease");
    if (!found || !optionalType || !releaseError) return;
    keyMetadata = keyType(0).metadata;
    stateMetadata = stateType(0).metadata;
    if (!keyMetadata || !stateMetadata) return;
    optionalStateMetadata = optionalType(0, stateMetadata).metadata;
    // The state initializer returns an Optional, which may take a tag byte.
    available = optionalStateMetadata && SSCoreDeviceValueSize(keyMetadata) == sizeof(uint16_t) &&
                SSCoreDeviceValueSize(stateMetadata) == sizeof(uint8_t) &&
                SSCoreDeviceValueSize(optionalStateMetadata) >= sizeof(uint8_t) &&
                SSCoreDeviceValueSize(optionalStateMetadata) <= 2;
  });
  return available;
}

bool SSCoreDeviceSendKey(void *capability, uint32_t usage, bool down) {
  if (!capability || usage > UINT16_MAX || !SSCoreDeviceKeyboardAvailable()) return false;
  void *metadata = ((void **)capability)[3], *witness = ((void **)capability)[4];
  if (!metadata || !witness) return false;
  uint8_t state[2] = {0};
  initState(state, down ? 1 : 2);
  if (!SSCoreDeviceOptionalHasValue(state, stateMetadata)) {
    SSCoreDeviceDestroyValue(state, optionalStateMetadata);
    return false;
  }
  uint16_t key = 0;
  initKey(&key, (uint16_t)usage);
  void *error = NULL;
  sendKey(&key, state, metadata, witness, capability, &error);
  SSCoreDeviceDestroyValue(&key, keyMetadata);
  SSCoreDeviceDestroyValue(state, optionalStateMetadata);
  // HIDKeyboard throws untyped: the error register owns a Swift error.
  if (error) {
    releaseError(error);
    return false;
  }
  return true;
}
