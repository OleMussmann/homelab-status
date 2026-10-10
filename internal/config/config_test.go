package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListenAddr(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"set", "[server]\nlisten = \"127.0.0.1:9000\"\n", "127.0.0.1:9000"},
		{"unset", "[server]\npoll_interval_minutes = 5\n", DefaultListen},
		{"no server section", "", DefaultListen},
		// A missing password file would make Load fail; ListenAddr must not care.
		{"missing secret", "[server]\nlisten = \":8080\"\n\n[[nixos]]\nhostname = \"x\"\npassword_file = \"/nonexistent\"\n", ":8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ListenAddr(writeConfig(t, tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListenAddrMissingFile(t *testing.T) {
	if _, err := ListenAddr(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Error("expected an error for a missing config file")
	}
}
