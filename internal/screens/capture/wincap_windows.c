//go:build windows && cgo

// See wincap_windows.h. One capture thread owns Desktop Duplication and the
// encoder; the host's pacing reaches it through the atomics below.
#define COBJMACROS
#define INITGUID
#include <windows.h>
#include <d3d11.h>
#include <dxgi1_2.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mferror.h>
#include <mftransform.h>
#include <strmif.h>
#include <codecapi.h>
#include <wmcodecdsp.h>
#include <stdio.h>
#include <string.h>

#include "wincap_windows.h"

// The Microsoft H.264 encoder MFT, for when enumeration finds none.
DEFINE_GUID(wincapH264Encoder, 0x6ca50344, 0x051a, 0x4ded, 0x97, 0x79, 0xa4, 0x33, 0x05, 0x16, 0x5e, 0x35);

// The encoder settings, from codecapi.h's values: in C its names need
// __uuidof, which only C++ has.
#define WINCAP_GUID(name, ...) DEFINE_GUID(name, __VA_ARGS__)
WINCAP_GUID(wincapRateControlMode, STATIC_CODECAPI_AVEncCommonRateControlMode);
WINCAP_GUID(wincapMeanBitRate, STATIC_CODECAPI_AVEncCommonMeanBitRate);
WINCAP_GUID(wincapLowLatency, STATIC_CODECAPI_AVLowLatencyMode);
WINCAP_GUID(wincapBPictures, STATIC_CODECAPI_AVEncMPVDefaultBPictureCount);
WINCAP_GUID(wincapGOPSize, STATIC_CODECAPI_AVEncMPVGOPSize);
WINCAP_GUID(wincapForceKeyFrame, STATIC_CODECAPI_AVEncVideoForceKeyFrame);

static HANDLE gOut;
static CRITICAL_SECTION gWriteLock;
static HANDLE gThread;
static volatile LONG gStop;

// Pacing: frames the host allows now, -1 for every frame until it paces.
static volatile LONG gCredit = -1;
static volatile LONG gBitRate;
static volatile LONG gBitRateChanged;
static volatile LONG gForceKeyframe;
static int gMaxShortSide;

// Desktop Duplication of the main monitor.
static ID3D11Device *gDevice;
static ID3D11DeviceContext *gContext;
static IDXGIOutputDuplication *gDup;
static ID3D11Texture2D *gStaging;
static int gStagingW, gStagingH;

// The desktop as BGRA, its size, and whether a frame of it waits to be encoded.
static uint8_t *gDesk;
static int gDeskW, gDeskH;
static int gPending;
static int64_t gChangedUS;
static int gSharpenLeft;

// The cursor: where, whether shown, and its shape.
static int gCursorX, gCursorY, gCursorVisible;
static uint8_t *gShape;
static UINT gShapeSize;
static DXGI_OUTDUPL_POINTER_SHAPE_INFO gShapeInfo;
static uint8_t *gUnder;  // the pixels the cursor covers, put back after encoding

// The encoder and its size.
static IMFTransform *gEnc;
static ICodecAPI *gCodec;
static int gEncW, gEncH;
static uint8_t *gSequenceHeader;  // SPS and PPS, Annex B, for keyframes without them
static UINT32 gSequenceHeaderSize;

// A still screen is encoded again this many times, kStillUS apart, so it
// sharpens past what the bitrate let a moving one have.
enum { kSharpenFrames = 3 };
static const int64_t kStillUS = 300000;

int64_t wincap_now_us(void) {
  static LARGE_INTEGER freq;
  LARGE_INTEGER now;
  if (!freq.QuadPart) QueryPerformanceFrequency(&freq);
  QueryPerformanceCounter(&now);
  return (int64_t)(now.QuadPart / freq.QuadPart * 1000000 + now.QuadPart % freq.QuadPart * 1000000 / freq.QuadPart);
}

// MARK: - Output

static void writeAll(const uint8_t *bytes, DWORD left) {
  while (left > 0) {
    DWORD n = 0;
    if (!WriteFile(gOut, bytes, left, &n, NULL) || n == 0) ExitProcess(0);  // the host closed the stream
    bytes += n;
    left -= n;
  }
}

static void putBig32(uint8_t *p, uint32_t v) {
  p[0] = v >> 24, p[1] = v >> 16, p[2] = v >> 8, p[3] = v;
}

