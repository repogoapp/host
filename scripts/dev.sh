#!/usr/bin/env bash
# The fast loop: restart the host, put a fresh build on the phone, watch the log.
#
# One command because a phone talking to a stale host is indistinguishable
# from a logic bug.
#
#   scripts/dev.sh           rebuild + redeploy both
#   scripts/dev.sh --host    host only, skip the ~60s iOS build
#   scripts/dev.sh --pair    also print a fresh pairing QR
set -euo pipefail

# Link the Rust transcript parser when it has been built (`make native`);
# otherwise the pure-Go parser is used and nothing here changes.
GO_TAGS=""
[ -f "$(dirname "$0")/../internal/session/native/lib/librepogo_import.a" ] && GO_TAGS="-tags native"

cd "$(dirname "$0")/.."

PORT="${REPOGO_PORT:-51999}"
LOG="${TMPDIR:-/tmp}/repogo-host.log"
BIN="${TMPDIR:-/tmp}/repogo-host"

DEVICE_UDID="00008150-000E55060A84401C"           # xcodebuild destination
DEVICE_ID="198E2642-2072-5D79-9804-C32CFC14A18D"  # devicectl
BUNDLE_ID="app.repogo"
IOS_PROJECT="../swift/repogo/repogo.xcodeproj"
DERIVED="build/repogo-ios"

host_only=false
pairing=false
host_args=()
for arg in "$@"; do
  case "$arg" in
    --host|--host-only) host_only=true ;;
    -pair|--pair) pairing=true ;;
    *) host_args+=("$arg") ;;
  esac
done

say() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

HOST_PID=""
TAIL_PID=""
cleanup() {
  [ -n "$TAIL_PID" ] && kill "$TAIL_PID" 2>/dev/null || true
  [ -n "$HOST_PID" ] && kill "$HOST_PID" 2>/dev/null || true
}

stop_host() {
  if pids=$(lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null) && [ -n "$pids" ]; then
    # SIGTERM, not -9: the host closes its sockets on the way out, so a
    # connected phone sees a real disconnect instead of hanging on a dead one.
    kill $pids 2>/dev/null || true
    for _ in $(seq 1 30); do
      lsof -t -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1 || break
      sleep 0.1
    done
    echo "  stopped (pid $(echo "$pids" | tr '\n' ' '))"
  else
    echo "  nothing was running on :$PORT"
  fi
}

start_host() {
  : > "$LOG"
  "$BIN" serve -port "$PORT" "${host_args[@]+"${host_args[@]}"}" >>"$LOG" 2>&1 &
  HOST_PID=$!
  # Kill it on Ctrl-C, so quitting the log tail does not leave a stray host
  # holding the port for the next run.
  trap cleanup EXIT INT TERM HUP

  for _ in $(seq 1 100); do
    grep -q "runtime listening" "$LOG" && break
    kill -0 $HOST_PID 2>/dev/null || { echo "host exited:"; cat "$LOG"; exit 1; }
    sleep 0.1
  done
  grep -E "device identity|runtime listening" "$LOG" | sed 's/^/  /' || true
}

deploy_ios() {
  say "Building and installing RepoGo"
  source ../../scripts/ios/dev-logs-base.sh
  xcodebuild -project "$IOS_PROJECT" \
    -scheme repogo -configuration Debug \
    -destination "id=$DEVICE_UDID" -derivedDataPath "$DERIVED" \
    -allowProvisioningUpdates \
    REPOGO_DEV_LOGS_BASE="$DEV_LOGS_BASE" \
    REPOGO_LOG_SERVER_BASE="$LOG_SERVER_BASE" \
    build -quiet || return $?

  xcrun devicectl device install app --device "$DEVICE_ID" \
    "$DERIVED/Build/Products/Debug-iphoneos/RepoGo.app" >/dev/null || return $?
  xcrun devicectl device process launch --device "$DEVICE_ID" "$BUNDLE_ID" >/dev/null || return $?
  echo "  launched on device"
}

# Compile BEFORE touching anything running. A broken build should cost three
# seconds, not a killed host and a half-finished deploy.
say "Building host"
go build $GO_TAGS -o "$BIN" ./repogo

if [ "$pairing" = true ] && [ "$host_only" = false ]; then
  # Pairing codes live two minutes and the iOS build takes about one, so the
  # QR has to be minted AFTER the slow step or it is half expired on arrival.
  deploy_ios
  say "Stopping the old host"; "$BIN" stop; stop_host
  say "Starting host"; start_host
else
  # Normal loop: bring the host up first so the app connects to the new one the
  # moment it launches, rather than to the build it is replacing.
  say "Stopping the old host"; "$BIN" stop; stop_host
  say "Starting host"; start_host
  # A failed phone deploy must not take the host with it: under `set -e` the
  # EXIT trap would SIGTERM it, and its clean "shutting down" reads as a crash.
  if [ "$host_only" = false ] && ! deploy_ios; then
    printf '\n  iOS deploy FAILED (see above) — host is still running on :%s\n' "$PORT"
  fi
fi

if [ "$pairing" = true ]; then "$BIN" pair; fi

say "Host log (Ctrl-C to stop everything)"
# Tail in the background and wait on the host: bash defers a trap until the
# foreground child returns, so a foreground tail would swallow Ctrl-C and leave
# the host holding the port.
tail -n +1 -f "$LOG" &
TAIL_PID=$!
# Say so when the host dies on its own; a silent return to the prompt is
# indistinguishable from Ctrl-C.
status=0; wait "$HOST_PID" || status=$?
HOST_PID=""
if [ "$status" -ne 0 ]; then
  say "Host exited with status $status"
  exit "$status"
fi
