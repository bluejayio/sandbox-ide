// Package hostpool tracks the set of registered host agents and their
// last-known capacity. Host agents POST /internal/heartbeat every 10s; the
// pool updates the in-memory record. Stale hosts (no heartbeat in >30s) are
// considered unhealthy and excluded from placement.
package hostpool

import (
	"context"
	"sync"
	"time"
)

const (
	// HealthTTL is how long a host is considered healthy after its last
	// heartbeat. Beyond this, the placer skips it.
	HealthTTL = 30 * time.Second
)

// VMSummary mirrors what the host agent reports per running VM.
type VMSummary struct {
	VMID        string `json:"vm_id"`
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id"`
	Runtime     string `json:"runtime"`
	SizeClass   string `json:"size_class"`
}

// Host is the scheduler's view of one registered host agent.
type Host struct {
	ID         string    // stable host identifier (hostname or registered id)
	BaseURL    string    // e.g. http://10.0.1.42:8080 — used by hostclient
	FreeMemMiB int       // updated each heartbeat (and decremented by Reserve)
	VMs        []VMSummary
	LastSeen   time.Time
	// TODO: track AllocatedVCPUs, PhysicalCPUs, VMCount once host agent
	//       extends its heartbeat to report them.
}

// Healthy reports whether the host's last heartbeat is recent enough that
// the placer should consider it.
func (h *Host) Healthy(now time.Time) bool {
	return now.Sub(h.LastSeen) <= HealthTTL
}

// Heartbeat is the payload host agents POST to the scheduler.
type Heartbeat struct {
	HostID     string      `json:"host_id"`
	BaseURL    string      `json:"base_url"`
	FreeMemMiB int         `json:"free_mem_mib"`
	VMs        []VMSummary `json:"vms"`
	Timestamp  time.Time   `json:"timestamp"`
}

// Pool is the interface the rest of the scheduler depends on. Two
// implementations exist: InMemory (default, lost on restart) and Redis
// (persistent across scheduler restarts, with native TTL semantics).
type Pool interface {
	Apply(ctx context.Context, hb Heartbeat, now time.Time) error
	Healthy(ctx context.Context, now time.Time) ([]Host, error)
	All(ctx context.Context) ([]Host, error)
	Reserve(ctx context.Context, hostID string, mib int) error
	Release(ctx context.Context, hostID string, mib int) error
}

// InMemoryPool is the default Pool implementation. State is lost on
// scheduler restart. Suitable for local development and tests.
type InMemoryPool struct {
	mu    sync.RWMutex
	hosts map[string]*Host
}

func NewInMemory() *InMemoryPool {
	return &InMemoryPool{hosts: make(map[string]*Host)}
}

func (p *InMemoryPool) Apply(_ context.Context, hb Heartbeat, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	h, ok := p.hosts[hb.HostID]
	if !ok {
		h = &Host{ID: hb.HostID}
		p.hosts[hb.HostID] = h
	}
	h.BaseURL = hb.BaseURL
	h.FreeMemMiB = hb.FreeMemMiB
	h.VMs = hb.VMs
	h.LastSeen = now
	return nil
}

func (p *InMemoryPool) Healthy(_ context.Context, now time.Time) ([]Host, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]Host, 0, len(p.hosts))
	for _, h := range p.hosts {
		if h.Healthy(now) {
			out = append(out, *h)
		}
	}
	return out, nil
}

func (p *InMemoryPool) All(_ context.Context) ([]Host, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]Host, 0, len(p.hosts))
	for _, h := range p.hosts {
		out = append(out, *h)
	}
	return out, nil
}

func (p *InMemoryPool) Reserve(_ context.Context, hostID string, mib int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok {
		h.FreeMemMiB -= mib
	}
	return nil
}

func (p *InMemoryPool) Release(_ context.Context, hostID string, mib int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok {
		h.FreeMemMiB += mib
	}
	return nil
}
