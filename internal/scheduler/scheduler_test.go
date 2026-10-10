package scheduler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ole/dashboard-api/internal/config"
	"github.com/ole/dashboard-api/internal/heartbeat"
)

// newTestScheduler returns a scheduler with one NixOS target at targetURL and
// a heartbeat endpoint that counts pings.
func newTestScheduler(t *testing.T, targetURL string, critical bool) (*Scheduler, *atomic.Int32) {
	t.Helper()
	var pings atomic.Int32
	hb := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { pings.Add(1) }))
	t.Cleanup(hb.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		NixOS: []config.NixOSConfig{{Hostname: "host", URL: targetURL, Critical: critical}},
	}
	s, err := New(cfg, heartbeat.New(hb.URL, logger), logger)
	if err != nil {
		t.Fatal(err)
	}
	return s, &pings
}

func TestHeartbeatAfterCycleThatReachedATarget(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "# TYPE node_load1 gauge\nnode_load1 0.5\n")
	}))
	defer target.Close()

	s, pings := newTestScheduler(t, target.URL, true)
	s.pollAll(context.Background())

	if pings.Load() != 1 {
		t.Errorf("got %d pings, want 1", pings.Load())
	}
	state := s.GetMachineState("host")
	if state.Status != StatusOnline {
		t.Errorf("status = %s, want online", state.Status)
	}
	if !state.Critical {
		t.Error("critical flag not passed through to the machine state")
	}
}

func TestNoHeartbeatWhenNothingAnswered(t *testing.T) {
	target := httptest.NewServer(http.NotFoundHandler())
	target.Close() // connection refused

	s, pings := newTestScheduler(t, target.URL, false)
	s.pollAll(context.Background())

	if pings.Load() != 0 {
		t.Errorf("got %d pings, want 0", pings.Load())
	}
	if got := s.GetMachineState("host").Status; got != StatusUnreachable {
		t.Errorf("status = %s, want unreachable", got)
	}
}
