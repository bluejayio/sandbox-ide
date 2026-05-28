package firecracker

// MachineConfig maps to PUT /machine-config
type MachineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

// BootSource maps to PUT /boot-source
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

// Drive maps to PUT /drives/{drive_id}
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface maps to PUT /network-interfaces/{iface_id}
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

// Vsock maps to PUT /vsock
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// InstanceAction maps to PUT /actions
type InstanceAction struct {
	ActionType string `json:"action_type"`
}

// SnapshotCreateParams maps to PUT /snapshot/create
type SnapshotCreateParams struct {
	SnapshotType string `json:"snapshot_type"` // "Full" or "Diff"
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// SnapshotLoadParams maps to PUT /snapshot/load
type SnapshotLoadParams struct {
	SnapshotPath        string     `json:"snapshot_path"`
	MemBackend          MemBackend `json:"mem_backend"`
	EnableDiffSnapshots bool       `json:"enable_diff_snapshots"`
	ResumeVM            bool       `json:"resume_vm"`
}

type MemBackend struct {
	BackendPath string `json:"backend_path"`
	BackendType string `json:"backend_type"` // "File"
}
