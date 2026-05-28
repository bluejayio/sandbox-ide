package vm

import "time"

type Status string

const (
	StatusStarting  Status = "starting"
	StatusRunning   Status = "running"
	StatusSuspended Status = "suspended"
	StatusTerminated Status = "terminated"
)

type SizeClass struct {
	Name   string
	VCPUs  int
	MemMiB int
}

var (
	Small  = SizeClass{"small", 1, 512}
	Medium = SizeClass{"medium", 2, 2048}
	Large  = SizeClass{"large", 4, 8192}
)

func SizeByName(name string) (SizeClass, bool) {
	m := map[string]SizeClass{"small": Small, "medium": Medium, "large": Large}
	s, ok := m[name]
	return s, ok
}

type VM struct {
	ID          string
	Status      Status
	Size        SizeClass
	WorkspaceID string
	Runtime     string

	// Host-side paths
	SocketPath  string
	OverlayPath string
	TAPName     string
	VsockPath   string
	GuestCID    int
	PID         int

	// Set when suspended
	SnapshotPath string
	MemPath      string

	StartedAt   time.Time
	SuspendedAt time.Time
}
