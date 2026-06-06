#!/usr/bin/env bash
# End-to-end test of the host-agent's POST /vms/{id}/exec route.
#
# Unlike test-vsock.sh (which dials Firecracker's UDS directly via socat),
# this test goes through the host-agent's HTTP API — the same path real
# clients (scheduler / public gateway) will use:
#
#   curl -N → host-agent /vms/{id}/exec → vsock UDS → vm-agent in guest
#
# Prereqs:
#   - scripts/up.sh has been run (scheduler + host-agent + redis)
#   - scripts/build-image.sh has produced python3.12-with-agent.ext4
#   - jq + curl installed

set -euo pipefail

SCHED=${SCHED:-http://localhost:9090}
AGENT=${AGENT:-http://localhost:8080}
RUNTIME=${RUNTIME:-python3.12-with-agent}
BOOT_WAIT=${BOOT_WAIT:-10}

# 1. Create a session ---------------------------------------------------
echo "==> creating session (runtime=$RUNTIME)"
session=$(curl -fsS -X POST "$SCHED/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"workspace_id\":\"ws-exec-test\",\"runtime\":\"$RUNTIME\",\"size_class\":\"small\"}")
echo "$session" | jq .
session_id=$(echo "$session" | jq -r .session_id)
vm_id=$(echo "$session" | jq -r .vm_id)

cleanup() {
  echo ""
  echo "==> cleaning up session $session_id"
  curl -fsS -X DELETE "$SCHED/v1/sessions/$session_id" || true
}
trap cleanup EXIT

# 2. Wait for the guest to boot + vm-agent to start ----------------------
echo ""
echo "==> waiting ${BOOT_WAIT}s for guest boot + vm-agent startup"
sleep "$BOOT_WAIT"

# 3. POST an exec request to the host-agent. curl -N disables buffering so
# we see frames as they stream back. -d @- pipes stdin in as the body.
echo ""
echo "==> POST $AGENT/vms/$vm_id/exec"
echo ""
echo "==> response stream:"
printf '%s\n' '{"type":"exec","id":"e1","code":"echo hello from vm && python3 -c \"print(2+2)\" && uname -a"}' \
  | curl -fsSN -X POST "$AGENT/vms/$vm_id/exec" \
      -H 'Content-Type: application/x-ndjson' \
      --data-binary @-

echo ""
echo "==> done"
