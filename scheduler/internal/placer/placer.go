// Package placer chooses which host should run a new VM. The MVP uses
// best-fit on memory: among hosts that can accommodate the VM, pick the one
// that will have the least free memory remaining after placement, so the
// fleet packs tightly.
package placer

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/sandbox-ide/scheduler/internal/hostpool"
)

// SizeClass mirrors what the API request specifies. Resource requirements
// for placement decisions.
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

// ErrNoCapacity means no healthy host can accommodate the request. The caller
// should either retry, queue the request, or signal the autoscaler.
var ErrNoCapacity = errors.New("no host has capacity for this size class")

// Pick returns the chosen host. Best-fit on memory.
//
// TODO: extend with CPU oversubscription cap and VM-slot limit once the host
//       agent's heartbeat reports allocated_vcpus / physical_cpus / vm_count.
func Pick(ctx context.Context, pool hostpool.Pool, size SizeClass) (*hostpool.Host, error) {
	hosts, err := pool.Healthy(ctx, time.Now())
	if err != nil {
		return nil, err
	}

	var best *hostpool.Host
	bestScore := math.MaxInt

	for i := range hosts {
		h := &hosts[i]
		if h.FreeMemMiB < size.MemMiB {
			continue
		}
		// Best-fit: prefer the host that will have the *least* free memory
		// remaining after this placement.
		score := h.FreeMemMiB - size.MemMiB
		if score < bestScore {
			bestScore = score
			best = h
		}
	}

	if best == nil {
		return nil, ErrNoCapacity
	}
	return best, nil
}
