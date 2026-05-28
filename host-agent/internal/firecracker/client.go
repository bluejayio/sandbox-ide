package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"time"
)

// Client drives the Firecracker REST API over a Unix domain socket.
// Each running microVM has its own socket at a distinct path.
type Client struct {
	vmID       string
	socketPath string
	http       *http.Client
}

func NewClient(socketPath, vmID string) *Client {
	return &Client{
		vmID:       vmID,
		socketPath: socketPath,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", socketPath)
				},
			},
		},
	}
}

func (c *Client) put(path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker[%s] %s: %w", c.vmID, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("firecracker[%s] %s: status %d", c.vmID, path, resp.StatusCode)
	}
	return nil
}

func (c *Client) ConfigureMachine(cfg MachineConfig) error {
	return c.put("/machine-config", cfg)
}

func (c *Client) ConfigureBootSource(bs BootSource) error {
	return c.put("/boot-source", bs)
}

func (c *Client) AddDrive(drive Drive) error {
	return c.put("/drives/"+drive.DriveID, drive)
}

func (c *Client) AddNetworkInterface(iface NetworkInterface) error {
	return c.put("/network-interfaces/"+iface.IfaceID, iface)
}

func (c *Client) ConfigureVsock(vsock Vsock) error {
	return c.put("/vsock", vsock)
}

func (c *Client) Start() error {
	return c.put("/actions", InstanceAction{ActionType: "InstanceStart"})
}

func (c *Client) CreateSnapshot(params SnapshotCreateParams) error {
	return c.put("/snapshot/create", params)
}

func (c *Client) LoadSnapshot(params SnapshotLoadParams) error {
	return c.put("/snapshot/load", params)
}

// Spawn starts a Firecracker process and waits until its API socket is ready.
func Spawn(vmID, socketPath, logPath string) (int, error) {
	args := []string{"--api-sock", socketPath, "--id", vmID}
	if logPath != "" {
		args = append(args, "--log-path", logPath, "--level", "Info")
	}

	cmd := exec.Command("/usr/bin/firecracker", args...)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn firecracker[%s]: %w", vmID, err)
	}

	// Poll until the API socket appears (firecracker creates it on startup).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			return cmd.Process.Pid, nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	cmd.Process.Kill()
	return 0, fmt.Errorf("firecracker[%s]: API socket never appeared at %s", vmID, socketPath)
}
