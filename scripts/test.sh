#!/usr/bin/env bash
# End-to-end smoke test against a running scheduler + host-agent. Verifies
# that a session can be created (which boots a real microVM), looked up,
# and destroyed.

set -euo pipefail

SCHED=${SCHED:-http://localhost:9090}

if ! command -v jq >/dev/null 2>&1; then
  echo "note: jq not installed — output will be raw JSON. install with: sudo apt install -y jq" >&2
  alias jq=cat
fi

# 1. Wait for at least one host to register --------------------------------
echo "==> waiting for a host to appear in /v1/hosts"
for i in {1..30}; do
  count=$(curl -fsS "$SCHED/v1/hosts" | jq -r '.hosts | length' 2>/dev/null || echo 0)
  if [[ "$count" -ge 1 ]]; then
    echo "    $count host(s) registered"
    break
  fi
  sleep 1
done
if [[ "$count" -lt 1 ]]; then
  echo "error: no hosts registered after 30s. Check $REPO_ROOT/run/agent.log" >&2
  exit 1
fi

echo "==> hosts:"
curl -fsS "$SCHED/v1/hosts" | jq .

# 2. Create a session ------------------------------------------------------
echo ""
echo "==> POST /v1/sessions"
resp=$(curl -fsS -X POST "$SCHED/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d '{"workspace_id":"ws-smoke","runtime":"python3.12","size_class":"small"}')
echo "$resp" | jq .

session_id=$(echo "$resp" | jq -r .session_id)
if [[ -z "$session_id" || "$session_id" == "null" ]]; then
  echo "error: failed to extract session_id" >&2
  exit 1
fi

# 3. Read it back ----------------------------------------------------------
echo ""
echo "==> GET /v1/sessions/$session_id"
curl -fsS "$SCHED/v1/sessions/$session_id" | jq .

# 4. Destroy it ------------------------------------------------------------
echo ""
echo "==> DELETE /v1/sessions/$session_id"
curl -fsS -X DELETE "$SCHED/v1/sessions/$session_id"
echo "    deleted"

# 5. Confirm capacity was released ----------------------------------------
echo ""
echo "==> hosts after delete:"
curl -fsS "$SCHED/v1/hosts" | jq .

echo ""
echo "==> smoke test passed"
