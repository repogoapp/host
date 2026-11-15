//go:build darwin && cgo && !simcap_uhid

package capture

/*
#include <CoreFoundation/CoreFoundation.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>

// Stands in for the Swift bridge this build doesn't link.
void *simcap_uhid_capability(const char *udid, const char *metadata, const char *witness) {
  fprintf(stderr, "simcap: no touch input: this build has no Universal HID bridge (build it with -tags simcap_uhid)\n");
  return NULL;
}

bool simcap_uhid_display(const char *udid, uint32_t *screen, int32_t *rotation) { return false; }

bool simcap_uhid_control(void *capability, void *data) {
  if (data) CFRelease(data);
  return false;
}
*/
import "C"

// UniversalHID reports whether this build links the Swift bridge that
// iOS 27 simulators' touches and keys go through (scripts/build-simcap-uhid.sh,
// then -tags simcap_uhid).
const UniversalHID = false
