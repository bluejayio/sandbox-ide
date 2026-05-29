package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sandbox-ide/host-agent/internal/firecracker"
	"github.com/sandbox-ide/host-agent/internal/network"
)

const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodule overlay.metacopy=off"

type Config struct {
	KernelPath   string // path to vmlinux on host
	BaseImageDir string // base rootfs images: <dir>/<runtime>.ext4
	SnapshotDir  string // where VM snapshots are written
	SocketDir    string // unix sockets and overlay files
	LogDir       string // per-VM firecracker logs
}

type Manager struct {
	cfg      Config
	mu       sync.RWMutex
	vms      map[string]*VM
	tapIndex atomic.Int32
	cidSeq   atomic.Int32 // vsock CIDs start at 3 (0-2 are reserved)
}

func NewManager(cfg Config) *Manager {
	m := &Manager{cfg: cfg, vms: make(map[string]*VM)}
	m.cidSeq.Store(3)
	for _, dir := range []string{cfg.SnapshotDir, cfg.SocketDir, cfg.LogDir} {
		os.MkdirAll(dir, 0o750)
	}
	return m
}

// Create boots a new microVM for the given workspace and runtime.
func (m *Manager) Create(ctx context.Context, vmID, runtime, workspaceID string, size SizeClass) (*VM, error) {
	socketPath := filepath.Join(m.cfg.SocketDir, vmID+".sock")
	overlayPath := filepath.Join(m.cfg.SocketDir, vmID+"-root.ext4")
	vsockPath := filepath.Join(m.cfg.SocketDir, vmID+"-vsock.sock")
	logPath := filepath.Join(m.cfg.LogDir, vmID+".log")

	// 1. Per-VM writable copy of the base image. Firecracker only supports
	//    raw block devices (no qcow2), so we make an actual copy. On
	//    reflink-capable filesystems (btrfs, XFS) this is instant and shares
	//    pages with the base. On ext4 it does a full copy.
	baseImage := filepath.Join(m.cfg.BaseImageDir, runtime+".ext4")
	if err := createOverlay(baseImage, overlayPath); err != nil {
		return nil, fmt.Errorf("create rootfs for %s: %w", vmID, err)
	}

	// 2. TAP device — unique index keeps subnet addresses non-overlapping.
	idx := int(m.tapIndex.Add(1))
	tap, err := network.Create(vmID, idx)
	if err != nil {
		os.Remove(overlayPath)
		return nil, fmt.Errorf("create tap for %s: %w", vmID, err)
	}

	// 3. Spawn the Firecracker process; it creates the API socket on startup.
	pid, err := firecracker.Spawn(vmID, socketPath, logPath)
	if err != nil {
		network.Delete(tap.Name)
		os.Remove(overlayPath)
		return nil, err
	}

	cid := int(m.cidSeq.Add(1))
	fc := firecracker.NewClient(socketPath, vmID)

	// 4. Configure and boot via the Firecracker REST API.
	steps := []struct {
		name string
		fn   func() error
	}{
		{"machine-config", func() error {
			return fc.ConfigureMachine(firecracker.MachineConfig{VCPUCount: size.VCPUs, MemSizeMiB: size.MemMiB})
		}},
		{"boot-source", func() error {
			return fc.ConfigureBootSource(firecracker.BootSource{KernelImagePath: m.cfg.KernelPath, BootArgs: bootArgs})
		}},
		{"root-drive", func() error {
			return fc.AddDrive(firecracker.Drive{DriveID: "rootfs", PathOnHost: overlayPath, IsRootDevice: true})
		}},
		{"network", func() error {
			return fc.AddNetworkInterface(firecracker.NetworkInterface{IfaceID: "eth0", GuestMAC: tap.MAC.String(), HostDevName: tap.Name})
		}},
		{"vsock", func() error {
			return fc.ConfigureVsock(firecracker.Vsock{GuestCID: cid, UDSPath: vsockPath})
		}},
		{"start", func() error { return fc.Start() }},
	}

	for _, step := range steps {
		if err := step.fn(); err != nil {
			m.cleanup(vmID, tap.Name, overlayPath, pid)
			return nil, fmt.Errorf("vm %s configure %s: %w", vmID, step.name, err)
		}
	}

	v := &VM{
		ID: vmID, Status: StatusRunning, Size: size,
		WorkspaceID: workspaceID, Runtime: runtime,
		SocketPath: socketPath, OverlayPath: overlayPath,
		TAPName: tap.Name, VsockPath: vsockPath,
		GuestCID: cid, PID: pid,
		StartedAt: time.Now(),
	}
	m.mu.Lock()
	m.vms[vmID] = v
	m.mu.Unlock()

	return v, nil
}

