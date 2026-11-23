#!/usr/bin/env bash
# Builds repogo-ax.dex from RepoGoAx.java, for the host to embed. Run after
# changing the Java; commit both. Needs the Android SDK (platform 37 and
# build-tools) and a JDK 17 or later (Android Studio's).
set -euo pipefail
cd "$(dirname "$0")"
SDK="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
JDK="${JAVA_HOME:-/Applications/Android Studio.app/Contents/jbr/Contents/Home}"
PLATFORM="$SDK/platforms/android-37.0/android.jar"
D8="$(ls -d "$SDK"/build-tools/*/ | sort -V | tail -1)d8"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
"$JDK/bin/javac" --release 17 -cp "$PLATFORM" -d "$work/classes" RepoGoAx.java
JAVA_HOME="$JDK" "$D8" --min-api 30 --lib "$PLATFORM" --output "$work" "$work"/classes/*.class
cp "$work/classes.dex" repogo-ax.dex
echo "built repogo-ax.dex ($(wc -c < repogo-ax.dex) bytes)"
