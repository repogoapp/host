#!/usr/bin/env bash
# Stop the host before removing the cache so it cannot write during deletion.
set -euo pipefail

cache_dir="${REPOGO_HOME:-$HOME/.repogo}"
db="$cache_dir/chats.db"
port="${REPOGO_PORT:-51999}"
service="gui/$(id -u)/app.repogo.host"

pids=$({
  lsof -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true
  lsof -t "$db" "$db-wal" "$db-shm" 2>/dev/null || true
} | sort -un)

if launchctl print "$service" >/dev/null 2>&1; then
  echo "Stopping the host launch agent"
  launchctl bootout "$service"
fi

for pid in $pids; do
  command=$(ps -p "$pid" -o comm=) || continue
  case "${command##*/}" in
    repogo|repogo-host) ;;
    *) echo "Process $pid ($command) is using the host port or cache; aborting." >&2; exit 1 ;;
  esac
  echo "Stopping host $pid"
  kill "$pid" 2>/dev/null || true
  for _ in {1..50}; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid"
  fi
done

if lsof -t "$db" "$db-wal" "$db-shm" 2>/dev/null | grep -q .; then
  echo "The cache is still open; aborting." >&2
  exit 1
fi

rm -f -- "$db" "$db-wal" "$db-shm" "$db-journal"
echo "Deleted $db. The next host start will rebuild the cache."
