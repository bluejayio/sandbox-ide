package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
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
// On failure, returns an error that includes firecracker's exit status and
// any stdout/stderr it produced so the caller can see why startup failed.
//
// We capture both stdout and stderr because Firecracker writes startup
// errors to stdout in some configurations (the --log-path file only opens
// after config is validated). We also reap the process via Wait() in a
// goroutine so an early exit is detected immediately rather than after the
// full 5-second socket-poll timeout.
func Spawn(vmID, socketPath, logPath string) (int, error) {
	args := []string{"--api-sock", socketPath, "--id", vmID}
	if logPath != "" {
		args = append(args, "--log-path", logPath, "--level", "Info")
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("/usr/bin/firecracker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn firecracker[%s]: %w", vmID, err)
	}

	// Reap the process in the background. If firecracker exits before
	// the API socket appears, this channel fires immediately and we can
	// return a useful error instead of timing out.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Poll until the API socket appears or firecracker dies.
	deadline := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			return cmd.Process.Pid, nil
		}

		select {
		case waitErr := <-exited:
			return 0, fmt.Errorf("firecracker[%s] exited early (%v); stdout=%q stderr=%q",
				vmID, waitErr,
				strings.TrimSpace(stdout.String()),
				strings.TrimSpace(stderr.String()))
		case <-ticker.C:
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				<-exited // reap to avoid zombie
				return 0, fmt.Errorf("firecracker[%s]: API socket never appeared at %s; stdout=%q stderr=%q",
					vmID, socketPath,
					strings.TrimSpace(stdout.String()),
					strings.TrimSpace(stderr.String()))
			}
		}
	}
}
