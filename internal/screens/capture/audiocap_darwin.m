#import <AudioToolbox/AudioToolbox.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <CoreAudio/CATapDescription.h>
#import <CoreAudio/CoreAudio.h>
#import <Foundation/Foundation.h>
#import <os/lock.h>
#import <time.h>
#import <unistd.h>

#include "audiocap_darwin.h"

// The Mac's sound for a live view. A process tap (macOS 14.2+) takes
// everything the Mac plays; it can't run an IOProc itself, so it sits in a
// private aggregate device clocked by the default output, which is read
// instead. The IO thread only copies PCM; a loop on this thread encodes it.
// The tap and the aggregate are private to this process, so they, and the
// mute, go when it exits however it ends.

enum { kOutputRate = 48000, kChannels = 2, kBitRate = 96000 };

static int gFD = -1;
static os_unfair_lock gLock = OS_UNFAIR_LOCK_INIT;
static NSMutableData *gPending;
static AudioStreamBasicDescription gInFormat;

static void fail(char *why, int whyLength, NSString *message) {
  snprintf(why, (size_t)whyLength, "%s", message.UTF8String);
}

static void writeAll(NSData *data) {
  const uint8_t *bytes = data.bytes;
  NSUInteger left = data.length;
  while (left > 0) {
    ssize_t n = write(gFD, bytes, left);
    if (n <= 0) _exit(0);  // the host closed its end: the view is over
    bytes += n;
    left -= (NSUInteger)n;
  }
}

// A message for the host rather than a frame: JSON, its length's top bit set.
static void tellHost(NSDictionary *message) {
  NSData *json = [NSJSONSerialization dataWithJSONObject:message options:0 error:nil];
  if (!json) return;
  uint32_t length = CFSwapInt32HostToBig((uint32_t)json.length | 0x80000000u);
  NSMutableData *out = [NSMutableData dataWithBytes:&length length:4];
  [out appendData:json];
  writeAll(out);
}

// One AAC access unit as a frame: its length, its capture time (8 bytes,
// big-endian microseconds on the uptime clock, as the video's), the unit.
static void writeFrame(const void *unit, uint32_t size, uint64_t capturedUS) {
  NSMutableData *out = [NSMutableData dataWithCapacity:12 + size];
  uint32_t length = CFSwapInt32HostToBig(8 + size);
  uint64_t time = CFSwapInt64HostToBig(capturedUS);
  [out appendBytes:&length length:4];
  [out appendBytes:&time length:8];
  [out appendBytes:unit length:size];
  writeAll(out);
}

static uint64_t uptimeUS(void) { return clock_gettime_nsec_np(CLOCK_UPTIME_RAW) / 1000; }

