package hostpool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisPool stores host state in Redis. Each host is a single key:
//
//	host:{id} → JSON({base_url, free_mem_mib, vms, last_seen})  TTL: 2 × HealthTTL
//
// Heartbeats SET with TTL — Redis natively expires dead hosts, no GC needed.
// Reservations (Reserve/Release) update only the free_mem_mib field via an
// atomic Lua script so concurrent reservations and heartbeats don't race.
type RedisPool struct {
	rdb *redis.Client
	ttl time.Duration
}

const hostKeyPrefix = "host:"

func NewRedis(rdb *redis.Client) *RedisPool {
	// Use 2× HealthTTL so a host whose heartbeat is "just slightly stale"
	// is still visible (marked unhealthy by Healthy()), giving the placer
	// the chance to skip it rather than treating it as a fresh registration.
	return &RedisPool{rdb: rdb, ttl: 2 * HealthTTL}
}

// storedHost is the JSON shape persisted in Redis. Separate from Host so
// LastSeen serialises cleanly as RFC3339.
type storedHost struct {
	BaseURL    string      `json:"base_url"`
	FreeMemMiB int         `json:"free_mem_mib"`
	VMs        []VMSummary `json:"vms"`
	LastSeen   time.Time   `json:"last_seen"`
}

func (p *RedisPool) Apply(ctx context.Context, hb Heartbeat, now time.Time) error {
	data, err := json.Marshal(storedHost{
		BaseURL: hb.BaseURL, FreeMemMiB: hb.FreeMemMiB,
		VMs: hb.VMs, LastSeen: now,
	})
	if err != nil {
		return err
	}
	return p.rdb.Set(ctx, hostKeyPrefix+hb.HostID, data, p.ttl).Err()
}

func (p *RedisPool) All(ctx context.Context) ([]Host, error) {
	keys, err := p.scanKeys(ctx)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	// MGET pulls all hosts in one round trip. Nil entries are expired
	// between the SCAN and the MGET — skip them.
	values, err := p.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis MGET hosts: %w", err)
	}

	out := make([]Host, 0, len(values))
	for i, v := range values {
		raw, ok := v.(string)
		if !ok {
			continue // key expired between SCAN and MGET
		}
		var s storedHost
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			continue // skip malformed entries; don't crash placement
		}
		out = append(out, Host{
			ID: keys[i][len(hostKeyPrefix):], BaseURL: s.BaseURL,
			FreeMemMiB: s.FreeMemMiB, VMs: s.VMs, LastSeen: s.LastSeen,
		})
	}
	return out, nil
}

func (p *RedisPool) Healthy(ctx context.Context, now time.Time) ([]Host, error) {
	all, err := p.All(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, h := range all {
		if h.Healthy(now) {
			out = append(out, h)
		}
	}
	return out, nil
}

// reserveScript atomically decrements free_mem_mib inside the stored JSON
// without disturbing other fields. Returns 1 if the key was found, 0 if not.
var reserveScript = redis.NewScript(`
local raw = redis.call("GET", KEYS[1])
if not raw then return 0 end
local h = cjson.decode(raw)
h.free_mem_mib = h.free_mem_mib + tonumber(ARGV[1])
redis.call("SET", KEYS[1], cjson.encode(h), "KEEPTTL")
return 1
`)

func (p *RedisPool) Reserve(ctx context.Context, hostID string, mib int) error {
	// Reserve = subtract mib, i.e. add a negative delta.
	_, err := reserveScript.Run(ctx, p.rdb, []string{hostKeyPrefix + hostID}, -mib).Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("reserve %s: %w", hostID, err)
	}
	return nil
}

func (p *RedisPool) Release(ctx context.Context, hostID string, mib int) error {
	_, err := reserveScript.Run(ctx, p.rdb, []string{hostKeyPrefix + hostID}, mib).Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("release %s: %w", hostID, err)
	}
	return nil
}

// scanKeys walks all host:* keys using SCAN (avoids blocking the server
// like KEYS would).
func (p *RedisPool) scanKeys(ctx context.Context) ([]string, error) {
	var (
		cursor uint64
		out    []string
	)
	for {
		batch, next, err := p.rdb.Scan(ctx, cursor, hostKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("redis SCAN: %w", err)
		}
		out = append(out, batch...)
		if next == 0 {
			break
		}
		cursor = next
	}
	return out, nil
}
