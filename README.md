# sandbox-ide

A multi-tenant, browser-based cloud IDE / notebook. Users open a workspace, run
code in an isolated sandbox, and stream output back to the browser. Code runs
inside [Firecracker](https://firecracker-microvm.github.io/) microVMs for
hardware-level isolation between tenants while keeping cold-start latency
~150 ms.

## Architecture at a glance

```
Browser ──► API gateway ──► Scheduler ──► Host agent ──► Firecracker microVM
                                │              │              │
                                ▼              ▼              ▼
                              Redis      TAP + iptables    vm-agent (vsock)
                            (hostpool)   (per-VM NAT)
```

- **Scheduler** — receives session create/delete, picks a host via best-fit
  memory placement, calls the host agent. State in Redis (host registry,
  reservations).
- **Host agent** — runs on every Firecracker host. Spawns/snapshots/destroys
  microVMs, sets up TAP networking, sends periodic heartbeats with capacity.
- **vm-agent** *(in progress)* — runs inside each guest. Listens on vsock,
  executes shell commands, streams output back to the host agent.

## Repository layout

```
sandbox-ide/
├── scheduler/        Go module — places sessions on hosts
├── host-agent/       Go module — drives Firecracker on a hypervisor host
├── vm-agent/         Go module — runs inside the guest, vsock exec channel
├── scripts/          build / up / down / test / build-image
└── docker-compose.yml  Redis for local dev (bound to 127.0.0.1)
```

---

## Setting up the host agent using EC2 instance.

Firecracker requires Linux + KVM. Most cloud providers don't expose
`/dev/kvm` on VMs. Bare metal machines are extremely expensive; AWS only does on (**C8i, M8i, and R8i instances**)[https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual/]. Specifically:

- ✅ Works: `c8.*`, `M8i.*`, `R8i.*` (x86, current-gen Nitro)
- ❌ Does **not** work: Graviton instances (`c6g.*`, `c7g.*`, etc.) — ARM
  with no nested-virt
- ❌ Does **not** work: Lightsail — KVM not exposed
- ❌ Avoid: bare-metal `*.metal` instances — they work but cost 50× more

### Launch

| Setting | Value |
|---|---|
| Instance type | `c8i.large` (2 vCPU, 4 GB) is fine for dev. Must be from C8i / M8i / R8i — nested virt isn't available on older families. |
| Architecture | **x86 / amd64** |
| AMI | Ubuntu 24.04 LTS amd64 |
| Storage | Default 30 GB gp3 |
| Filesystem option | None (S3 / EFS / FSx not needed yet) |
| Security group | SSH (22) from your IP only. Nothing else open. |

### One-time bootstrap on the instance

```bash
# 1. System deps
sudo apt update
sudo apt install -y golang-go docker.io docker-compose-v2 jq

# 2. Add yourself to docker group (avoids sudo for docker commands)
sudo usermod -aG docker ubuntu
newgrp docker

# 3. Firecracker binary
wget https://github.com/firecracker-microvm/firecracker/releases/download/v1.7.0/firecracker-v1.7.0-x86_64.tgz
tar -xzf firecracker-v1.7.0-x86_64.tgz
sudo mv release-v1.7.0-x86_64/firecracker-v1.7.0-x86_64 /usr/bin/firecracker
sudo chmod +x /usr/bin/firecracker
firecracker --version

# 4. Make /dev/kvm accessible (persists across reboots via udev rule)
echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/65-kvm.rules
sudo udevadm trigger /dev/kvm
ls -l /dev/kvm   # expect: crw-rw-rw-

# 5. Directories the host agent expects (owned by ubuntu so the agent can
#    write to them; runs as root via sudo so this is belt-and-braces)
sudo mkdir -p /opt/firecracker/images /var/lib/agent/snapshots \
              /var/run/agent/vms /var/log/agent/vms
sudo chown -R ubuntu:ubuntu /opt/firecracker /var/lib/agent \
                            /var/run/agent /var/log/agent
```

### Download the guest kernel and base rootfs

Firecracker requires a prebuilt Linux kernel (`vmlinux`) and a raw ext4
rootfs image. The Firecracker project publishes both on S3 under their CI
bucket — these are the same images their own integration tests use.

```bash
cd /opt/firecracker

# Guest kernel (Linux 5.10, ~25 MB)
wget -O vmlinux \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.11/x86_64/vmlinux-5.10.225

# Base rootfs — Ubuntu 22.04 (~300 MB)
wget -O images/python3.12.ext4 \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.7/x86_64/ubuntu-22.04.ext4

# Optional: SSH key for the rootfs (lets you SSH into a booted VM for debugging)
wget -O ubuntu-22.04.id_rsa \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.7/x86_64/ubuntu-22.04.id_rsa
chmod 600 ubuntu-22.04.id_rsa
```

The rootfs is named `python3.12.ext4` because the scheduler's runtime field
maps to `{runtime}.ext4` in the base image dir. The actual image is plain
Ubuntu 22.04 — Python comes preinstalled. We'll bake a real Python-specific
image once we add the vm-agent.

**Available kernel versions** (in `firecracker-ci/v1.11/x86_64/`):
- `vmlinux-5.10.225` — recommended
- `vmlinux-6.1.102` — newer, marginally larger
- `vmlinux-5.10.225-no-acpi` — for ACPI-disabled boot

You can list everything available with:
```bash
curl -s 'https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/v1.11/x86_64/&list-type=2' \
  | grep -oP '<Key>[^<]+'
```

---

## Running the system

### First time (or after pulling new code)

```bash
git clone git@github.com:bluejayio/sandbox-ide.git
cd sandbox-ide
./scripts/build.sh
```

### Bring everything up

```bash
./scripts/up.sh
```

This starts Redis (in Docker), the scheduler, and the host agent — all in the
background. Logs go to `run/*.log`, PIDs to `run/*.pid`.

### Smoke test

```bash
# Wait ~12s after up.sh for the first heartbeat to register the host
sleep 12
./scripts/test.sh
```

The script lists hosts, creates a session (which boots a real microVM),
reads it back, and deletes it. Should print `==> smoke test passed`.

### Tear down

```bash
./scripts/down.sh
```

---

## Local development on macOS

You can edit and build the code on macOS — the build is portable. You can't
actually run Firecracker (needs Linux + KVM), but the binaries cross-compile.

Push to a feature branch, then on the EC2 box:
```bash
git pull && ./scripts/build.sh && ./scripts/up.sh && ./scripts/test.sh
```

### VSCode / gopls

[.vscode/settings.json](.vscode/settings.json) pins gopls to `GOOS=linux` so
build-tagged Linux files (TAP / iptables / vsock) resolve correctly in the
editor. Reload your VSCode window after first opening the repo.

If gopls can't find Go itself (Homebrew installs to a non-standard path),
verify the `go.goroot` value in that settings file matches your install:
```bash
go env GOROOT
```

---

## Debugging tips

| Symptom | Likely cause | Check |
|---|---|---|
| `bind: address already in use` | Old agent still running | `sudo pkill -f bin/agent` |
| `firecracker exited early: Invalid instance ID` | VM ID contains an underscore | Fixed; use hyphens only |
| `API socket never appeared` and stderr empty | Firecracker died before logging started; need `cmd.Wait()` | Fixed; should now print real exit status |
| `Disk size N is not a multiple of sector size 512` | qcow2 rootfs (Firecracker only supports raw) | Fixed; we now copy the base image |
| Heartbeat never reaches scheduler | Wrong `--advertise-url` on the agent | Confirm host can reach `<scheduler>/internal/heartbeat` |
| Scheduler returns 503 "no host has capacity" | No host has heartbeat in <30s | Check `/v1/hosts` and agent log |

---

## Status

| Phase | Status |
|---|---|
| Host agent: Firecracker lifecycle (create/destroy/snapshot/restore) | ✅ |
| Scheduler: session create/get/delete + best-fit placement | ✅ |
| Redis-backed host registry with TTL + atomic reservations | ✅ |
| End-to-end smoke test boots a real microVM | ✅ |
| Output streaming (vsock → agent → scheduler → WebSocket → browser) | 🚧 in progress |
| Workspace files (S3-backed virtio-fs) | ⏳ |
| Pre-warm pool (snapshot-based fast assignment) | ⏳ |
| HA scheduler (active-active behind ALB) | ⏳ |
| Browser UI | ⏳ |
