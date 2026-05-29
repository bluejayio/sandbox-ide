package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sandbox-ide/host-agent/internal/api"
	"github.com/sandbox-ide/host-agent/internal/vm"
)

func main() {
	var (
		listenAddr   = flag.String("addr", ":8080", "host agent listen address")
		schedulerURL = flag.String("scheduler", "", "scheduler base URL for heartbeats (e.g. http://scheduler:9090)")
		kernelPath   = flag.String("kernel", "/opt/firecracker/vmlinux", "path to guest kernel image")
		baseImageDir = flag.String("base-images", "/opt/firecracker/images", "directory of base rootfs images")
		snapshotDir  = flag.String("snapshots", "/var/lib/agent/snapshots", "snapshot storage directory")
		socketDir    = flag.String("sockets", "/var/run/agent/vms", "VM socket and overlay directory")
		logDir       = flag.String("logs", "/var/log/agent/vms", "per-VM log directory")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mgr := vm.NewManager(vm.Config{
		KernelPath:   *kernelPath,
		BaseImageDir: *baseImageDir,
		SnapshotDir:  *snapshotDir,
		SocketDir:    *socketDir,
		LogDir:       *logDir,
	})

	srv := api.NewServer(mgr, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Send heartbeats to the scheduler every 10 seconds if a URL was provided.
	if *schedulerURL != "" {
		go api.Heartbeat(ctx, mgr, *schedulerURL, 10*time.Second, log)
	}

	httpServer := &http.Server{
		Addr:         *listenAddr,
		Handler:      srv,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // snapshot/restore can be slow
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info("host agent started", "addr", *listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	httpServer.Shutdown(shutdownCtx)
}
