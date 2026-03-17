// Command dashboard-api is the homelab dashboard backend.
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

	"github.com/ole/dashboard-api/internal/alerter"
	"github.com/ole/dashboard-api/internal/config"
	"github.com/ole/dashboard-api/internal/scheduler"
	"github.com/ole/dashboard-api/internal/server"
)

func main() {
	configPath := flag.String("config", "/config/config.toml", "path to TOML config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
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
		"alerting_enabled", cfg.Alerting.Enabled,
	)

	// Set up alerter.
	var alert *alerter.Alerter
	if cfg.Alerting.Enabled {
		alert = alerter.New(cfg.Alerting.NtfyURL, cfg.Alerting.CooldownMinutes, logger)
		logger.Info("alerting enabled", "ntfy_url", cfg.Alerting.NtfyURL)
	} else {
		alert = alerter.New("", 0, logger) // disabled no-op alerter
	}

	// Set up scheduler.
	sched, err := scheduler.New(cfg, alert, logger)
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
