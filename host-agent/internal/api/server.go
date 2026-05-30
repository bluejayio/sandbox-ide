package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sandbox-ide/host-agent/internal/vm"
)

// Server exposes the host agent's HTTP API consumed by the scheduler.
//
// Routes:
//
//	POST   /vms                      create and boot a new VM
//	DELETE /vms/{id}                 destroy a VM
//	POST   /vms/{id}/snapshot        pause and snapshot a VM
//	POST   /vms/restore              restore a VM from snapshot
//	GET    /heartbeat                host capacity + VM inventory
type Server struct {
	mgr          *vm.Manager
	log          *slog.Logger
	mux          *http.ServeMux
	hostID       string // identifies this host to the scheduler
	advertiseURL string // URL the scheduler uses to reach this agent
}

func NewServer(mgr *vm.Manager, log *slog.Logger, hostID, advertiseURL string) *Server {
	s := &Server{
		mgr: mgr, log: log, mux: http.NewServeMux(),
		hostID: hostID, advertiseURL: advertiseURL,
	}
	s.mux.HandleFunc("/vms", s.handleCreateVM)
	s.mux.HandleFunc("/vms/restore", s.handleRestore)
	s.mux.HandleFunc("/vms/", s.handleVM) // /vms/{id} and /vms/{id}/snapshot
	s.mux.HandleFunc("/heartbeat", s.handleHeartbeat)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.mux.ServeHTTP(w, r)
	s.log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
}

// --- /vms ---------------------------------------------------------------

type createVMRequest struct {
	VMID        string `json:"vm_id"`
	Runtime     string `json:"runtime"`
	WorkspaceID string `json:"workspace_id"`
	SizeClass   string `json:"size_class"` // "small" | "medium" | "large"
}

type vmResponse struct {
	VMID      string    `json:"vm_id"`
	Status    string    `json:"status"`
	GuestCID  int       `json:"guest_cid"`
	TAPName   string    `json:"tap_name"`
	StartedAt time.Time `json:"started_at"`
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req createVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.VMID == "" || req.Runtime == "" || req.WorkspaceID == "" {
		jsonError(w, "vm_id, runtime, workspace_id are required", http.StatusBadRequest)
		return
	}

	size, ok := vm.SizeByName(req.SizeClass)
	if !ok {
		size = vm.Small
	}

	v, err := s.mgr.Create(r.Context(), req.VMID, req.Runtime, req.WorkspaceID, size)
	if err != nil {
		s.log.Error("create vm failed", "vm_id", req.VMID, "err", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, vmResponse{
		VMID: v.ID, Status: string(v.Status),
		GuestCID: v.GuestCID, TAPName: v.TAPName, StartedAt: v.StartedAt,
	})
}

// --- /vms/{id} and /vms/{id}/snapshot -----------------------------------

func (s *Server) handleVM(w http.ResponseWriter, r *http.Request) {
	// Strip leading /vms/ and split on /
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/vms/"), "/", 2)
	vmID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodDelete && action == "":
		s.destroyVM(w, r, vmID)
	case r.Method == http.MethodPost && action == "snapshot":
		s.snapshotVM(w, r, vmID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) destroyVM(w http.ResponseWriter, r *http.Request, vmID string) {
	if err := s.mgr.Destroy(r.Context(), vmID); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type snapshotResponse struct {
	VMID         string `json:"vm_id"`
	SnapshotPath string `json:"snapshot_path"`
	MemPath      string `json:"mem_path"`
}

func (s *Server) snapshotVM(w http.ResponseWriter, r *http.Request, vmID string) {
	snapPath, memPath, err := s.mgr.Snapshot(r.Context(), vmID)
	if err != nil {
		s.log.Error("snapshot failed", "vm_id", vmID, "err", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, snapshotResponse{VMID: vmID, SnapshotPath: snapPath, MemPath: memPath})
}

// --- /vms/restore -------------------------------------------------------

type restoreRequest struct {
	VMID         string `json:"vm_id"`
	SnapshotPath string `json:"snapshot_path"`
	MemPath      string `json:"mem_path"`
	Runtime      string `json:"runtime"`
	SizeClass    string `json:"size_class"`
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	size, ok := vm.SizeByName(req.SizeClass)
	if !ok {
		size = vm.Small
	}

	v, err := s.mgr.Restore(r.Context(), req.VMID, req.SnapshotPath, req.MemPath, req.Runtime, size)
	if err != nil {
		s.log.Error("restore failed", "vm_id", req.VMID, "err", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, vmResponse{
		VMID: v.ID, Status: string(v.Status),
		GuestCID: v.GuestCID, TAPName: v.TAPName, StartedAt: v.StartedAt,
	})
}

// --- /heartbeat ---------------------------------------------------------

type heartbeatResponse struct {
	HostID     string    `json:"host_id"`
	BaseURL    string    `json:"base_url"`
	FreeMemMiB int       `json:"free_mem_mib"`
	VMs        []vmSlot  `json:"vms"`
	Timestamp  time.Time `json:"timestamp"`
}

type vmSlot struct {
	VMID        string `json:"vm_id"`
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id"`
	Runtime     string `json:"runtime"`
	SizeClass   string `json:"size_class"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vms := s.mgr.List()
	slots := make([]vmSlot, 0, len(vms))
	for _, v := range vms {
		slots = append(slots, vmSlot{
			VMID: v.ID, Status: string(v.Status),
			WorkspaceID: v.WorkspaceID, Runtime: v.Runtime,
			SizeClass: v.Size.Name,
		})
	}

	jsonOK(w, heartbeatResponse{
		HostID:     s.hostID,
		BaseURL:    s.advertiseURL,
		FreeMemMiB: s.mgr.FreeMemMiB(),
		VMs:        slots,
		Timestamp:  time.Now(),
	})
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

// Heartbeat sends a periodic capacity report to the scheduler.
func (s *Server) Heartbeat(ctx context.Context, schedulerURL string, interval time.Duration) {
	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			vms := s.mgr.List()
			slots := make([]vmSlot, 0, len(vms))
			for _, v := range vms {
				slots = append(slots, vmSlot{
					VMID: v.ID, Status: string(v.Status),
					WorkspaceID: v.WorkspaceID, Runtime: v.Runtime,
					SizeClass: v.Size.Name,
				})
			}
			payload, _ := json.Marshal(heartbeatResponse{
				HostID:     s.hostID,
				BaseURL:    s.advertiseURL,
				FreeMemMiB: s.mgr.FreeMemMiB(),
				VMs:        slots,
				Timestamp:  time.Now(),
			})

			resp, err := client.Post(schedulerURL+"/internal/heartbeat", "application/json",
				strings.NewReader(string(payload)))
			if err != nil {
				s.log.Warn("heartbeat failed", "err", err)
				continue
			}
			resp.Body.Close()
		}
	}
}
