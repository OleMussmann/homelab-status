package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewWithoutProjectIsNil(t *testing.T) {
	if New("https://gitlab.example", "", "main", "") != nil {
		t.Error("want nil client when no project is configured")
	}
}

func newServer(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		const base = "/api/v4/projects/group%2Fnixos/repository"
		switch r.URL.EscapedPath() {
		case base + "/commits/main":
			io.WriteString(w, `{"id":"headsha","short_id":"heads"}`)
		case base + "/compare":
			if r.URL.Query().Get("to") != "main" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch r.URL.Query().Get("from") {
			case "headsha":
				io.WriteString(w, `{"commits":[]}`)
			case "oldsha":
				io.WriteString(w, `{"commits":[{"id":"a"},{"id":"b"},{"id":"headsha"}]}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHeadAndBehind(t *testing.T) {
	srv := newServer(t, "secret")
	c := New(srv.URL+"/", "group/nixos", "main", "secret")
	ctx := context.Background()

	head, err := c.Head(ctx)
	if err != nil || head != "headsha" {
		t.Fatalf("Head = %q, %v; want headsha", head, err)
	}
	if n, err := c.Behind(ctx, "headsha"); err != nil || n != 0 {
		t.Errorf("Behind(head) = %d, %v; want 0", n, err)
	}
	if n, err := c.Behind(ctx, "oldsha"); err != nil || n != 3 {
		t.Errorf("Behind(old) = %d, %v; want 3", n, err)
	}
	if _, err := c.Behind(ctx, "neverpushed"); !errors.Is(err, ErrUnknownRevision) {
		t.Errorf("Behind(unknown) error = %v, want ErrUnknownRevision", err)
	}
}

func TestWrongTokenIsAnError(t *testing.T) {
	srv := newServer(t, "secret")
	c := New(srv.URL, "group/nixos", "main", "wrong")
	if _, err := c.Head(context.Background()); err == nil {
		t.Error("want an error for a rejected token")
	}
}
