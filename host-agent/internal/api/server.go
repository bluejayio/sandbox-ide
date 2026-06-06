package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sandbox-ide/host-agent/internal/vm"
	"github.com/sandbox-ide/host-agent/internal/vsock"
)

// vsockGuestPort is the port vm-agent listens on inside every guest.
// Matches the --port default in vm-agent/cmd/vm-agent/main.go.
const vsockGuestPort = 5252

// Server exposes the host agent's HTTP API consumed by the scheduler.
//
// Routes:
//
//	POST   /vms                      create and boot a new VM
//	DELETE /vms/{id}                 destroy a VM
//	POST   /vms/{id}/snapshot        pause and snapshot a VM
//	POST   /vms/{id}/exec            stream an exec request to the guest's vm-agent
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
	case r.Method == http.MethodPost && action == "exec":
		s.execVM(w, r, vmID)
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

// --- /vms/{id}/exec -----------------------------------------------------
//
// The caller POSTs one or more NDJSON exec requests in the body; the host
// agent opens a vsock channel to vm-agent inside the guest, pipes the
// request body in, and streams the NDJSON response back chunk-by-chunk.
// The HTTP response uses Transfer-Encoding: chunked with a Flusher after
// each frame so callers see output as it's produced, not after the run.

func (s *Server) execVM(w http.ResponseWriter, r *http.Request, vmID string) {
	v, ok := s.mgr.Get(vmID)
	if !ok {
		jsonError(w, "vm not found", http.StatusNotFound)
		return
	}
	if v.VsockPath == "" {
		jsonError(w, "vm has no vsock channel", http.StatusConflict)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// All net/http servers expose Flusher; this is defensive only.
		jsonError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Read the request body upfront. Exec requests are small (one NDJSON
	// line per call). Reading first avoids a race where a goroutine reads
	// r.Body after the handler returns and net/http has already closed it.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	conn, err := vsock.Dial(v.VsockPath, vsockGuestPort)
	if err != nil {
		s.log.Error("vsock dial failed", "vm_id", vmID, "err", err)
		jsonError(w, "vsock dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer conn.Close()

	if _, err := conn.Write(body); err != nil {
		s.log.Error("vsock write failed", "vm_id", vmID, "err", err)
		jsonError(w, "vsock write: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Stream guest output until vm-agent emits its {"type":"exit"} frame.
	// We can't half-close to signal end-of-requests because Firecracker's
	// UDS proxy tears down the vsock bridge on host CloseWrite, dropping
	// in-flight output. The protocol itself tells us when we're done:
	// vm-agent always sends exactly one exit frame as its last line.
	if err := streamUntilExit(w, flusher, conn); err != nil && err != io.EOF {
		s.log.Warn("exec stream ended", "vm_id", vmID, "err", err)
	}
}

// streamUntilExit copies NDJSON frames from src to w line-by-line, flushing
// after each frame. Returns once a line whose decoded "type" field is
// "exit" has been forwarded, or on error/EOF.
func streamUntilExit(w io.Writer, f http.Flusher, src io.Reader) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		f.Flush()

		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Type == "exit" {
			return nil
		}
	}
	return scanner.Err()
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
	GuestCID    int    `json:"guest_cid"` // needed by the scheduler's stream relay to dial vsock
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
			GuestCID:  v.GuestCID,
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
					GuestCID:  v.GuestCID,
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
