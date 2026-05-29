#!/usr/bin/env bash
# Stops everything up.sh started: host-agent, scheduler, and the redis
# container. Safe to run twice — missing PIDs / containers are ignored.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$REPO_ROOT/run"

stop_pidfile() {
  local name=$1 pidfile=$2 use_sudo=${3:-false}
  if [[ ! -f "$pidfile" ]]; then
    echo "    $name: no pidfile"
    return
  fi
  local pid
  pid=$(cat "$pidfile")
  if [[ -z "$pid" ]] || ! kill -0 "$pid" 2>/dev/null; then
    echo "    $name: pid $pid not running"
    rm -f "$pidfile"
    return
  fi
  if [[ "$use_sudo" == "true" ]]; then
    sudo kill "$pid" 2>/dev/null || true
  else
    kill "$pid" 2>/dev/null || true
  fi
  echo "    $name: stopped pid $pid"
  rm -f "$pidfile"
}

echo "==> stopping host-agent"
stop_pidfile "host-agent" "$RUN_DIR/agent.pid" true

echo "==> stopping scheduler"
stop_pidfile "scheduler" "$RUN_DIR/scheduler.pid" false

echo "==> stopping redis"
(cd "$REPO_ROOT" && docker compose down) || true

echo "==> done"
