package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestCommitsBehindUpstream(t *testing.T) {
	const metricsFmt = "# TYPE node_nixos_configuration_revision gauge\nnode_nixos_configuration_revision{revision=\"%s\"} 1\n"
	hostRev := "oldsha"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, metricsFmt, hostRev)
	}))
	defer target.Close()

	gitlabUp := true
	var compares atomic.Int32
	gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gitlabUp {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits/main"):
			io.WriteString(w, `{"id":"headsha"}`)
		case strings.HasSuffix(r.URL.Path, "/compare") && r.URL.Query().Get("from") == "oldsha":
			compares.Add(1)
			io.WriteString(w, `{"commits":[{"id":"a"},{"id":"headsha"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gitlab.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		NixOS:    []config.NixOSConfig{{Hostname: "host", URL: target.URL}},
		Upstream: config.UpstreamConfig{GitLabURL: gitlab.URL, Project: "group/nixos", Branch: "main"},
	}
	s, err := New(cfg, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	behind := func() *int { return s.GetMachineState("host").CommitsBehind }

	s.pollAll(context.Background())
	if got := behind(); got == nil || *got != 2 {
		t.Fatalf("commits_behind = %v, want 2", got)
	}
	if snap := s.GetSnapshot(); snap.Upstream == nil || snap.Upstream.Revision != "headsha" || snap.Upstream.Status != StatusOnline {
		t.Errorf("upstream state = %+v, want online at headsha", snap.Upstream)
	}

	// Same head, same revision: answered from the cache.
	s.pollAll(context.Background())
	if compares.Load() != 1 {
		t.Errorf("got %d compare requests, want 1", compares.Load())
	}

	hostRev = "headsha"
	s.pollAll(context.Background())
	if got := behind(); got == nil || *got != 0 {
		t.Errorf("commits_behind = %v, want 0 at head", got)
	}

	// A commit upstream has never seen is unknown, not "behind".
	hostRev = "neverpushed"
	s.pollAll(context.Background())
	if got := behind(); got != nil {
		t.Errorf("commits_behind = %d, want absent for an unknown revision", *got)
	}

	hostRev = "oldsha"
	gitlabUp = false
	s.pollAll(context.Background())
	if got := behind(); got != nil {
		t.Errorf("commits_behind = %d, want absent while upstream is unreachable", *got)
	}
	if got := s.GetSnapshot().Upstream.Status; got != StatusUnreachable {
		t.Errorf("upstream status = %s, want unreachable", got)
	}
}

func TestNoUpstreamConfigured(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "# TYPE node_load1 gauge\nnode_load1 0.5\n")
	}))
	defer target.Close()

	s, _ := newTestScheduler(t, target.URL, true)
	s.pollAll(context.Background())
	if s.GetSnapshot().Upstream != nil {
		t.Error("want no upstream section when none is configured")
	}
	if s.GetMachineState("host").CommitsBehind != nil {
		t.Error("want no commits_behind when no upstream is configured")
	}
}
