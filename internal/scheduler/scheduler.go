// Package scheduler runs periodic metric collection and maintains machine state.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ole/dashboard-api/internal/collector"
	"github.com/ole/dashboard-api/internal/config"
	"github.com/ole/dashboard-api/internal/heartbeat"
)

// MachineStatus represents the online/offline state.
type MachineStatus string

const (
	StatusOnline      MachineStatus = "online"
	StatusUnreachable MachineStatus = "unreachable"
	StatusOffline     MachineStatus = "offline"
)

// MachineState holds the current state for one NixOS machine.
type MachineState struct {
	Type     string                  `json:"type"`
	Critical bool                    `json:"critical"`
	Status   MachineStatus           `json:"status"`
	LastSeen time.Time               `json:"last_seen"`
	Metrics  *collector.NixOSMetrics `json:"metrics,omitempty"`
}

// IncusState holds the current state for Incus.
type IncusState struct {
	Status   MachineStatus           `json:"status"`
	LastSeen time.Time               `json:"last_seen"`
	Metrics  *collector.IncusMetrics `json:"metrics,omitempty"`
}

// Snapshot represents the full current state served by the API.
type Snapshot struct {
	Machines map[string]*MachineState `json:"machines"`
	Incus    *IncusState              `json:"incus,omitempty"`
}

// Scheduler periodically polls all targets and maintains in-memory state.
type Scheduler struct {
	cfg         *config.Config
	heartbeat   *heartbeat.Pinger
	logger      *slog.Logger
	httpClient  *http.Client
	incusClient *http.Client

	mu       sync.RWMutex
	machines map[string]*machineTracker
	incus    *incusTracker

	// semaphore limits concurrent polls.
	sem chan struct{}
}

type machineTracker struct {
	cfg       config.NixOSConfig
	state     *MachineState
	failCount int
}

type incusTracker struct {
	state     *IncusState
	failCount int
}

// New creates a scheduler from the given config.
func New(cfg *config.Config, hb *heartbeat.Pinger, logger *slog.Logger) (*Scheduler, error) {
	s := &Scheduler{
		cfg:       cfg,
		heartbeat: hb,
		logger:    logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		machines: make(map[string]*machineTracker),
		sem:      make(chan struct{}, 5),
	}

	// Initialize machine trackers.
	for _, n := range cfg.NixOS {
		s.machines[n.Hostname] = &machineTracker{
			cfg: n,
			state: &MachineState{
				Type:     "nixos",
				Critical: n.Critical,
				Status:   StatusOffline,
			},
		}
	}

	// Initialize Incus tracker if configured.
	if cfg.Incus.URL != "" {
		client, err := collector.NewIncusTLSClient(cfg.Incus.CertFile, cfg.Incus.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("create incus client: %w", err)
		}
		s.incusClient = client
		s.incus = &incusTracker{
			state: &IncusState{
				Status: StatusOffline,
			},
		}
	}

	return s, nil
}

// Run starts the scheduler. It performs an immediate poll, then ticks at the configured interval.
// It blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("scheduler starting, performing initial poll")
	s.pollAll(ctx)

	interval := time.Duration(s.cfg.Server.PollIntervalMinutes) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopping")
			return
		case <-ticker.C:
			s.pollAll(ctx)
		}
	}
}

// GetSnapshot returns a copy of the current state.
func (s *Scheduler) GetSnapshot() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := &Snapshot{
		Machines: make(map[string]*MachineState, len(s.machines)),
	}
	for name, t := range s.machines {
		// Shallow copy is fine; metrics are replaced atomically.
		state := *t.state
		snap.Machines[name] = &state
	}
	if s.incus != nil {
		state := *s.incus.state
		snap.Incus = &state
	}
	return snap
}

// GetMachineState returns the state for a single machine, or nil.
func (s *Scheduler) GetMachineState(hostname string) *MachineState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.machines[hostname]
	if !ok {
		return nil
	}
	state := *t.state
	return &state
}

func (s *Scheduler) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	var reached atomic.Int32

	// Poll NixOS machines.
	for _, t := range s.machines {
		wg.Add(1)
		go func(tracker *machineTracker) {
			defer wg.Done()
			s.sem <- struct{}{}        // acquire
			defer func() { <-s.sem }() // release
			if s.pollNixOS(ctx, tracker) {
				reached.Add(1)
			}
		}(t)
	}

	// Poll Incus.
	if s.incus != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			if s.pollIncus(ctx) {
				reached.Add(1)
			}
		}()
	}

	wg.Wait()
	s.logger.Info("poll cycle complete", "reached", reached.Load())

	// A cycle that reached nothing is not a sign of life: stay silent and
	// let the dead-man's switch fire.
	if reached.Load() > 0 && ctx.Err() == nil {
		s.heartbeat.Ping(ctx)
	}
}

// pollNixOS scrapes one machine and reports whether it answered.
func (s *Scheduler) pollNixOS(ctx context.Context, t *machineTracker) bool {
	pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	metrics, err := collector.ScrapeNodeExporter(
		pollCtx, s.httpClient,
		t.cfg.URL, t.cfg.Username, t.cfg.Password(),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		t.failCount++
		s.logger.Warn("poll failed",
			"hostname", t.cfg.Hostname,
			"error", err,
			"fail_count", t.failCount,
		)

		switch {
		case t.failCount >= 3:
			t.state.Status = StatusOffline
		default:
			t.state.Status = StatusUnreachable
		}
		return false
	}

	// Success.
	t.failCount = 0
	t.state.Status = StatusOnline
	t.state.LastSeen = time.Now()
	t.state.Metrics = metrics
	return true
}

// pollIncus scrapes the Incus metrics endpoint and reports whether it answered.
func (s *Scheduler) pollIncus(ctx context.Context) bool {
	pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	metrics, err := collector.ScrapeIncus(pollCtx, s.incusClient, s.cfg.Incus.URL)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.incus.failCount++
		s.logger.Warn("incus poll failed",
			"error", err,
			"fail_count", s.incus.failCount,
		)
		switch {
		case s.incus.failCount >= 3:
			s.incus.state.Status = StatusOffline
		default:
			s.incus.state.Status = StatusUnreachable
		}
		return false
	}

	s.incus.failCount = 0
	s.incus.state.Status = StatusOnline
	s.incus.state.LastSeen = time.Now()
	s.incus.state.Metrics = metrics
	return true
}
