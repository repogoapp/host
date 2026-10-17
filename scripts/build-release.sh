#!/usr/bin/env bash
# Builds the host for every platform a release ships, from one Mac: the two
# macOS binaries with the system's clang, the two Linux ones static against
# musl with zig as the C compiler (cgo stays on for SQLite). Writes
# dist/repogo_<os>_<arch>. RELEASE_VERSION and RELEASE_REPOSITORY are stamped in.
set -euo pipefail
cd "$(dirname "$0")/.."

test -n "${RELEASE_VERSION:-}" || { echo "RELEASE_VERSION is not set" >&2; exit 1; }
test -n "${RELEASE_REPOSITORY:-}" || { echo "RELEASE_REPOSITORY is not set" >&2; exit 1; }
command -v zig >/dev/null || { echo "zig is not installed (brew install zig)" >&2; exit 1; }

module=github.com/repogoapp/host/internal/release
stamp="-X $module.Version=$RELEASE_VERSION -X $module.Repository=$RELEASE_REPOSITORY"
rm -rf dist && mkdir -p dist

for arch in arm64 amd64; do
  echo "darwin/$arch"
  CGO_ENABLED=1 GOOS=darwin GOARCH=$arch \
    go build -trimpath -ldflags "-s -w $stamp" -o "dist/repogo_darwin_$arch" ./repogo
done

# netgo/osusergo keep name and user lookups off libc's NSS, which a static
# binary can't load; one binary then runs on any Linux whatever its glibc.
for arch in arm64 amd64; do
  target=$([ "$arch" = arm64 ] && echo aarch64-linux-musl || echo x86_64-linux-musl)
  echo "linux/$arch"
  CC="zig cc -target $target" CGO_ENABLED=1 GOOS=linux GOARCH=$arch \
    go build -trimpath -tags netgo,osusergo,sqlite_omit_load_extension \
    -ldflags "-s -w -linkmode external -extldflags -static $stamp" \
    -o "dist/repogo_linux_$arch" ./repogo
done

ls -l dist
