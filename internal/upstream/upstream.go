// Package upstream asks the GitLab project that holds the NixOS
// configuration where its branch is, and how far behind it a given commit
// is. That is what tells "nothing to deploy" from "stopped deploying".
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnknownRevision is returned when the project does not know a commit,
// for example a system built from a commit that was never pushed.
var ErrUnknownRevision = errors.New("revision unknown to upstream")

// Client queries one branch of one GitLab project. A nil Client is valid;
// callers check for it before use.
type Client struct {
	apiBase string
	branch  string
	token   string
	client  *http.Client
}

// New returns a Client, or nil when project is empty. gitlabURL is the
// instance root (https://gitlab.com). token may be empty for a public
// project.
func New(gitlabURL, project, branch, token string) *Client {
	if project == "" {
		return nil
	}
	return &Client{
		apiBase: strings.TrimRight(gitlabURL, "/") + "/api/v4/projects/" + url.PathEscape(project) + "/repository",
		branch:  branch,
		token:   token,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Head returns the commit the branch points at.
func (c *Client) Head(ctx context.Context) (string, error) {
	var commit struct {
		ID string `json:"id"`
	}
	if err := c.get(ctx, "/commits/"+url.PathEscape(c.branch), &commit); err != nil {
		return "", err
	}
	if commit.ID == "" {
		return "", errors.New("upstream returned no commit id")
	}
	return commit.ID, nil
}

// Behind returns how many commits of the branch are missing from rev. It is
// 0 when rev is the head or ahead of it.
func (c *Client) Behind(ctx context.Context, rev string) (int, error) {
	var cmp struct {
		Commits []json.RawMessage `json:"commits"`
	}
	q := url.Values{"from": {rev}, "to": {c.branch}}
	if err := c.get(ctx, "/compare?"+q.Encode(), &cmp); err != nil {
		return 0, err
	}
	return len(cmp.Commits), nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("PRIVATE-TOKEN", c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("query upstream: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// GitLab answers 404 for an unknown commit, and also for a project
		// the token cannot see. Head() failing the same way tells them apart.
		return ErrUnknownRevision
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("upstream returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read upstream response: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode upstream response: %w", err)
	}
	return nil
}