// Finds the next Annex B start code at or after from; its size in *size.
static size_t startCode(const uint8_t *b, size_t n, size_t from, int *size) {
  for (size_t i = from; i + 3 <= n; i++) {
    if (b[i] == 0 && b[i + 1] == 0) {
      if (b[i + 2] == 1) return *size = 3, i;
      if (i + 4 <= n && b[i + 2] == 0 && b[i + 3] == 1) return *size = 4, i;
    }
  }
  *size = 0;
  return n;
}

// appendNALs appends Annex B's NAL units to out behind 4-byte lengths, and
// says whether it held an SPS and an IDR slice.
static size_t appendNALs(uint8_t *out, const uint8_t *b, size_t n, int *sps, int *idr) {
  size_t at = 0, written = 0;
  int size;
  size_t start = startCode(b, n, 0, &size);
  while (start < n) {
    size_t from = start + size;
    int next;
    size_t end = startCode(b, n, from, &next);
    size_t len = end - from;
    while (len > 0 && b[from + len - 1] == 0 && end < n) len--;  // trailing zero bytes before the next code
    if (len > 0) {
      int type = b[from] & 0x1f;
      if (type == 7) *sps = 1;
      if (type == 5) *idr = 1;
      putBig32(out + at, (uint32_t)len);
      memcpy(out + at + 4, b + from, len);
      at += 4 + len;
      written++;
    }
    start = end;
    size = next;
  }
  return at;
}

// emit writes one encoded access unit as a frame message: its length, its
// capture time, then its NAL units behind 4-byte lengths, the parameter sets
// first on a keyframe.
static void emit(IMFSample *sample, int64_t capturedUS) {
  IMFMediaBuffer *buffer = NULL;
  if (FAILED(IMFSample_ConvertToContiguousBuffer(sample, &buffer))) return;
  BYTE *data = NULL;
  DWORD length = 0;
  if (FAILED(IMFMediaBuffer_Lock(buffer, &data, NULL, &length))) {
    IMFMediaBuffer_Release(buffer);
    return;
  }
  size_t room = 12 + gSequenceHeaderSize + 8 + (size_t)length * 2 + 64;
  uint8_t *out = malloc(room);
  if (out) {
    int sps = 0, idr = 0, headerSPS = 0, headerIDR = 0;
    size_t at = 12;
    uint8_t *units = malloc((size_t)length * 2 + 64);
    size_t unitsLen = units ? appendNALs(units, data, length, &sps, &idr) : 0;
    if (idr && !sps && gSequenceHeader) {
      at += appendNALs(out + at, gSequenceHeader, gSequenceHeaderSize, &headerSPS, &headerIDR);
    }
    if (units) {
      memcpy(out + at, units, unitsLen);
      at += unitsLen;
      free(units);
    }
    if (unitsLen > 0) {
      putBig32(out, (uint32_t)(at - 4));
      uint64_t t = (uint64_t)capturedUS;
      putBig32(out + 4, (uint32_t)(t >> 32));
      putBig32(out + 8, (uint32_t)t);
      EnterCriticalSection(&gWriteLock);
      writeAll(out, (DWORD)at);
      LeaveCriticalSection(&gWriteLock);
    }
    free(out);
  }
  IMFMediaBuffer_Unlock(buffer);
  IMFMediaBuffer_Release(buffer);
}

// MARK: - Encoder

static void setCodecU32(const GUID *key, ULONG value) {
  if (!gCodec) return;
  VARIANT v;
  VariantInit(&v);
  v.vt = VT_UI4;
  v.ulVal = value;
  ICodecAPI_SetValue(gCodec, key, &v);
}

static void setCodecBool(const GUID *key, BOOL value) {
  if (!gCodec) return;
  VARIANT v;
  VariantInit(&v);
  v.vt = VT_BOOL;
  v.boolVal = value ? VARIANT_TRUE : VARIANT_FALSE;
  ICodecAPI_SetValue(gCodec, key, &v);
}

static void endEncoder(void) {
  if (gCodec) ICodecAPI_Release(gCodec), gCodec = NULL;
  if (gEnc) IMFTransform_Release(gEnc), gEnc = NULL;
  free(gSequenceHeader), gSequenceHeader = NULL, gSequenceHeaderSize = 0;
  gEncW = gEncH = 0;
}

