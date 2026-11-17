#import <AudioToolbox/AudioToolbox.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <CoreAudio/CATapDescription.h>
#import <CoreAudio/CoreAudio.h>
#import <Foundation/Foundation.h>
#import <os/lock.h>
#import <unistd.h>

#include "miccap_darwin.h"

// A phone's microphone as a Mac input, with no driver. This process plays
// what the phone records on the default output, muted; a tap of just this
// process, inside a public aggregate device, is what other apps see as an
// input named after the phone. Each phone has its own device, under an ID
// that stays the same, so an app that picked it picks it again. The device
// goes when this process does, except after a kill -9: the next run for
// that phone, or the host starting, removes it.
//
// Such a device can't be the Mac's default input (macOS doesn't allow it
// for a tap), so apps pick it by name.
enum {
  kRate = 24000,
  kBytesPerSecond = kRate * 2,
  // Played once this much has arrived after running dry, and cut back to
  // it when more than kMost waits: the phone's packets come unevenly.
  kStartAt = kBytesPerSecond * 60 / 1000,
  kMost = kBytesPerSecond * 200 / 1000,
};

static os_unfair_lock gLock = OS_UNFAIR_LOCK_INIT;
static NSMutableData *gPending;
static BOOL gPlaying;

static void fail(char *why, int whyLength, NSString *message) {
  snprintf(why, (size_t)whyLength, "%s", message.UTF8String);
}

static AudioObjectPropertyAddress global(AudioObjectPropertySelector selector) {
  return (AudioObjectPropertyAddress){selector, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
}

// Plays what is pending, silence when it runs dry.
static OSStatus render(void *ref, AudioUnitRenderActionFlags *flags, const AudioTimeStamp *time, UInt32 bus,
                       UInt32 frames, AudioBufferList *io) {
  uint8_t *out = io->mBuffers[0].mData;
  UInt32 want = frames * 2;
  UInt32 took = 0;
  os_unfair_lock_lock(&gLock);
  if (!gPlaying && gPending.length >= kStartAt) gPlaying = YES;
  if (gPlaying) {
    took = (UInt32)MIN((NSUInteger)want, gPending.length);
    memcpy(out, gPending.bytes, took);
    [gPending replaceBytesInRange:NSMakeRange(0, took) withBytes:NULL length:0];
    if (took < want) gPlaying = NO;
  }
  os_unfair_lock_unlock(&gLock);
  memset(out + took, 0, want - took);
  return noErr;
}

// Removes every device whose ID starts with prefix: what killed runs left.
void miccap_remove(const char *prefix) {
  @autoreleasepool {
    NSString *want = @(prefix);
    UInt32 size = 0;
    AudioObjectPropertyAddress address = global(kAudioHardwarePropertyDevices);
    if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &address, 0, NULL, &size) != noErr) return;
    NSMutableData *ids = [NSMutableData dataWithLength:size];
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &address, 0, NULL, &size, ids.mutableBytes) != noErr) return;
    const AudioObjectID *devices = ids.bytes;
    for (UInt32 i = 0; i < size / sizeof(AudioObjectID); i++) {
      CFStringRef uid = NULL;
      UInt32 uidSize = sizeof(uid);
      AudioObjectPropertyAddress uidAddress = global(kAudioDevicePropertyDeviceUID);
      if (AudioObjectGetPropertyData(devices[i], &uidAddress, 0, NULL, &uidSize, &uid) != noErr || !uid) continue;
      NSString *found = CFBridgingRelease(uid);
      if ([found hasPrefix:want]) AudioHardwareDestroyAggregateDevice(devices[i]);
    }
  }
}

// This process's audio object, which exists once it plays.
static AudioObjectID ownProcess(void) {
  pid_t pid = getpid();
  AudioObjectPropertyAddress address = global(kAudioHardwarePropertyTranslatePIDToProcessObject);
  for (int i = 0; i < 100; i++) {
    AudioObjectID process = kAudioObjectUnknown;
    UInt32 size = sizeof(process);
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &address, sizeof(pid), &pid, &size, &process) == noErr &&
        process != kAudioObjectUnknown) {
      return process;
    }
    usleep(20000);
  }
  return kAudioObjectUnknown;
}

static BOOL readAll(int fd, void *into, size_t length) {
  uint8_t *bytes = into;
  while (length > 0) {
    ssize_t n = read(fd, bytes, length);
    if (n <= 0) return NO;
    bytes += n;
    length -= (size_t)n;
  }
  return YES;
}

