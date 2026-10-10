// Package heartbeat pings a dead-man's-switch URL (healthchecks.io or
// compatible) so that an outside service notices when the collector, its
// host or its network goes quiet.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Pinger sends heartbeat pings. A nil Pinger is valid and does nothing.
type Pinger struct {
	url    string
	client *http.Client
	logger *slog.Logger
}

// New returns a Pinger for pingURL, or nil when pingURL is empty.
func New(pingURL string, logger *slog.Logger) *Pinger {
	if pingURL == "" {
		return nil
	}
	return &Pinger{
		url:    pingURL,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// Ping sends one heartbeat. Failures are logged, never returned: a missed
// ping is exactly what the receiving service exists to notice.
func (p *Pinger) Ping(ctx context.Context) {
	if p == nil {
		return
	}
	if err := p.ping(ctx); err != nil {
		p.logger.Warn("heartbeat ping failed", "error", err)
	}
}

func (p *Pinger) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return errors.New("invalid heartbeat url")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// The ping URL is a credential, and *url.Error embeds it. Report
		// only the underlying cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat endpoint returned %s", resp.Status)
	}
	return nil
}
