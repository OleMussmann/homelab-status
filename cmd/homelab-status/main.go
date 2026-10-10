// Command dashboard-api is the homelab dashboard backend.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ole/dashboard-api/internal/config"
	"github.com/ole/dashboard-api/internal/heartbeat"
	"github.com/ole/dashboard-api/internal/scheduler"
	"github.com/ole/dashboard-api/internal/server"
)

func main() {
	configPath := flag.String("config", "/config/config.toml", "path to TOML config file")
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz of the running server and exit 0 if healthy, 1 if not")
	flag.Parse()

	// The runtime image is distroless (no shell, no wget), so the container
	// healthcheck has to be this binary.
	if *healthcheck {
		if err := probe(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// probe checks that the server answers GET /healthz with 200. It is liveness
// only: stale metrics are not a reason to restart the collector.
func probe(configPath string) error {
	listen, err := config.ListenAddr(configPath)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned %s", resp.Status)
	}
	return nil
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger.Info("config loaded",
		"listen", cfg.Server.Listen,
		"nixos_machines", len(cfg.NixOS),
		"incus_configured", cfg.Incus.URL != "",
		"heartbeat_configured", cfg.Heartbeat.URL != "",
	)

	// Set up scheduler.
	sched, err := scheduler.New(cfg, heartbeat.New(cfg.Heartbeat.URL, logger), logger)
	if err != nil {
		return fmt.Errorf("create scheduler: %w", err)
	}

	// Set up HTTP server.
	srv := server.New(sched, logger)
	httpServer := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      srv.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Context for graceful shutdown.
	// Incus sends SIGPWR or SIGRTMIN+3 (37) to PID 1 to stop system containers.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGPWR, syscall.Signal(37))
	defer cancel()

	// Start scheduler in background.
	go sched.Run(ctx)

	// Start HTTP server in background.
	go func() {
		logger.Info("http server starting", "addr", cfg.Server.Listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			cancel()
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("shutdown signal received")

	// Graceful shutdown with 5s deadline.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}
