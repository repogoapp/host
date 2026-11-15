// Forwards the Swift ABI unchanged, swiftself and indirect results included,
// to CoreDevice's functions; uhid/Bridge.swift declares these names with
// @_silgen_name. SSCoreDeviceInitialize checks every target first.
// Adapted from serve-sim's CoreDeviceTrampolines.S (Apache-2.0).
#if defined(__aarch64__)
.macro FORWARD name, target
.text
.p2align 2
.globl _\name
_\name:
    adrp x16, _\target@PAGE
    ldr x16, [x16, _\target@PAGEOFF]
    br x16
.endmacro
#elif defined(__x86_64__)
.macro FORWARD name, target
.text
.globl _\name
_\name:
    jmp *_\target(%rip)
.endmacro
#endif

FORWARD SSCDShared, SSCDSharedTarget
FORWARD SSCDConnection, SSCDConnectionTarget
FORWARD SSCDCreateManager, SSCDCreateManagerTarget
FORWARD SSCDAllDevices, SSCDAllDevicesTarget
FORWARD SSCDInitialized, SSCDInitializedTarget
FORWARD SSCDIdentifier, SSCDIdentifierTarget
FORWARD SSCDVisibilityAllCases, SSCDVisibilityAllCasesTarget
FORWARD SSCDImplementation, SSCDImplementationTarget
FORWARD SSCDDisplayInfo, SSCDDisplayInfoTarget

// The async function pointers Swift calls through: each one's offset, and
// the callee's context size, copied from CoreDevice's own.
.data
.p2align 2
.globl _SSCDImplementationTu
_SSCDImplementationTu:
.long _SSCDImplementation - _SSCDImplementationTu
.long 0
.globl _SSCDDisplayInfoTu
_SSCDDisplayInfoTu:
.long _SSCDDisplayInfo - _SSCDDisplayInfoTu
.long 0