int miccap_run(int in, int out, const char *uid, const char *name, char *why, int whyLength) {
  gPending = [NSMutableData data];
  if (@available(macOS 14.2, *)) {
  } else {
    fail(why, whyLength, @"The phone's mic needs macOS 14.2 or later on this Mac.");
    return 1;
  }
  miccap_remove(uid);

  // Play on the default output: 16-bit mono at 24 kHz, as the phone sends.
  AudioComponentDescription description = {kAudioUnitType_Output, kAudioUnitSubType_DefaultOutput,
                                           kAudioUnitManufacturer_Apple};
  AudioUnit unit = NULL;
  AudioComponent component = AudioComponentFindNext(NULL, &description);
  if (!component || AudioComponentInstanceNew(component, &unit) != noErr) {
    fail(why, whyLength, @"This Mac has no sound output to play the phone's mic on.");
    return 1;
  }
  AudioStreamBasicDescription format = {
    .mSampleRate = kRate, .mFormatID = kAudioFormatLinearPCM,
    .mFormatFlags = kAudioFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked,
    .mBytesPerPacket = 2, .mFramesPerPacket = 1, .mBytesPerFrame = 2, .mChannelsPerFrame = 1, .mBitsPerChannel = 16,
  };
  AudioUnitSetProperty(unit, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Input, 0, &format, sizeof(format));
  AURenderCallbackStruct callback = {render, NULL};
  AudioUnitSetProperty(unit, kAudioUnitProperty_SetRenderCallback, kAudioUnitScope_Input, 0, &callback,
                       sizeof(callback));
  if (AudioUnitInitialize(unit) != noErr || AudioOutputUnitStart(unit) != noErr) {
    fail(why, whyLength, @"Couldn't start playing the phone's mic.");
    return 1;
  }

  // A public tap of just this process, always muted: muted only while
  // tapped, it would play aloud whenever no app is recording.
  AudioObjectID process = ownProcess();
  if (process == kAudioObjectUnknown) {
    fail(why, whyLength, @"Couldn't find the phone's mic's sound.");
    return 1;
  }
  CATapDescription *tap = [[CATapDescription alloc] initMonoMixdownOfProcesses:@[@(process)]];
  tap.UUID = [NSUUID UUID];
  tap.name = @(name);
  tap.privateTap = NO;
  tap.muteBehavior = CATapMuted;
  AudioObjectID tapID = kAudioObjectUnknown;
  OSStatus status = AudioHardwareCreateProcessTap(tap, &tapID);
  if (status != noErr) {
    fail(why, whyLength, [NSString stringWithFormat:@"Couldn't make the phone's mic (%d).", (int)status]);
    return 1;
  }

  // The device other apps pick: the tap alone, no sub-device, so its input
  // is just the mic.
  NSDictionary *aggregate = @{
    @kAudioAggregateDeviceNameKey: @(name),
    @kAudioAggregateDeviceUIDKey: @(uid),
    @kAudioAggregateDeviceIsPrivateKey: @NO,
    @kAudioAggregateDeviceIsStackedKey: @NO,
    @kAudioAggregateDeviceTapAutoStartKey: @YES,
    @kAudioAggregateDeviceSubDeviceListKey: @[],
    @kAudioAggregateDeviceTapListKey: @[@{
      @kAudioSubTapDriftCompensationKey: @YES,
      @kAudioSubTapUIDKey: tap.UUID.UUIDString,
    }],
  };
  AudioObjectID device = kAudioObjectUnknown;
  status = AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)aggregate, &device);
  if (status != noErr) {
    AudioHardwareDestroyProcessTap(tapID);
    fail(why, whyLength, [NSString stringWithFormat:@"Couldn't add the phone's mic to this Mac (%d).", (int)status]);
    return 1;
  }

  // Tells the host it's ready, with this process's ID for the sound's tap
  // to leave out: a host message, its length's top bit set.
  NSData *ready = [[NSString stringWithFormat:@"{\"mic\":{\"pid\":%d}}", getpid()]
      dataUsingEncoding:NSUTF8StringEncoding];
  uint32_t length = CFSwapInt32HostToBig((uint32_t)ready.length | 0x80000000u);
  write(out, &length, 4);
  write(out, ready.bytes, ready.length);

  // Then PCM from the host, each behind its 4-byte length, until it closes.
  NSMutableData *chunk = [NSMutableData data];
  for (;;) {
    uint32_t size = 0;
    if (!readAll(in, &size, 4)) break;
    size = CFSwapInt32BigToHost(size);
    if (size == 0 || size > 1 << 20) break;
    [chunk setLength:size];
    if (!readAll(in, chunk.mutableBytes, size)) break;
    os_unfair_lock_lock(&gLock);
    [gPending appendData:chunk];
    if (gPending.length > kMost) {
      NSUInteger cut = (gPending.length - kStartAt) & ~(NSUInteger)1;
      [gPending replaceBytesInRange:NSMakeRange(0, cut) withBytes:NULL length:0];
    }
    os_unfair_lock_unlock(&gLock);
  }

  AudioHardwareDestroyAggregateDevice(device);
  AudioHardwareDestroyProcessTap(tapID);
  AudioOutputUnitStop(unit);
  return 0;
}
