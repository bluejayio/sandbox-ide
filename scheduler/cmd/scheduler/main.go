package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sandbox-ide/scheduler/internal/api"
	"github.com/sandbox-ide/scheduler/internal/hostpool"
	"github.com/sandbox-ide/scheduler/internal/session"
)

func main() {
	listenAddr := flag.String("addr", ":9090", "scheduler listen address")
	backend := flag.String("backend", "memory", "host registry backend: memory | redis")
	redisURL := flag.String("redis-url", "redis://localhost:6379/0", "Redis connection URL (used when --backend=redis)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	pool, err := buildPool(*backend, *redisURL, log)
	if err != nil {
		log.Error("hostpool init failed", "err", err)
		os.Exit(2)
	}

	sessions := session.NewStore()
	srv := api.NewServer(pool, sessions, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	httpServer := &http.Server{
		Addr:         *listenAddr,
		Handler:      srv,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 120 * time.Second, // VM boot may be slow
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info("scheduler started", "addr", *listenAddr, "backend", *backend)
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

func buildPool(backend, redisURL string, log *slog.Logger) (hostpool.Pool, error) {
	switch backend {
	case "memory":
		return hostpool.NewInMemory(), nil
	case "redis":
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			return nil, fmt.Errorf("parse redis URL: %w", err)
		}
		rdb := redis.NewClient(opts)
		// Fail fast if Redis is unreachable at startup.
		pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			return nil, fmt.Errorf("redis ping: %w", err)
		}
		log.Info("redis connected", "url", redisURL)
		return hostpool.NewRedis(rdb), nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want memory | redis)", backend)
	}
}
