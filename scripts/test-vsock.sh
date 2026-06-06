#!/usr/bin/env bash
# Manual integration test for vm-agent over vsock.
#
# Boots a microVM using the python3.12-with-agent image (built by
# scripts/build-image.sh), finds its vsock CID via the host agent, then
# sends an exec request and prints the streamed NDJSON response.
#
# Prereqs on this box:
#   - scripts/up.sh has been run (Redis + scheduler + host-agent are up)
#   - scripts/build-image.sh has produced python3.12-with-agent.ext4
#   - socat is installed (sudo apt install -y socat)

set -euo pipefail

SCHED=${SCHED:-http://localhost:9090}
AGENT=${AGENT:-http://localhost:8080}
RUNTIME=${RUNTIME:-python3.12-with-agent}
VSOCK_PORT=${VSOCK_PORT:-5252}

if ! command -v socat >/dev/null; then
  echo "error: socat not installed. run: sudo apt install -y socat" >&2
  exit 1
fi

# 1. Create a session against the with-agent image -------------------------
echo "==> creating session (runtime=$RUNTIME)"
session=$(curl -fsS -X POST "$SCHED/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"workspace_id\":\"ws-vsock-test\",\"runtime\":\"$RUNTIME\",\"size_class\":\"small\"}")
echo "$session" | jq .
session_id=$(echo "$session" | jq -r .session_id)
vm_id=$(echo "$session" | jq -r .vm_id)

cleanup() {
  echo ""
  echo "==> cleaning up session $session_id"
  curl -fsS -X DELETE "$SCHED/v1/sessions/$session_id" || true
}
trap cleanup EXIT

# 2. Find the VM's vsock CID via the host agent ----------------------------
echo ""
echo "==> looking up vsock CID for $vm_id"
cid=$(curl -fsS "$AGENT/heartbeat" | jq -r ".vms[] | select(.vm_id == \"$vm_id\") | .guest_cid")
if [[ -z "$cid" || "$cid" == "null" ]]; then
  echo "error: could not find guest_cid for $vm_id" >&2
  curl -fsS "$AGENT/heartbeat" | jq .
  exit 1
fi
echo "    cid=$cid"

# 3. Wait a few seconds for the guest to boot and vm-agent to start -------
echo ""
echo "==> waiting 8s for guest boot + vm-agent startup"
sleep 8

# 4. Send an exec request and stream the response ------------------------
echo ""
echo "==> sending exec request"
request='{"type":"exec","id":"e1","code":"echo hello from vm && python3 -c \"print(2+2)\" && uname -a"}'
echo "    request: $request"
echo ""
echo "==> response stream:"
# Firecracker exposes vsock as a UDS proxy on the host (not native AF_VSOCK).
# Connect to the per-VM Unix socket, send "CONNECT <port>\n" so Firecracker
# bridges to the guest's vsock port, then exchange NDJSON. tail -n +2 strips
# Firecracker's "OK <port>" preamble so only vm-agent's frames remain.
# You must send "CONNECT <port>\n" before sending the exec request: 
# https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md#host-initiated-connections
uds="/var/run/agent/vms/${vm_id}-vsock.sock"
# sleep keeps stdin open long enough for vm-agent to run the exec and stream
# back its frames; socat -t 5 extends the half-close grace window so the read
# side of the socket isn't torn down before those frames arrive.
{
  printf 'CONNECT %s\n' "$VSOCK_PORT"
  printf '%s\n' "$request"
  sleep 5
} | sudo socat -t 5 - UNIX-CONNECT:"$uds" | tail -n +2 | head -20

echo ""
echo "==> done"
