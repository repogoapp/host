#!/usr/bin/env bash
# Rebuild the host and restart it in the background, then return.
#
# `dev.sh --host` does the same but stays attached to the log; this one exits
# once the new host is listening, so it can run from an action button.
#
#   scripts/restart.sh        rebuild + restart
#   scripts/restart.sh -v     extra args go to `serve`
set -euo pipefail

# Run from an action, this script is the host's own child: the host kills the
# action's process group when it shuts down, taking this script with it. So
# re-run in a new session and stream its output here until it finishes.
if [ -z "${REPOGO_RESTART_DETACHED:-}" ]; then
  out="${TMPDIR:-/tmp}/repogo-restart.log"
  : > "$out"
  REPOGO_RESTART_DETACHED=1 nohup perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' \
    bash "$0" "$@" >>"$out" 2>&1 </dev/null &
  child=$!
  tail -n +1 -f "$out" &
  tail_pid=$!
  status=0; wait "$child" || status=$?
  sleep 0.2; kill "$tail_pid" 2>/dev/null || true
  exit "$status"
fi
# The new host passes its environment to the actions it runs; left set, the
# next restart from an action would skip the detach above.
unset REPOGO_RESTART_DETACHED

cd "$(dirname "$0")/.."

GO_TAGS=""
[ -f internal/session/native/lib/librepogo_import.a ] && GO_TAGS="-tags native"

PORT="${REPOGO_PORT:-51999}"
LOG="${TMPDIR:-/tmp}/repogo-host.log"
BIN="${TMPDIR:-/tmp}/repogo-host"

say() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

# Compile before touching the running host, so a broken build leaves it up.
say "Building host"
go build $GO_TAGS -o "$BIN" ./repogo

say "Stopping the old host"
"$BIN" stop
if pids=$(lsof -t -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null) && [ -n "$pids" ]; then
  kill $pids 2>/dev/null || true
  for _ in $(seq 1 30); do
    lsof -t -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 0.1
  done
  echo "  stopped (pid $(echo "$pids" | tr '\n' ' '))"
else
  echo "  nothing was running on :$PORT"
fi

say "Starting host"
: > "$LOG"
# Dev only: the host also sends its logs to apps/log-server (bun run
# log-server), beside the phone's. A release host never sets this.
export REPOGO_LOG_SERVER="${REPOGO_LOG_SERVER:-http://127.0.0.1:${REPOGO_LOG_SERVER_PORT:-3939}}"
# Dev only: Go profiles on loopback, for bench/monitor and `go tool pprof`.
export REPOGO_PPROF="${REPOGO_PPROF:-127.0.0.1:6061}"
# A new session, so the host outlives this script and whatever launched it.
nohup perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' \
  "$BIN" serve -port "$PORT" "$@" >>"$LOG" 2>&1 </dev/null &
HOST_PID=$!

for _ in $(seq 1 100); do
  grep -q "runtime listening" "$LOG" && break
  kill -0 "$HOST_PID" 2>/dev/null || { echo "host exited:"; cat "$LOG"; exit 1; }
  sleep 0.1
done
if ! grep -q "runtime listening" "$LOG"; then
  echo "host did not report listening within 10s; log: $LOG"
  exit 1
fi
grep -E "device identity|runtime listening" "$LOG" | sed 's/^/  /' || true
echo "  running as pid $HOST_PID; log: $LOG"
