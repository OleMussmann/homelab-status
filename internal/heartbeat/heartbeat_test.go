package heartbeat

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNilPingerIsNoOp(t *testing.T) {
	if p := New("", slog.Default()); p != nil {
		t.Fatal("empty URL must yield a nil Pinger")
	}
	var p *Pinger
	p.Ping(context.Background()) // must not panic
}

func TestPingHitsURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/secret-uuid" {
			hits.Add(1)
		}
	}))
	defer srv.Close()

	New(srv.URL+"/secret-uuid", slog.Default()).Ping(context.Background())
	if hits.Load() != 1 {
		t.Errorf("got %d pings, want 1", hits.Load())
	}
}

func TestFailureIsLoggedWithoutURL(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		close   bool
	}{
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, false},
		{"connection refused", func(w http.ResponseWriter, r *http.Request) {}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			pingURL := srv.URL + "/secret-uuid"
			if tt.close {
				srv.Close()
			} else {
				defer srv.Close()
			}

			var buf bytes.Buffer
			New(pingURL, slog.New(slog.NewTextHandler(&buf, nil))).Ping(context.Background())

			out := buf.String()
			if !strings.Contains(out, "heartbeat ping failed") {
				t.Errorf("failure not logged: %q", out)
			}
			if strings.Contains(out, "secret-uuid") {
				t.Errorf("ping URL leaked into the log: %q", out)
			}
		})
	}
}
