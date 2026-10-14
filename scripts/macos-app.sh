#!/usr/bin/env bash
# Wrap a macOS host binary in RepoGo Host.app, so System Settings, Activity
# Monitor and privacy prompts name the host and show its icon. The release
# workflow signs the bundle this makes.
#
#   scripts/macos-app.sh <binary> <version> <out/RepoGo Host.app>
set -euo pipefail

binary=$1
version=$2
app=$3
packaging="$(cd "$(dirname "$0")/.." && pwd)/packaging/macos"

rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$binary" "$app/Contents/MacOS/repogo"
chmod 755 "$app/Contents/MacOS/repogo"
sed "s/{{VERSION}}/$version/g" "$packaging/Info.plist" > "$app/Contents/Info.plist"
cp "$packaging/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
plutil -lint "$app/Contents/Info.plist" >/dev/null
