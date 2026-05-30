// Package exec runs a shell command and streams its stdout/stderr line by
// line to a caller-provided writer. Each line becomes one NDJSON message,
// so the consumer can render output as it arrives instead of buffering the
// whole run.
package exec

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
)

// Message is one frame on the wire (NDJSON: one Message per line).
type Message struct {
	Type string `json:"type"` // "stdout" | "stderr" | "exit" | "error"
	ID   string `json:"id"`   // exec request id, echoed back so the client can correlate
	Data string `json:"data,omitempty"`
	Code int    `json:"code,omitempty"` // only set on "exit"
}

// safeWriter serialises concurrent writes from the stdout/stderr pumps so
// JSON frames don't interleave.
type safeWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *safeWriter) writeFrame(msg Message) {
	data, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w.Write(data)
	s.w.Write([]byte{'\n'})
}

// Run executes `bash -c <code>` and streams output frames to out as the
// process runs. Returns the exit code (or non-zero on failure to start).
func Run(ctx context.Context, id, code string, out io.Writer) (exitCode int) {
	sw := &safeWriter{w: out}
	cmd := exec.CommandContext(ctx, "bash", "-c", code)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sw.writeFrame(Message{Type: "error", ID: id, Data: err.Error()})
		return 1
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		sw.writeFrame(Message{Type: "error", ID: id, Data: err.Error()})
		return 1
	}

	if err := cmd.Start(); err != nil {
		sw.writeFrame(Message{Type: "error", ID: id, Data: err.Error()})
		return 1
	}

	// Pump stdout and stderr concurrently. Each goroutine writes one frame
	// per line — small enough to feel interactive, large enough to avoid
	// per-byte overhead.
	var wg sync.WaitGroup
	wg.Add(2)
	go pumpLines("stdout", id, stdout, sw, &wg)
	go pumpLines("stderr", id, stderr, sw, &wg)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			sw.writeFrame(Message{Type: "error", ID: id, Data: err.Error()})
			exitCode = 1
		}
	}
	sw.writeFrame(Message{Type: "exit", ID: id, Code: exitCode})
	return exitCode
}

func pumpLines(stream, id string, r io.Reader, sw *safeWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	// Allow longer lines (default is 64 KiB; bump to 1 MiB for big outputs).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		sw.writeFrame(Message{Type: stream, ID: id, Data: scanner.Text() + "\n"})
	}
}
