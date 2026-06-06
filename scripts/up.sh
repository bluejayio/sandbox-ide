#!/usr/bin/env bash
# Starts the full local stack on this machine: Redis (via docker compose),
# the scheduler, and the host-agent. All three run in the background; PIDs
# and logs are written under run/ at the repo root so down.sh can stop them.
#
# Usage:
#   scripts/up.sh              # use binaries already in bin/ (run build.sh first)
#   scripts/up.sh --build      # rebuild binaries before starting

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$REPO_ROOT/bin"
RUN_DIR="$REPO_ROOT/run"
mkdir -p "$RUN_DIR"

if [[ "${1:-}" == "--build" ]]; then
  "$REPO_ROOT/scripts/build.sh"
fi

if [[ ! -x "$BIN_DIR/scheduler" || ! -x "$BIN_DIR/agent" ]]; then
  echo "error: binaries missing. Run scripts/build.sh first (or scripts/up.sh --build)." >&2
  exit 1
fi

# Kill any leftover scheduler/agent from a previous run that didn't get
# cleaned up (typically because the user forgot down.sh). Without this,
# the new processes fail silently on "address already in use".
echo "==> clearing stale processes"
if pgrep -f "^$BIN_DIR/scheduler " >/dev/null; then
  echo "    killing stale scheduler"
  pkill -f "^$BIN_DIR/scheduler " || true
fi
if pgrep -f "^$BIN_DIR/agent " >/dev/null; then
  echo "    killing stale host-agent (needs sudo)"
  sudo pkill -f "^$BIN_DIR/agent " || true
fi
sleep 1   # give the kernel a moment to release the ports

# 1. Redis ---------------------------------------------------------------
echo "==> starting redis"
(cd "$REPO_ROOT" && docker compose up -d redis)

# Wait until redis responds to PING. compose's healthcheck handles this
# eventually, but we want it ready before the scheduler tries to connect.
echo "==> waiting for redis"
for i in {1..20}; do
  if docker exec sandbox-redis redis-cli ping >/dev/null 2>&1; then
    echo "    redis ready"
    break
  fi
  sleep 0.5
done

# 2. Scheduler -----------------------------------------------------------
echo "==> starting scheduler"
nohup "$BIN_DIR/scheduler" \
  --addr :9090 \
  --backend redis \
  --redis-url redis://localhost:6379/0 \
  > "$RUN_DIR/scheduler.log" 2>&1 &
echo $! > "$RUN_DIR/scheduler.pid"

# Wait for the scheduler to start listening before launching the agent
# (otherwise the agent's first heartbeat will hit a connection-refused).
echo "==> waiting for scheduler"
for i in {1..20}; do
  if curl -fsS localhost:9090/v1/hosts >/dev/null 2>&1; then
    echo "    scheduler ready"
    break
  fi
  sleep 0.5
done

# 3. Host agent ----------------------------------------------------------
# The agent needs sudo because it creates TAP devices, manipulates iptables,
# and talks to /dev/kvm. sudo -E preserves PATH so the binary is findable.
echo "==> starting host-agent (needs sudo)"
sudo -b nohup "$BIN_DIR/agent" \
  --addr :8080 \
  --scheduler http://localhost:9090 \
  --advertise-url http://localhost:8080 \
  > "$RUN_DIR/agent.log" 2>&1 || true

# Capture the agent PID. sudo backgrounded the process, so we grep ps.
# Anchor the pattern at the start of the command line so we match only the
# real binary invocation, not the sudo/nohup wrappers (which also have the
# binary path somewhere in their argv).
sleep 0.5
pgrep -f "^$BIN_DIR/agent " > "$RUN_DIR/agent.pid" || true

echo ""
echo "==> stack is up"
echo "    scheduler  PID $(cat "$RUN_DIR/scheduler.pid" 2>/dev/null || echo "?"),  log $RUN_DIR/scheduler.log"
echo "    host-agent PID $(cat "$RUN_DIR/agent.pid" 2>/dev/null || echo "?"),  log $RUN_DIR/agent.log"
echo ""
echo "tail logs:    tail -f $RUN_DIR/*.log"
echo "smoke test:   scripts/test.sh"
echo "stop:         scripts/down.sh"
