// Package hostclient is the scheduler's HTTP client for talking to a host
// agent. It mirrors the agent's HTTP API (POST /vms, DELETE /vms/{id}).
package hostclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: 60 * time.Second, // VM boot and snapshot can be slow
		},
	}
}

// CreateVMRequest matches host-agent/internal/api.createVMRequest.
type CreateVMRequest struct {
	VMID        string `json:"vm_id"`
	Runtime     string `json:"runtime"`
	WorkspaceID string `json:"workspace_id"`
	SizeClass   string `json:"size_class"`
}

// VMResponse matches host-agent/internal/api.vmResponse.
type VMResponse struct {
	VMID      string    `json:"vm_id"`
	Status    string    `json:"status"`
	GuestCID  int       `json:"guest_cid"`
	TAPName   string    `json:"tap_name"`
	StartedAt time.Time `json:"started_at"`
}

func (c *Client) CreateVM(ctx context.Context, req CreateVMRequest) (*VMResponse, error) {
	var out VMResponse
	if err := c.do(ctx, http.MethodPost, "/vms", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DestroyVM(ctx context.Context, vmID string) error {
	return c.do(ctx, http.MethodDelete, "/vms/"+vmID, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("host agent %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("host agent %s %s: status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
