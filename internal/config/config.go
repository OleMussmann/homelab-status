// Package config loads and validates the dashboard-api TOML configuration.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Incus    IncusConfig    `toml:"incus"`
	Alerting AlertingConfig `toml:"alerting"`
	NixOS    []NixOSConfig  `toml:"nixos"`
}

// ServerConfig controls the HTTP listener and global poll interval.
type ServerConfig struct {
	Listen              string `toml:"listen"`
	PollIntervalMinutes int    `toml:"poll_interval_minutes"`
}

// IncusConfig describes how to reach the Incus metrics endpoint.
type IncusConfig struct {
	URL      string `toml:"url"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

// AlertingConfig controls ntfy.sh push notifications.
type AlertingConfig struct {
	Enabled         bool          `toml:"enabled"`
	NtfyURL         string        `toml:"ntfy_url"`
	CooldownMinutes int           `toml:"cooldown_minutes"`
	Rules           AlertingRules `toml:"rules"`
}

// AlertingRules defines which conditions trigger alerts.
type AlertingRules struct {
	MachineOffline       bool `toml:"machine_offline"`
	SMARTFailure         bool `toml:"smart_failure"`
	OOMKill              bool `toml:"oom_kill"`
	HighDiskUsagePercent int  `toml:"high_disk_usage_percent"`
	FailedServices       bool `toml:"failed_services"`
	BorgStaleHours       int  `toml:"borg_stale_hours"`
}

// NixOSConfig describes a single monitored NixOS machine.
type NixOSConfig struct {
	Hostname     string `toml:"hostname"`
	URL          string `toml:"url"`
	Username     string `toml:"username"`
	PasswordFile string `toml:"password_file"`
	Critical     bool   `toml:"critical"`

	// password is the resolved value read from PasswordFile at load time.
	password string
}

// DefaultListen is the listen address used when [server] listen is unset.
const DefaultListen = "0.0.0.0:8080"

// Load reads and parses a TOML config file, applies defaults, and validates.
func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	// Apply defaults.
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = DefaultListen
	}
	if cfg.Server.PollIntervalMinutes <= 0 {
		cfg.Server.PollIntervalMinutes = 5
	}
	if cfg.Alerting.CooldownMinutes <= 0 {
		cfg.Alerting.CooldownMinutes = 15
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	// Pre-load password files so we fail fast on startup.
	for i := range cfg.NixOS {
		if cfg.NixOS[i].PasswordFile != "" {
			pw, err := readSecretFile(cfg.NixOS[i].PasswordFile)
			if err != nil {
				return nil, fmt.Errorf("nixos %s password: %w", cfg.NixOS[i].Hostname, err)
			}
			cfg.NixOS[i].password = pw
		}
	}

	return &cfg, nil
}

// ListenAddr returns only the [server] listen address from the config file,
// with the default applied. Unlike Load it neither validates nor reads
// password files, so a health probe does not fail on an unrelated secret.
func ListenAddr(path string) (string, error) {
	var cfg struct {
		Server ServerConfig `toml:"server"`
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return "", fmt.Errorf("decode config %s: %w", path, err)
	}
	if cfg.Server.Listen == "" {
		return DefaultListen, nil
	}
	return cfg.Server.Listen, nil
}

// Password returns the resolved password for a NixOS machine.
func (n *NixOSConfig) Password() string {
	return n.password
}

func (c *Config) validate() error {
	if len(c.NixOS) == 0 && c.Incus.URL == "" {
		return fmt.Errorf("at least one [[nixos]] entry or [incus] URL must be configured")
	}
	for i, n := range c.NixOS {
		if n.Hostname == "" {
			return fmt.Errorf("nixos[%d]: hostname is required", i)
		}
		if n.URL == "" {
			return fmt.Errorf("nixos[%d] (%s): url is required", i, n.Hostname)
		}
	}
	if c.Alerting.Enabled && c.Alerting.NtfyURL == "" {
		return fmt.Errorf("alerting.ntfy_url is required when alerting is enabled")
	}
	return nil
}

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
