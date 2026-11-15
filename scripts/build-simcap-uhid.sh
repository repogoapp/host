#!/bin/sh
# Builds the Swift half of simcap's Universal HID (iOS 27 simulator touch and
# keys) into a static library per architecture. A macOS build that links it
# then passes -tags simcap_uhid; one without still builds, minus iOS 27 input.
# Needs Xcode. Usage: scripts/build-simcap-uhid.sh [arm64|x86_64 ...]
set -eu
dir="$(cd "$(dirname "$0")/../internal/screens/capture/uhid" && pwd)"
archs="${*:-arm64 x86_64}"
for arch in $archs; do
  mkdir -p "$dir/build/$arch"
  xcrun swiftc -parse-as-library -emit-library -static -O -swift-version 5 \
    -target "$arch-apple-macos14" -module-name SimcapUHIDBridge -I "$dir" \
    "$dir/Bridge.swift" -o "$dir/build/$arch/libsimcapuhid.a"
  echo "built $dir/build/$arch/libsimcapuhid.a"
done
