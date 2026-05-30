#!/usr/bin/env bash
# Bakes the vm-agent binary into a fresh copy of the base rootfs image,
# producing python3.12-with-agent.ext4. The host agent's --base-images dir
# should then be pointed at the directory containing this image, and the
# session runtime should be "python3.12-with-agent" so the agent picks it.
#
# Must be run on Linux (we mount an ext4 image via the loop device).
# Requires root for mount/umount.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Inputs
BASE_IMAGE=${BASE_IMAGE:-/opt/firecracker/images/python3.12.ext4}
AGENT_BIN=${AGENT_BIN:-$REPO_ROOT/bin/vm-agent}

# Output
OUT_IMAGE=${OUT_IMAGE:-/opt/firecracker/images/python3.12-with-agent.ext4}

# Workspace
MOUNT_DIR=${MOUNT_DIR:-/mnt/sandbox-rootfs}

# 1. Cross-compile vm-agent for the guest (Linux x86_64).
if [[ ! -x "$AGENT_BIN" ]]; then
  echo "==> vm-agent not built; building now"
  (cd "$REPO_ROOT/vm-agent" && GOOS=linux GOARCH=amd64 go build -o "$AGENT_BIN" ./cmd/vm-agent)
fi
echo "==> using vm-agent: $AGENT_BIN ($(du -h "$AGENT_BIN" | cut -f1))"

# 2. Copy the base image to the output path.
echo "==> copying $BASE_IMAGE → $OUT_IMAGE"
sudo cp "$BASE_IMAGE" "$OUT_IMAGE"

# 3. Grow the filesystem so we have space for the new binary + unit file.
#    fsck must run before resize2fs, and resize2fs requires the loop file
#    to be the right size first.
echo "==> growing filesystem"
sudo truncate -s 1G "$OUT_IMAGE"
sudo e2fsck -f -y "$OUT_IMAGE" >/dev/null
sudo resize2fs "$OUT_IMAGE" >/dev/null

# 4. Mount, copy binary + systemd unit, enable the service.
echo "==> mounting and installing vm-agent"
sudo mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$OUT_IMAGE" "$MOUNT_DIR"

cleanup() { sudo umount "$MOUNT_DIR" 2>/dev/null || true; }
trap cleanup EXIT

sudo install -m 0755 "$AGENT_BIN" "$MOUNT_DIR/usr/local/bin/vm-agent"

# systemd unit: run as root, restart on failure, listen on vsock port 5252.
sudo tee "$MOUNT_DIR/etc/systemd/system/vm-agent.service" >/dev/null <<'EOF'
[Unit]
Description=Sandbox VM Agent (vsock exec channel)
After=network.target

[Service]
ExecStart=/usr/local/bin/vm-agent --port 5252
Restart=always
RestartSec=1
User=root

[Install]
WantedBy=multi-user.target
EOF

# Enable the unit by creating the multi-user.target.wants symlink directly
# (we can't run `systemctl enable` against an offline rootfs).
sudo mkdir -p "$MOUNT_DIR/etc/systemd/system/multi-user.target.wants"
sudo ln -sf /etc/systemd/system/vm-agent.service \
  "$MOUNT_DIR/etc/systemd/system/multi-user.target.wants/vm-agent.service"

sudo umount "$MOUNT_DIR"
trap - EXIT

echo ""
echo "==> done"
echo "    image:  $OUT_IMAGE"
echo "    runtime to request when creating a session: python3.12-with-agent"
