// Package session is the scheduler's in-memory record of active sessions.
//
// A session is the compute attached to a workspace: one VM running on one
// host. The store maps session_id → which VM, which host, what state.
//
// MVP: in-memory only. Lost on restart. The intended production design uses
// Postgres for durable session records + Redis for hot path lookups.
package session

import (
	"errors"
	"sync"
	"time"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusError    Status = "error"
	StatusDeleted  Status = "deleted"
)

type Session struct {
	ID          string
	WorkspaceID string
	Runtime     string
	SizeClass   string
	MemMiB      int       // resource reservation, needed for Release on delete
	HostID      string    // which host the VM lives on
	HostBaseURL string    // cached for fast routing (avoids hostpool lookup)
	VMID        string    // VM id on the host agent
	Status      Status
	CreatedAt   time.Time
}

var ErrNotFound = errors.New("session not found")

type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

func (s *Store) Put(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *Store) Get(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	// Return a copy so callers can't mutate shared state.
	copy := *sess
	return &copy, nil
}

// UpdateStatus atomically sets a new status on an existing session.
func (s *Store) UpdateStatus(id string, status Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	sess.Status = status
	return nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(s.sessions, id)
	return nil
}

func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		copy := *sess
		out = append(out, &copy)
	}
	return out
}