// readSequenceHeader keeps the output type's SPS and PPS, if it has them.
static void readSequenceHeader(void) {
  IMFMediaType *type = NULL;
  if (FAILED(IMFTransform_GetOutputCurrentType(gEnc, 0, &type))) return;
  UINT32 size = 0;
  if (SUCCEEDED(IMFMediaType_GetBlobSize(type, &MF_MT_MPEG_SEQUENCE_HEADER, &size)) && size > 0) {
    free(gSequenceHeader);
    gSequenceHeader = malloc(size);
    if (gSequenceHeader && SUCCEEDED(IMFMediaType_GetBlob(type, &MF_MT_MPEG_SEQUENCE_HEADER, gSequenceHeader, size, NULL))) {
      gSequenceHeaderSize = size;
    } else {
      free(gSequenceHeader), gSequenceHeader = NULL, gSequenceHeaderSize = 0;
    }
  }
  IMFMediaType_Release(type);
}

static IMFTransform *findEncoder(void) {
  MFT_REGISTER_TYPE_INFO in = {MFMediaType_Video, MFVideoFormat_NV12};
  MFT_REGISTER_TYPE_INFO out = {MFMediaType_Video, MFVideoFormat_H264};
  IMFActivate **found = NULL;
  UINT32 count = 0;
  IMFTransform *enc = NULL;
  // Synchronous encoders only: a hardware one is asynchronous, for later.
  if (SUCCEEDED(MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_LOCALMFT | MFT_ENUM_FLAG_SORTANDFILTER,
                          &in, &out, &found, &count))) {
    for (UINT32 i = 0; i < count; i++) {
      if (!enc) IMFActivate_ActivateObject(found[i], &IID_IMFTransform, (void **)&enc);
      IMFActivate_Release(found[i]);
    }
    CoTaskMemFree(found);
  }
  if (!enc) CoCreateInstance(&wincapH264Encoder, NULL, CLSCTX_INPROC_SERVER, &IID_IMFTransform, (void **)&enc);
  return enc;
}

