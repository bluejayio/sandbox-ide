// Package api is the scheduler's external HTTP API.
//
// External routes (consumed by the browser / API gateway):
//
//	POST   /v1/sessions          create a session (pick host, boot VM)
//	GET    /v1/sessions/{id}     read session status
//	DELETE /v1/sessions/{id}     destroy a session and reclaim capacity
//	GET    /v1/hosts             list registered hosts (debug/admin)
//
// Internal routes (consumed by host agents):
//
//	POST   /internal/heartbeat   host agent capacity report
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sandbox-ide/scheduler/internal/hostclient"
	"github.com/sandbox-ide/scheduler/internal/hostpool"
	"github.com/sandbox-ide/scheduler/internal/placer"
	"github.com/sandbox-ide/scheduler/internal/session"
)

type Server struct {
	pool     hostpool.Pool
	sessions *session.Store
	log      *slog.Logger
	mux      *http.ServeMux
}

func NewServer(pool hostpool.Pool, sessions *session.Store, log *slog.Logger) *Server {
	s := &Server{pool: pool, sessions: sessions, log: log, mux: http.NewServeMux()}

	s.mux.HandleFunc("/v1/sessions", s.handleSessions)
	s.mux.HandleFunc("/v1/sessions/", s.handleSession)
	s.mux.HandleFunc("/v1/hosts", s.handleListHosts)
	s.mux.HandleFunc("/internal/heartbeat", s.handleHeartbeat)

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.mux.ServeHTTP(w, r)
	s.log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
}

// --- /v1/sessions (POST create) -----------------------------------------

type createSessionRequest struct {
	WorkspaceID string `json:"workspace_id"`
	Runtime     string `json:"runtime"`
	SizeClass   string `json:"size_class"`
}

type sessionResponse struct {
	SessionID   string    `json:"session_id"`
	WorkspaceID string    `json:"workspace_id"`
	Runtime     string    `json:"runtime"`
	SizeClass   string    `json:"size_class"`
	Status      string    `json:"status"`
	HostID      string    `json:"host_id"`
	VMID        string    `json:"vm_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" || req.Runtime == "" {
		jsonError(w, "workspace_id and runtime are required", http.StatusBadRequest)
		return
	}

	size, ok := placer.SizeByName(req.SizeClass)
	if !ok {
		size = placer.Small
	}

	host, err := placer.Pick(r.Context(), s.pool, size)
	if err != nil {
		s.log.Warn("no capacity for session", "workspace_id", req.WorkspaceID, "size", size.Name, "err", err)
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	sessionID := "sess-" + randID(12)
	// Firecracker's --id rejects underscores, so use hyphens throughout.
	vmID := "vm-" + randID(12)

	// Reserve capacity optimistically; release on failure.
	if err := s.pool.Reserve(r.Context(), host.ID, size.MemMiB); err != nil {
		s.log.Error("reserve capacity failed", "host", host.ID, "err", err)
		jsonError(w, "reserve failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sess := &session.Session{
		ID:          sessionID,
		WorkspaceID: req.WorkspaceID,
		Runtime:     req.Runtime,
		SizeClass:   size.Name,
		MemMiB:      size.MemMiB,
		HostID:      host.ID,
		HostBaseURL: host.BaseURL,
		VMID:        vmID,
		Status:      session.StatusStarting,
		CreatedAt:   time.Now(),
	}
	s.sessions.Put(sess)

	client := hostclient.New(host.BaseURL)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	vm, err := client.CreateVM(ctx, hostclient.CreateVMRequest{
		VMID:        vmID,
		Runtime:     req.Runtime,
		WorkspaceID: req.WorkspaceID,
		SizeClass:   size.Name,
	})
	if err != nil {
		s.log.Error("host agent create failed", "host", host.ID, "err", err)
		if relErr := s.pool.Release(r.Context(), host.ID, size.MemMiB); relErr != nil {
			s.log.Warn("release after create failure", "host", host.ID, "err", relErr)
		}
		s.sessions.UpdateStatus(sessionID, session.StatusError)
		jsonError(w, "failed to boot VM: "+err.Error(), http.StatusBadGateway)
		return
	}

	s.sessions.UpdateStatus(sessionID, session.StatusRunning)

	jsonOK(w, sessionResponse{
		SessionID: sessionID, WorkspaceID: req.WorkspaceID,
		Runtime: req.Runtime, SizeClass: size.Name,
		Status: string(session.StatusRunning),
		HostID: host.ID, VMID: vm.VMID,
		CreatedAt: sess.CreatedAt,
	})
}

// --- /v1/sessions/{id} (GET, DELETE) ------------------------------------

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getSession(w, r, id)
	case http.MethodDelete:
		s.deleteSession(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := s.sessions.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, sessionResponse{
		SessionID: sess.ID, WorkspaceID: sess.WorkspaceID,
		Runtime: sess.Runtime, SizeClass: sess.SizeClass,
		Status: string(sess.Status),
		HostID: sess.HostID, VMID: sess.VMID,
		CreatedAt: sess.CreatedAt,
	})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := s.sessions.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	client := hostclient.New(sess.HostBaseURL)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := client.DestroyVM(ctx, sess.VMID); err != nil {
		// Log but proceed — the host may already be gone. Leaving the session
		// in the store with status=error would be worse for the caller.
		s.log.Warn("host agent destroy failed", "host", sess.HostID, "vm_id", sess.VMID, "err", err)
	}

	if err := s.pool.Release(r.Context(), sess.HostID, sess.MemMiB); err != nil {
		s.log.Warn("release on delete", "host", sess.HostID, "err", err)
	}
	s.sessions.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

// --- /v1/hosts (GET) ----------------------------------------------------

type hostResponse struct {
	ID         string    `json:"id"`
	BaseURL    string    `json:"base_url"`
	FreeMemMiB int       `json:"free_mem_mib"`
	VMCount    int       `json:"vm_count"`
	LastSeen   time.Time `json:"last_seen"`
	Healthy    bool      `json:"healthy"`
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	hosts, err := s.pool.All(r.Context())
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]hostResponse, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, hostResponse{
			ID: h.ID, BaseURL: h.BaseURL,
			FreeMemMiB: h.FreeMemMiB, VMCount: len(h.VMs),
			LastSeen: h.LastSeen, Healthy: h.Healthy(now),
		})
	}
	jsonOK(w, map[string]any{"hosts": out})
}

// --- /internal/heartbeat (POST) -----------------------------------------

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var hb hostpool.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		jsonError(w, "invalid heartbeat: "+err.Error(), http.StatusBadRequest)
		return
	}
	if hb.HostID == "" || hb.BaseURL == "" {
		jsonError(w, "host_id and base_url are required", http.StatusBadRequest)
		return
	}
	if err := s.pool.Apply(r.Context(), hb, time.Now()); err != nil {
		s.log.Error("apply heartbeat", "host", hb.HostID, "err", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ------------------------------------------------------------

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func randID(n int) string {
	b := make([]byte, n/2)
	rand.Read(b)
	return hex.EncodeToString(b)
}
