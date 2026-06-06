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

  # pidfile may contain multiple PIDs (one per line) because up.sh captures
  # the host-agent via `pgrep -f`, which matches the sudo wrappers too.
  local stopped=() skipped=()
  local pid
  while read -r pid; do
    [[ -z "$pid" ]] && continue
    if ! kill -0 "$pid" 2>/dev/null && ! sudo kill -0 "$pid" 2>/dev/null; then
      skipped+=("$pid")
      continue
    fi
    if [[ "$use_sudo" == "true" ]]; then
      sudo kill "$pid" 2>/dev/null || true
    else
      kill "$pid" 2>/dev/null || true
    fi
    stopped+=("$pid")
  done < "$pidfile"

  if ((${#stopped[@]})); then
    echo "    $name: stopped pid(s) ${stopped[*]}"
  fi
  if ((${#skipped[@]})); then
    echo "    $name: pid(s) ${skipped[*]} not running"
  fi
  if ((${#stopped[@]} == 0 && ${#skipped[@]} == 0)); then
    echo "    $name: empty pidfile"
  fi
  rm -f "$pidfile"
}

echo "==> stopping host-agent"
stop_pidfile "host-agent" "$RUN_DIR/agent.pid" true

echo "==> stopping scheduler"
stop_pidfile "scheduler" "$RUN_DIR/scheduler.pid" false

echo "==> stopping redis"
(cd "$REPO_ROOT" && docker compose down) || true

echo "==> done"