// ensureEncoder makes the encoder for a w×h picture, again when the size changes.
static BOOL ensureEncoder(int w, int h) {
  if (gEnc && gEncW == w && gEncH == h) return TRUE;
  endEncoder();
  gEnc = findEncoder();
  if (!gEnc) return FALSE;
  IMFTransform_QueryInterface(gEnc, &IID_ICodecAPI, (void **)&gCodec);
  // Rate control and latency go before the types, which some encoders fix them at.
  setCodecU32(&wincapRateControlMode, eAVEncCommonRateControlMode_CBR);
  setCodecU32(&wincapMeanBitRate, (ULONG)gBitRate);
  setCodecBool(&wincapLowLatency, TRUE);
  setCodecU32(&wincapBPictures, 0);
  setCodecU32(&wincapGOPSize, 600);

  IMFMediaType *out = NULL, *in = NULL;
  MFCreateMediaType(&out);
  IMFMediaType_SetGUID(out, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
  IMFMediaType_SetGUID(out, &MF_MT_SUBTYPE, &MFVideoFormat_H264);
  IMFMediaType_SetUINT32(out, &MF_MT_AVG_BITRATE, (UINT32)gBitRate);
  IMFMediaType_SetUINT64(out, &MF_MT_FRAME_SIZE, ((UINT64)w << 32) | (UINT32)h);
  IMFMediaType_SetUINT64(out, &MF_MT_FRAME_RATE, ((UINT64)60 << 32) | 1);
  IMFMediaType_SetUINT64(out, &MF_MT_PIXEL_ASPECT_RATIO, ((UINT64)1 << 32) | 1);
  IMFMediaType_SetUINT32(out, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  IMFMediaType_SetUINT32(out, &MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Main);
  IMFMediaType_SetUINT32(out, &MF_MT_YUV_MATRIX, MFVideoTransferMatrix_BT709);
  IMFMediaType_SetUINT32(out, &MF_MT_VIDEO_PRIMARIES, MFVideoPrimaries_BT709);
  IMFMediaType_SetUINT32(out, &MF_MT_TRANSFER_FUNCTION, MFVideoTransFunc_709);
  IMFMediaType_SetUINT32(out, &MF_MT_VIDEO_NOMINAL_RANGE, MFNominalRange_16_235);
  HRESULT hr = IMFTransform_SetOutputType(gEnc, 0, out, 0);
  IMFMediaType_Release(out);
  if (SUCCEEDED(hr)) {
    MFCreateMediaType(&in);
    IMFMediaType_SetGUID(in, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(in, &MF_MT_SUBTYPE, &MFVideoFormat_NV12);
    IMFMediaType_SetUINT64(in, &MF_MT_FRAME_SIZE, ((UINT64)w << 32) | (UINT32)h);
    IMFMediaType_SetUINT64(in, &MF_MT_FRAME_RATE, ((UINT64)60 << 32) | 1);
    IMFMediaType_SetUINT64(in, &MF_MT_PIXEL_ASPECT_RATIO, ((UINT64)1 << 32) | 1);
    IMFMediaType_SetUINT32(in, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    hr = IMFTransform_SetInputType(gEnc, 0, in, 0);
    IMFMediaType_Release(in);
  }
  if (FAILED(hr)) {
    endEncoder();
    return FALSE;
  }
  IMFTransform_ProcessMessage(gEnc, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
  IMFTransform_ProcessMessage(gEnc, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
  readSequenceHeader();
  gEncW = w, gEncH = h;
  gForceKeyframe = 1;
  return TRUE;
}

// toNV12 scales BGRA pixels to w×h (nearest, chroma averaged over each 2×2)
// and converts them to BT.709 video range NV12.
static void toNV12(const uint8_t *src, int sw, int sh, int stride, uint8_t *dst, int w, int h) {
  uint8_t *uv = dst + (size_t)w * h;
  for (int y = 0; y < h; y += 2) {
    const uint8_t *rows[2] = {src + (size_t)(y * sh / h) * stride, src + (size_t)((y + 1) * sh / h) * stride};
    for (int x = 0; x < w; x += 2) {
      int r = 0, g = 0, b = 0;
      for (int dy = 0; dy < 2; dy++) {
        for (int dx = 0; dx < 2; dx++) {
          const uint8_t *p = rows[dy] + (size_t)((x + dx) * sw / w) * 4;
          int pb = p[0], pg = p[1], pr = p[2];
          dst[(size_t)(y + dy) * w + x + dx] = (uint8_t)(16 + ((47 * pr + 157 * pg + 16 * pb + 128) >> 8));
          r += pr, g += pg, b += pb;
        }
      }
      r >>= 2, g >>= 2, b >>= 2;
      uint8_t *c = uv + (size_t)(y / 2) * w + x;
      c[0] = (uint8_t)(128 + ((-26 * r - 87 * g + 112 * b + 128) >> 8));
      c[1] = (uint8_t)(128 + ((112 * r - 102 * g - 10 * b + 128) >> 8));
    }
  }
}

// encodedSize is the picture's size for the encoder: the desktop's, scaled
// down to gMaxShortSide on its short side, both even.
static void encodedSize(int *w, int *h) {
  int sw = gDeskW, sh = gDeskH, shortSide = sw < sh ? sw : sh;
  if (gMaxShortSide > 0 && shortSide > gMaxShortSide) {
    sw = (int)((int64_t)sw * gMaxShortSide / shortSide);
    sh = (int)((int64_t)sh * gMaxShortSide / shortSide);
  }
  *w = sw & ~1, *h = sh & ~1;
}

static void drawCursor(int save);

// encodeDesk encodes the desktop as it is now, the cursor drawn on it.
// Spends a credit only on a frame that came out; one that didn't gives the
// host's pacer nothing to wait for.
static void encodeDesk(int64_t capturedUS) {
  int w, h;
  encodedSize(&w, &h);
  if (w < 2 || h < 2 || !ensureEncoder(w, h)) return;
  if (InterlockedExchange(&gBitRateChanged, 0)) setCodecU32(&wincapMeanBitRate, (ULONG)gBitRate);
  if (InterlockedExchange(&gForceKeyframe, 0)) setCodecU32(&wincapForceKeyFrame, 1);

  IMFSample *sample = NULL;
  IMFMediaBuffer *buffer = NULL;
  DWORD size = (DWORD)w * h * 3 / 2;
  if (FAILED(MFCreateSample(&sample)) || FAILED(MFCreateMemoryBuffer(size, &buffer))) goto done;
  BYTE *to = NULL;
  if (FAILED(IMFMediaBuffer_Lock(buffer, &to, NULL, NULL))) goto done;
  drawCursor(1);
  toNV12(gDesk, gDeskW, gDeskH, gDeskW * 4, to, w, h);
  drawCursor(0);
  IMFMediaBuffer_Unlock(buffer);
  IMFMediaBuffer_SetCurrentLength(buffer, size);
  IMFSample_AddBuffer(sample, buffer);
  IMFSample_SetSampleTime(sample, capturedUS * 10);
  IMFSample_SetSampleDuration(sample, 166667);
  if (FAILED(IMFTransform_ProcessInput(gEnc, 0, sample, 0))) goto done;

  int made = 0;
  for (;;) {
    MFT_OUTPUT_STREAM_INFO info = {0};
    IMFTransform_GetOutputStreamInfo(gEnc, 0, &info);
    MFT_OUTPUT_DATA_BUFFER output = {0};
    IMFSample *outSample = NULL;
    if (!(info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES | MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))) {
      IMFMediaBuffer *outBuffer = NULL;
      DWORD cb = info.cbSize > size ? info.cbSize : size;
      MFCreateSample(&outSample);
      MFCreateMemoryBuffer(cb, &outBuffer);
      IMFSample_AddBuffer(outSample, outBuffer);
      IMFMediaBuffer_Release(outBuffer);
      output.pSample = outSample;
    }
    DWORD status = 0;
    HRESULT hr = IMFTransform_ProcessOutput(gEnc, 0, 1, &output, &status);
    if (output.pEvents) IMFCollection_Release(output.pEvents);
    if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {
      IMFMediaType *type = NULL;
      if (SUCCEEDED(IMFTransform_GetOutputAvailableType(gEnc, 0, 0, &type))) {
        IMFTransform_SetOutputType(gEnc, 0, type, 0);
        IMFMediaType_Release(type);
        readSequenceHeader();
      }
      if (outSample) IMFSample_Release(outSample);
      continue;
    }
    if (SUCCEEDED(hr) && output.pSample) {
      emit(output.pSample, capturedUS);
      made = 1;
    }
    if (output.pSample && output.pSample != outSample) IMFSample_Release(output.pSample);
    if (outSample) IMFSample_Release(outSample);
    if (FAILED(hr)) break;
  }
  if (made) {
    LONG credit = gCredit;
    if (credit > 0) InterlockedCompareExchange(&gCredit, credit - 1, credit);
  }
done:
  if (buffer) IMFMediaBuffer_Release(buffer);
  if (sample) IMFSample_Release(sample);
}

// MARK: - Cursor

// drawCursor draws the cursor into gDesk, saving the pixels under it, or
// with save 0 puts them back.
static void drawCursor(int save) {
  if (!gCursorVisible || !gShape || !gDesk) return;
  int w = (int)gShapeInfo.Width, h = (int)gShapeInfo.Height;
  if (gShapeInfo.Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME) h /= 2;
  int x0 = gCursorX, y0 = gCursorY;
  int left = x0 < 0 ? -x0 : 0, top = y0 < 0 ? -y0 : 0;
  int right = x0 + w > gDeskW ? gDeskW - x0 : w, bottom = y0 + h > gDeskH ? gDeskH - y0 : h;
  if (left >= right || top >= bottom) return;
  if (!save) {
    if (!gUnder) return;
    for (int y = top; y < bottom; y++) {
      memcpy(gDesk + ((size_t)(y0 + y) * gDeskW + x0 + left) * 4, gUnder + ((size_t)y * w + left) * 4, (size_t)(right - left) * 4);
    }
    return;
  }
  free(gUnder);
  gUnder = malloc((size_t)w * h * 4);
  if (!gUnder) return;
  for (int y = top; y < bottom; y++) {
    memcpy(gUnder + ((size_t)y * w + left) * 4, gDesk + ((size_t)(y0 + y) * gDeskW + x0 + left) * 4, (size_t)(right - left) * 4);
  }
  UINT pitch = gShapeInfo.Pitch;
  for (int y = top; y < bottom; y++) {
    uint8_t *d = gDesk + ((size_t)(y0 + y) * gDeskW + x0) * 4;
    for (int x = left; x < right; x++) {
      uint8_t *p = d + (size_t)x * 4;
      switch (gShapeInfo.Type) {
        case DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR: {
          const uint8_t *s = gShape + (size_t)y * pitch + (size_t)x * 4;
          int a = s[3];
          for (int c = 0; c < 3; c++) p[c] = (uint8_t)((s[c] * a + p[c] * (255 - a) + 127) / 255);
          break;
        }
        case DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR: {
          const uint8_t *s = gShape + (size_t)y * pitch + (size_t)x * 4;
          if (s[3] == 0) {
            p[0] = s[0], p[1] = s[1], p[2] = s[2];
          } else {
            p[0] ^= s[0], p[1] ^= s[1], p[2] ^= s[2];
          }
          break;
        }
        case DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME: {
          int bit = 0x80 >> (x % 8);
          int and = gShape[(size_t)y * pitch + x / 8] & bit;
          int xor = gShape[(size_t)(y + h) * pitch + x / 8] & bit;
          if (!and) p[0] = p[1] = p[2] = 0;
          if (xor) p[0] ^= 0xff, p[1] ^= 0xff, p[2] ^= 0xff;
          break;
        }
      }
    }
  }
}

// MARK: - Desktop Duplication

static void endDuplication(void) {
  if (gDup) IDXGIOutputDuplication_Release(gDup), gDup = NULL;
}

static void endDevice(void) {
  endDuplication();
  if (gStaging) ID3D11Texture2D_Release(gStaging), gStaging = NULL;
  if (gContext) ID3D11DeviceContext_Release(gContext), gContext = NULL;
  if (gDevice) ID3D11Device_Release(gDevice), gDevice = NULL;
}

// openDuplication duplicates the main monitor (the one at the desktop's
// origin), on a device made on its own adapter, as Desktop Duplication needs.
static HRESULT openDuplication(void) {
  IDXGIFactory1 *factory = NULL;
  HRESULT hr = CreateDXGIFactory1(&IID_IDXGIFactory1, (void **)&factory);
  if (FAILED(hr)) return hr;
  IDXGIAdapter1 *adapter = NULL;
  IDXGIOutput *output = NULL;
  hr = DXGI_ERROR_NOT_FOUND;
  for (UINT a = 0; !output && IDXGIFactory1_EnumAdapters1(factory, a, &adapter) != DXGI_ERROR_NOT_FOUND; a++) {
    IDXGIOutput *o = NULL;
    for (UINT i = 0; !output && IDXGIAdapter1_EnumOutputs(adapter, i, &o) != DXGI_ERROR_NOT_FOUND; i++) {
      DXGI_OUTPUT_DESC desc;
      IDXGIOutput_GetDesc(o, &desc);
      if (desc.AttachedToDesktop && desc.DesktopCoordinates.left == 0 && desc.DesktopCoordinates.top == 0) {
        output = o;
      } else {
        IDXGIOutput_Release(o);
      }
    }
    if (!output) IDXGIAdapter1_Release(adapter), adapter = NULL;
  }
  IDXGIFactory1_Release(factory);
  if (!output) return hr;
  if (!gDevice) {
    D3D_FEATURE_LEVEL level;
    hr = D3D11CreateDevice((IDXGIAdapter *)adapter, D3D_DRIVER_TYPE_UNKNOWN, NULL, D3D11_CREATE_DEVICE_BGRA_SUPPORT, NULL, 0,
                           D3D11_SDK_VERSION, &gDevice, &level, &gContext);
  } else {
    hr = S_OK;
  }
  if (SUCCEEDED(hr)) {
    IDXGIOutput1 *output1 = NULL;
    hr = IDXGIOutput_QueryInterface(output, &IID_IDXGIOutput1, (void **)&output1);
    if (SUCCEEDED(hr)) {
      hr = IDXGIOutput1_DuplicateOutput(output1, (IUnknown *)gDevice, &gDup);
      IDXGIOutput1_Release(output1);
    }
  }
  IDXGIOutput_Release(output);
  IDXGIAdapter1_Release(adapter);
  return hr;
}

// copyFrame copies the duplicated frame into gDesk through a staging texture.
static BOOL copyFrame(IDXGIResource *resource) {
  ID3D11Texture2D *texture = NULL;
  if (FAILED(IDXGIResource_QueryInterface(resource, &IID_ID3D11Texture2D, (void **)&texture))) return FALSE;
  D3D11_TEXTURE2D_DESC desc;
  ID3D11Texture2D_GetDesc(texture, &desc);
  if (!gStaging || gStagingW != (int)desc.Width || gStagingH != (int)desc.Height) {
    if (gStaging) ID3D11Texture2D_Release(gStaging), gStaging = NULL;
    D3D11_TEXTURE2D_DESC staging = desc;
    staging.Usage = D3D11_USAGE_STAGING;
    staging.BindFlags = 0;
    staging.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    staging.MiscFlags = 0;
    staging.MipLevels = 1;
    staging.ArraySize = 1;
    staging.SampleDesc.Count = 1;
    if (FAILED(ID3D11Device_CreateTexture2D(gDevice, &staging, NULL, &gStaging))) {
      ID3D11Texture2D_Release(texture);
      return FALSE;
    }
    gStagingW = (int)desc.Width, gStagingH = (int)desc.Height;
  }
  ID3D11DeviceContext_CopyResource(gContext, (ID3D11Resource *)gStaging, (ID3D11Resource *)texture);
  ID3D11Texture2D_Release(texture);
  D3D11_MAPPED_SUBRESOURCE mapped;
  if (FAILED(ID3D11DeviceContext_Map(gContext, (ID3D11Resource *)gStaging, 0, D3D11_MAP_READ, 0, &mapped))) return FALSE;
  if (gDeskW != gStagingW || gDeskH != gStagingH) {
    free(gDesk);
    gDesk = malloc((size_t)gStagingW * gStagingH * 4);
    gDeskW = gStagingW, gDeskH = gStagingH;
  }
  if (gDesk) {
    for (int y = 0; y < gDeskH; y++) {
      memcpy(gDesk + (size_t)y * gDeskW * 4, (uint8_t *)mapped.pData + (size_t)y * mapped.RowPitch, (size_t)gDeskW * 4);
    }
  }
  ID3D11DeviceContext_Unmap(gContext, (ID3D11Resource *)gStaging, 0);
  return gDesk != NULL;
}

// readPointer takes the frame's cursor position and shape; TRUE if either changed.
static BOOL readPointer(const DXGI_OUTDUPL_FRAME_INFO *info) {
  BOOL changed = FALSE;
  if (info->LastMouseUpdateTime.QuadPart != 0) {
    gCursorVisible = info->PointerPosition.Visible;
    gCursorX = info->PointerPosition.Position.x;
    gCursorY = info->PointerPosition.Position.y;
    changed = TRUE;
  }
  if (info->PointerShapeBufferSize > 0) {
    if (info->PointerShapeBufferSize > gShapeSize) {
      free(gShape);
      gShape = malloc(info->PointerShapeBufferSize);
      gShapeSize = gShape ? info->PointerShapeBufferSize : 0;
    }
    UINT needed = 0;
    if (gShape && SUCCEEDED(IDXGIOutputDuplication_GetFramePointerShape(gDup, gShapeSize, gShape, &needed, &gShapeInfo))) {
      changed = TRUE;
    }
  }
  return changed;
}

// MARK: - The capture thread

static DWORD WINAPI captureLoop(LPVOID unused) {
  (void)unused;
  CoInitializeEx(NULL, COINIT_MULTITHREADED);
  while (!gStop) {
    if (!gDup && FAILED(openDuplication())) {
      // The secure desktop (UAC, the lock screen) or a session switch: keep
      // the last picture and try again.
      Sleep(250);
      continue;
    }
    DXGI_OUTDUPL_FRAME_INFO info;
    IDXGIResource *resource = NULL;
    HRESULT hr = IDXGIOutputDuplication_AcquireNextFrame(gDup, 16, &info, &resource);
    int64_t now = wincap_now_us();
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
      if (!gDesk) continue;
      if (gForceKeyframe) gPending = 1;
      if (gSharpenLeft > 0 && !gPending && now - gChangedUS > kStillUS * (kSharpenFrames + 1 - gSharpenLeft)) {
        gSharpenLeft--;
        gPending = 1;
      }
      if (gPending && gCredit != 0) {
        gPending = 0;
        encodeDesk(now);
      }
      continue;
    }
    if (FAILED(hr)) {
      endDuplication();
      continue;
    }
    BOOL changed = readPointer(&info);
    if (info.LastPresentTime.QuadPart != 0 || info.AccumulatedFrames > 0) {
      changed = copyFrame(resource) || changed;
    }
    IDXGIResource_Release(resource);
    IDXGIOutputDuplication_ReleaseFrame(gDup);
    if (changed && gDesk) {
      gChangedUS = now;
      gSharpenLeft = kSharpenFrames;
      gPending = 1;
    }
    if (gPending && gDesk && gCredit != 0) {
      gPending = 0;
      encodeDesk(now);
    }
  }
  endEncoder();
  endDevice();
  CoUninitialize();
  return 0;
}

int wincap_start(uintptr_t out, int maxShortSide, int bitRate, char *err, int errLen) {
  // Desktop Duplication and SendInput in physical pixels.
  HMODULE user32 = GetModuleHandleW(L"user32.dll");
  typedef BOOL(WINAPI * setAwareness)(HANDLE);
  setAwareness aware = user32 ? (setAwareness)(void *)GetProcAddress(user32, "SetProcessDpiAwarenessContext") : NULL;
  if (aware) aware((HANDLE)-4);  // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2

  gOut = (HANDLE)out;
  gMaxShortSide = maxShortSide;
  gBitRate = bitRate;
  InitializeCriticalSection(&gWriteLock);
  if (FAILED(CoInitializeEx(NULL, COINIT_MULTITHREADED)) || FAILED(MFStartup(MF_VERSION, MFSTARTUP_LITE))) {
    snprintf(err, errLen, "Media Foundation isn't available on this PC");
    return 1;
  }
  HRESULT hr = openDuplication();
  if (FAILED(hr)) {
    if (hr == E_ACCESSDENIED) {
      snprintf(err, errLen, "Windows isn't showing the desktop: it's locked or on a sign-in prompt");
    } else if (hr == DXGI_ERROR_UNSUPPORTED) {
      snprintf(err, errLen, "This PC's graphics can't share the screen (0x%08lx)", (unsigned long)hr);
    } else {
      snprintf(err, errLen, "This PC's screen isn't available: is the Windows session signed in and connected? (0x%08lx)",
               (unsigned long)hr);
    }
    endDevice();
    return 1;
  }
  IMFTransform *probe = findEncoder();
  if (!probe) {
    snprintf(err, errLen, "This PC has no H.264 encoder (Media Foundation)");
    endDevice();
    return 1;
  }
  IMFTransform_Release(probe);
  gThread = CreateThread(NULL, 0, captureLoop, NULL, 0, NULL);
  return gThread ? 0 : 1;
}

void wincap_stop(void) {
  gStop = 1;
  if (gThread) WaitForSingleObject(gThread, 2000), CloseHandle(gThread), gThread = NULL;
}

void wincap_credit(int frames) {
  if (frames >= 0) InterlockedExchange(&gCredit, frames);
}

void wincap_bitrate(int bitsPerSecond) {
  if (bitsPerSecond <= 0) return;
  InterlockedExchange(&gBitRate, bitsPerSecond);
  InterlockedExchange(&gBitRateChanged, 1);
}

void wincap_keyframe(void) { InterlockedExchange(&gForceKeyframe, 1); }

// MARK: - Input

static void sendMouse(DWORD flags, LONG dx, LONG dy, DWORD data) {
  INPUT in = {0};
  in.type = INPUT_MOUSE;
  in.mi.dx = dx, in.mi.dy = dy;
  in.mi.mouseData = data;
  in.mi.dwFlags = flags;
  SendInput(1, &in, sizeof in);
}

void wincap_pointer(double x, double y) {
  LONG dx = (LONG)(x * 65535.0 + 0.5), dy = (LONG)(y * 65535.0 + 0.5);
  sendMouse(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE, dx, dy, 0);
}

void wincap_button(int button, int down) {
  static const DWORD downs[] = {MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_RIGHTDOWN};
  static const DWORD ups[] = {MOUSEEVENTF_LEFTUP, MOUSEEVENTF_MIDDLEUP, MOUSEEVENTF_RIGHTUP};
  if (button < 0 || button > 2) return;
  sendMouse(down ? downs[button] : ups[button], 0, 0, 0);
}

void wincap_wheel(int vertical, int delta) {
  sendMouse(vertical ? MOUSEEVENTF_WHEEL : MOUSEEVENTF_HWHEEL, 0, 0, (DWORD)delta);
}

void wincap_vk(int vk, int down, int extended) {
  INPUT in = {0};
  in.type = INPUT_KEYBOARD;
  in.ki.wVk = (WORD)vk;
  in.ki.wScan = (WORD)MapVirtualKeyW((UINT)vk, MAPVK_VK_TO_VSC);
  in.ki.dwFlags = (down ? 0 : KEYEVENTF_KEYUP) | (extended ? KEYEVENTF_EXTENDEDKEY : 0);
  SendInput(1, &in, sizeof in);
}

void wincap_unicode(uint16_t unit) {
  INPUT in[2] = {0};
  in[0].type = in[1].type = INPUT_KEYBOARD;
  in[0].ki.wScan = in[1].ki.wScan = unit;
  in[0].ki.dwFlags = KEYEVENTF_UNICODE;
  in[1].ki.dwFlags = KEYEVENTF_UNICODE | KEYEVENTF_KEYUP;
  SendInput(2, in, sizeof in[0]);
}

int wincap_vk_scan(uint16_t ch) { return (int)VkKeyScanW((WCHAR)ch); }
