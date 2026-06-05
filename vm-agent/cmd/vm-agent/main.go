// vm-agent is a small daemon that runs inside each guest microVM. It listens on a vsock port for
// exec requests from the host agent and streams stdout/stderr back as
// NDJSON frames.
//
// Wire protocol (each frame is one JSON object on its own line):
//
//	Client → vm-agent:  {"type":"exec","id":"e1","code":"echo hi"}
//	vm-agent → client:  {"type":"stdout","id":"e1","data":"hi\n"}
//	vm-agent → client:  {"type":"exit","id":"e1","code":0}
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/mdlayher/vsock"

	vmexec "github.com/sandbox-ide/vm-agent/internal/exec"
)

// request is what the host sends to the vm-agent.
type request struct {
	Type string `json:"type"` // "exec"
	ID   string `json:"id"`
	Code string `json:"code"`
}

func main() {
	port := flag.Uint("port", 5252, "vsock port to listen on")
	flag.Parse()

	logger := log.New(os.Stderr, "vm-agent ", log.LstdFlags|log.Lmicroseconds)

	listener, err := vsock.Listen(uint32(*port), nil)
	if err != nil {
		logger.Fatalf("vsock listen on port %d: %v", *port, err)
	}
	defer listener.Close()
	logger.Printf("listening on vsock:%d", *port)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Printf("accept: %v", err)
			continue
		}
		go handle(ctx, conn, logger)
	}
}

func handle(ctx context.Context, conn net.Conn, logger *log.Logger) {
	defer conn.Close()
	logger.Printf("client connected: %s", conn.RemoteAddr())

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			logger.Printf("bad request: %v", err)
			continue
		}
		if req.Type != "exec" {
			logger.Printf("unknown request type: %q", req.Type)
			continue
		}
		// Each exec runs synchronously on this connection so output
		// stays in order. If we wanted concurrent execs we'd need
		// per-exec frame ordering anyway, so serial is simpler for v1.
		vmexec.Run(ctx, req.ID, req.Code, conn)
	}
	if err := scanner.Err(); err != nil {
		logger.Printf("client read: %v", err)
	}
}
