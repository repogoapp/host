// Universal HID: how iOS 27 simulators take touches and keys, through
// CoreDevice's private Swift API. Adapted from serve-sim's CoreDeviceShim
// (Apache-2.0, https://github.com/expo/serve-sim); its SS names are kept so
// fixes there carry over.
#pragma once
#include <stdbool.h>
#include <stdint.h>

// Loads CoreDevice and resolves every symbol the bridge calls; false when
// this Xcode lacks one, before anything is called.
bool SSCoreDeviceInitialize(void);
void *SSCoreDeviceSymbol(const char *name);
void SSCoreDeviceRetainBridgeObject(void *object);
uintptr_t SSCoreDeviceValueSize(void *metadata);
void SSCoreDeviceDestroyValue(void *value, void *metadata);
bool SSCoreDeviceOptionalHasValue(const void *value, void *wrappedMetadata);

struct SSCoreDeviceTouch { double x, y; };
bool SSCoreDeviceDigitizerAvailable(void);
// Coordinates are fractions in the panel's native orientation. edge 1 to 4
// (left, top, bottom, right) marks a system swipe in from that edge.
bool SSCoreDeviceSendTouches(void *capability, uint32_t serviceID, const struct SSCoreDeviceTouch *contacts,
                             uint8_t count, bool touching, uint32_t edge);
bool SSCoreDeviceKeyboardAvailable(void);
bool SSCoreDeviceSendKey(void *capability, uint32_t usage, bool down);

// Displays, from CoreDeviceUtilities: which panel is active and how it's turned.
struct SSCoreDeviceSwiftString { uintptr_t first, second; };
bool SSCoreDeviceDisplayAvailable(void);
void *SSCoreDeviceDisplayMetadata(void);
void *SSCoreDeviceDisplayInfoMetadata(void);
void *SSCoreDeviceDisplays(void *info);
uint32_t SSCoreDeviceDisplayID(void *display);
uint8_t SSCoreDeviceDisplayActive(void *display);
struct SSCoreDeviceSwiftString SSCoreDeviceDisplayOrientation(void *display);

// Device Hub's vendor controls: a hinge angle, a physical orientation. Each
// returns a retained CFData for simcap_uhid_control.
void *SSCoreDeviceHingeData(double angle);
void *SSCoreDeviceOrientationData(const char *value);
bool SSCoreDeviceSendControl(void *capability, uint64_t data0, uint64_t data1);

// The Swift bridge (uhid/Bridge.swift): a device's capability, found by its
// type's metadata and conformance symbols, or NULL. Builds tagged simcap_uhid
// link it; others a stub that says so (uhid_stub_darwin.go).
void *simcap_uhid_capability(const char *udid, const char *metadata, const char *witness);
// The active panel's screen ID and its framebuffer's rotation in degrees
// (0, 90, 180, 270); false when CoreDevice can't say within two seconds.
bool simcap_uhid_display(const char *udid, uint32_t *screen, int32_t *rotation);
// Sends a vendor control (data: a CFData from SSCoreDeviceHingeData or
// SSCoreDeviceOrientationData, released here) through capability.
bool simcap_uhid_control(void *capability, void *data);