static AudioObjectPropertyAddress global(AudioObjectPropertySelector selector) {
  return (AudioObjectPropertyAddress){selector, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
}

// Feeds the converter what has been taken off `gPending`.
typedef struct {
  NSData *data;
  NSUInteger offset;
} Feed;

static OSStatus feed(AudioConverterRef converter, UInt32 *packets, AudioBufferList *list,
                     AudioStreamPacketDescription **descriptions, void *user) {
  Feed *f = user;
  UInt32 frameSize = gInFormat.mBytesPerFrame;
  NSUInteger available = (f->data.length - f->offset) / frameSize;
  if (available == 0) {
    *packets = 0;
    return 1;  // no more for now; the next loop brings more
  }
  UInt32 frames = (UInt32)MIN((NSUInteger)*packets, available);
  list->mBuffers[0].mData = (uint8_t *)f->data.bytes + f->offset;
  list->mBuffers[0].mDataByteSize = frames * frameSize;
  list->mBuffers[0].mNumberChannels = gInFormat.mChannelsPerFrame;
  f->offset += frames * frameSize;
  *packets = frames;
  return noErr;
}

int audiocap_run(int fd, int mute, int excludePID, char *why, int whyLength) {
  gFD = fd;
  gPending = [NSMutableData data];
  if (@available(macOS 14.2, *)) {
  } else {
    fail(why, whyLength, @"Sound needs macOS 14.2 or later on this Mac.");
    return 1;
  }

  // The tap: everything the Mac plays, but the iPhone mic's own playing.
  NSMutableArray<NSNumber *> *excluded = [NSMutableArray array];
  if (excludePID > 0) {
    pid_t pid = excludePID;
    AudioObjectID process = kAudioObjectUnknown;
    UInt32 processSize = sizeof(process);
    AudioObjectPropertyAddress translate = global(kAudioHardwarePropertyTranslatePIDToProcessObject);
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &translate, sizeof(pid), &pid, &processSize, &process) ==
            noErr && process != kAudioObjectUnknown) {
      [excluded addObject:@(process)];
    }
  }
  CATapDescription *tap = [[CATapDescription alloc] initStereoGlobalTapButExcludeProcesses:excluded];
  tap.UUID = [NSUUID UUID];
  tap.privateTap = YES;
  tap.muteBehavior = mute ? CATapMutedWhenTapped : CATapUnmuted;
  AudioObjectID tapID = kAudioObjectUnknown;
  OSStatus status = AudioHardwareCreateProcessTap(tap, &tapID);
  if (status != noErr) {
    fail(why, whyLength, [NSString stringWithFormat:@"Couldn't tap the Mac's sound (%d).", (int)status]);
    return 1;
  }

  // A private aggregate around it, clocked by the default output.
  AudioObjectID output = kAudioObjectUnknown;
  UInt32 size = sizeof(output);
  AudioObjectPropertyAddress address = global(kAudioHardwarePropertyDefaultOutputDevice);
  AudioObjectGetPropertyData(kAudioObjectSystemObject, &address, 0, NULL, &size, &output);
  CFStringRef outputUID = NULL;
  size = sizeof(outputUID);
  address = global(kAudioDevicePropertyDeviceUID);
  if (AudioObjectGetPropertyData(output, &address, 0, NULL, &size, &outputUID) != noErr || !outputUID) {
    fail(why, whyLength, @"This Mac has no sound output to follow.");
    return 1;
  }
  NSString *outputUIDString = CFBridgingRelease(outputUID);
  NSDictionary *aggregate = @{
    @kAudioAggregateDeviceNameKey: @"RepoGo Screens",
    @kAudioAggregateDeviceUIDKey: [NSUUID UUID].UUIDString,
    @kAudioAggregateDeviceMainSubDeviceKey: outputUIDString,
    @kAudioAggregateDeviceIsPrivateKey: @YES,
    @kAudioAggregateDeviceIsStackedKey: @NO,
    @kAudioAggregateDeviceTapAutoStartKey: @YES,
    @kAudioAggregateDeviceSubDeviceListKey: @[@{@kAudioSubDeviceUIDKey: outputUIDString}],
    @kAudioAggregateDeviceTapListKey: @[@{
      @kAudioSubTapDriftCompensationKey: @YES,
      @kAudioSubTapUIDKey: tap.UUID.UUIDString,
    }],
  };
  AudioObjectID aggregateID = kAudioObjectUnknown;
  status = AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)aggregate, &aggregateID);
  if (status != noErr) {
    fail(why, whyLength, [NSString stringWithFormat:@"Couldn't read the Mac's sound (%d).", (int)status]);
    return 1;
  }

  // The tap's format, at the rate samples really arrive: the tap can say
  // 48 kHz while the clock device runs at 44.1 (a MacBook's speakers).
  size = sizeof(gInFormat);
  address = global(kAudioTapPropertyFormat);
  AudioObjectGetPropertyData(tapID, &address, 0, NULL, &size, &gInFormat);
  Float64 rate = 0;
  size = sizeof(rate);
  address = global(kAudioDevicePropertyNominalSampleRate);
  if (AudioObjectGetPropertyData(aggregateID, &address, 0, NULL, &size, &rate) == noErr && rate > 0) {
    gInFormat.mSampleRate = rate;
  }

  AudioStreamBasicDescription aac = {
    .mSampleRate = kOutputRate, .mFormatID = kAudioFormatMPEG4AAC, .mFormatFlags = kMPEG4Object_AAC_LC,
    .mFramesPerPacket = 1024, .mChannelsPerFrame = kChannels,
  };
  AudioConverterRef converter = NULL;
  status = AudioConverterNew(&gInFormat, &aac, &converter);
  if (status != noErr) {
    fail(why, whyLength, [NSString stringWithFormat:@"Couldn't encode the Mac's sound (%d).", (int)status]);
    return 1;
  }
  UInt32 bitRate = kBitRate;
  AudioConverterSetProperty(converter, kAudioConverterEncodeBitRate, sizeof(bitRate), &bitRate);
  UInt32 maxPacket = 0;
  size = sizeof(maxPacket);
  AudioConverterGetProperty(converter, kAudioConverterPropertyMaximumOutputPacketSize, &size, &maxPacket);
  if (maxPacket < 1536) maxPacket = 1536;

  AudioDeviceIOProcID ioProc = NULL;
  status = AudioDeviceCreateIOProcIDWithBlock(&ioProc, aggregateID, NULL,
      ^(const AudioTimeStamp *now, const AudioBufferList *input, const AudioTimeStamp *inputTime,
        AudioBufferList *output, const AudioTimeStamp *outputTime) {
        if (input->mNumberBuffers == 0 || !input->mBuffers[0].mData) return;
        os_unfair_lock_lock(&gLock);
        [gPending appendBytes:input->mBuffers[0].mData length:input->mBuffers[0].mDataByteSize];
        os_unfair_lock_unlock(&gLock);
      });
  if (status != noErr || AudioDeviceStart(aggregateID, ioProc) != noErr) {
    fail(why, whyLength, @"Couldn't start reading the Mac's sound.");
    return 1;
  }
  tellHost(@{@"audio": @{@"sample_rate": @(kOutputRate), @"channels": @(kChannels)}});

  // Encodes what arrived every 20 ms. Each unit is stamped with when its
  // first sample was captured: now, less what is still buffered.
  NSMutableData *packet = [NSMutableData dataWithLength:maxPacket];
  for (;;) {
    usleep(20000);
    NSData *taken;
    os_unfair_lock_lock(&gLock);
    taken = gPending.length > 0 ? [gPending copy] : nil;
    [gPending setLength:0];
    os_unfair_lock_unlock(&gLock);
    if (!taken) continue;
    Feed f = {taken, 0};
    for (;;) {
      UInt32 packets = 1;
      AudioStreamPacketDescription description;
      AudioBufferList list = {.mNumberBuffers = 1};
      list.mBuffers[0] = (AudioBuffer){kChannels, maxPacket, packet.mutableBytes};
      OSStatus s = AudioConverterFillComplexBuffer(converter, feed, &f, &packets, &list, &description);
      if (packets > 0 && list.mBuffers[0].mDataByteSize > 0) {
        writeFrame(packet.bytes, list.mBuffers[0].mDataByteSize, uptimeUS());
      }
      if (packets == 0 || (s != noErr && s != 1)) break;
    }
  }
}