// Snapshot pauses the VM and writes a full memory snapshot to disk.
// The VM process keeps running in a paused state until Destroy is called.
func (m *Manager) Snapshot(ctx context.Context, vmID string) (snapPath, memPath string, err error) {
	m.mu.RLock()
	v, ok := m.vms[vmID]
	m.mu.RUnlock()
	if !ok {
		return "", "", fmt.Errorf("vm %s not found", vmID)
	}

	snapPath = filepath.Join(m.cfg.SnapshotDir, vmID+"-snap")
	memPath = filepath.Join(m.cfg.SnapshotDir, vmID+"-mem")

	fc := firecracker.NewClient(v.SocketPath, vmID)
	if err := fc.CreateSnapshot(firecracker.SnapshotCreateParams{
		SnapshotType: "Full",
		SnapshotPath: snapPath,
		MemFilePath:  memPath,
	}); err != nil {
		return "", "", fmt.Errorf("snapshot %s: %w", vmID, err)
	}

	m.mu.Lock()
	v.Status = StatusSuspended
	v.SnapshotPath = snapPath
	v.MemPath = memPath
	v.SuspendedAt = time.Now()
	m.mu.Unlock()

	return snapPath, memPath, nil
}

// Restore boots a new Firecracker process from a memory snapshot.
// The restored VM resumes exactly where it was paused.
func (m *Manager) Restore(ctx context.Context, vmID, snapPath, memPath, runtime string, size SizeClass) (*VM, error) {
	socketPath := filepath.Join(m.cfg.SocketDir, vmID+".sock")
	vsockPath := filepath.Join(m.cfg.SocketDir, vmID+"-vsock.sock")
	logPath := filepath.Join(m.cfg.LogDir, vmID+"-restore.log")

	idx := int(m.tapIndex.Add(1))
	tap, err := network.Create(vmID, idx)
	if err != nil {
		return nil, fmt.Errorf("restore tap for %s: %w", vmID, err)
	}

	pid, err := firecracker.Spawn(vmID, socketPath, logPath)
	if err != nil {
		network.Delete(tap.Name)
		return nil, err
	}

	cid := int(m.cidSeq.Add(1))
	fc := firecracker.NewClient(socketPath, vmID)

	if err := fc.LoadSnapshot(firecracker.SnapshotLoadParams{
		SnapshotPath: snapPath,
		MemBackend:   firecracker.MemBackend{BackendPath: memPath, BackendType: "File"},
		ResumeVM:     true,
	}); err != nil {
		m.cleanup(vmID, tap.Name, "", pid)
		return nil, fmt.Errorf("restore %s: %w", vmID, err)
	}
	fc.ConfigureVsock(firecracker.Vsock{GuestCID: cid, UDSPath: vsockPath})

	v := &VM{
		ID: vmID, Status: StatusRunning, Size: size, Runtime: runtime,
		SocketPath: socketPath, TAPName: tap.Name,
		VsockPath: vsockPath, GuestCID: cid, PID: pid,
		SnapshotPath: snapPath, MemPath: memPath,
		StartedAt: time.Now(),
	}
	m.mu.Lock()
	m.vms[vmID] = v
	m.mu.Unlock()

	return v, nil
}

// Destroy terminates the VM process and releases all host resources.
func (m *Manager) Destroy(ctx context.Context, vmID string) error {
	m.mu.Lock()
	v, ok := m.vms[vmID]
	if ok {
		delete(m.vms, vmID)
	}
	m.mu.Unlock()

	if !ok {
		return nil
	}

	v.Status = StatusTerminated
	m.cleanup(vmID, v.TAPName, v.OverlayPath, v.PID)
	return nil
}

func (m *Manager) Get(vmID string) (*VM, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.vms[vmID]
	return v, ok
}

func (m *Manager) List() []*VM {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*VM, 0, len(m.vms))
	for _, v := range m.vms {
		out = append(out, v)
	}
	return out
}

// FreeMemMiB returns the estimated free memory available for new VMs.
func (m *Manager) FreeMemMiB() int {
	// Read available memory from /proc/meminfo in production.
	// Simplified: subtract running VM allocations from total.
	total := totalMemMiB()
	m.mu.RLock()
	defer m.mu.RUnlock()
	used := 0
	for _, v := range m.vms {
		if v.Status == StatusRunning {
			used += v.Size.MemMiB
		}
	}
	return total - used
}

func (m *Manager) cleanup(vmID, tapName, overlayPath string, pid int) {
	if tapName != "" {
		network.Delete(tapName)
	}
	if overlayPath != "" {
		os.Remove(overlayPath)
	}
	if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
	os.Remove(filepath.Join(m.cfg.SocketDir, vmID+".sock"))
	os.Remove(filepath.Join(m.cfg.SocketDir, vmID+"-vsock.sock"))
}

// createOverlay produces a per-VM writable rootfs from the shared base image.
// Firecracker requires raw block devices (no qcow2), so we copy the base.
//
// `cp --reflink=auto` attempts a copy-on-write reflink first (instant, no
// disk usage) and falls back to a full copy on filesystems that don't
// support reflinks (ext4). The base image is never modified.
func createOverlay(basePath, overlayPath string) error {
	out, err := exec.Command("cp", "--reflink=auto", basePath, overlayPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp: %s: %w", out, err)
	}
	return nil
}

func totalMemMiB() int {
	// In production: parse /proc/meminfo MemTotal.
	return 64 * 1024 // assume 64 GiB host for now
}
