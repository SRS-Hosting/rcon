package config

import (
	"strings"
	"testing"
	"time"
)

func valid() Config {
	return Config{Host: "127.0.0.1", Port: 27015, Password: "secret", TimeoutSeconds: 10}
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestValidateAcceptsEmptyPassword covers servers configured without one. It is
// unusual but legal, and refusing to run would be this tool inventing a rule the
// protocol does not have.
func TestValidateAcceptsEmptyPassword(t *testing.T) {
	cfg := valid()
	cfg.Password = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty password rejected: %v", err)
	}
}

func TestValidateRangeChecks(t *testing.T) {
	tests := []struct {
		name  string
		mutef func(*Config)
		want  string
	}{
		{"port too high", func(c *Config) { c.Port = 70000 }, "port"},
		{"port negative", func(c *Config) { c.Port = -1 }, "port"},
		{"port zero", func(c *Config) { c.Port = 0 }, "port"},
		{"no host", func(c *Config) { c.Host = "" }, "host"},
		{"timeout zero", func(c *Config) { c.TimeoutSeconds = 0 }, "timeoutSeconds"},
		{"timeout negative", func(c *Config) { c.TimeoutSeconds = -1 }, "timeoutSeconds"},
		{"timeout too high", func(c *Config) { c.TimeoutSeconds = 86400 }, "timeoutSeconds"},
		{"address without port", func(c *Config) { c.Address = "127.0.0.1" }, "host:port"},
		{"address without host", func(c *Config) { c.Address = ":27015" }, "no host"},
		{"address with word port", func(c *Config) { c.Address = "host:rcon" }, "non-numeric port"},
		{"address port too high", func(c *Config) { c.Address = "host:70000" }, "between 1 and"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutef(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateIgnoresHostAndPortWhenAddressIsSet keeps the defaults for the
// fields Address replaces from being reported as problems.
func TestValidateIgnoresHostAndPortWhenAddressIsSet(t *testing.T) {
	cfg := Config{Address: "game:7779", Port: 0, TimeoutSeconds: 10}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("address-only config rejected: %v", err)
	}
}

func TestAddr(t *testing.T) {
	cfg := valid()
	if got := cfg.Addr(); got != "127.0.0.1:27015" {
		t.Errorf("Addr() = %q", got)
	}

	// Address wins, so a single host:port can override the pair the sibling
	// services set in the environment.
	cfg.Address = "game:7779"
	if got := cfg.Addr(); got != "game:7779" {
		t.Errorf("Addr() with Address set = %q", got)
	}

	cfg = Config{Host: "::1", Port: 7779}
	if got := cfg.Addr(); got != "[::1]:7779" {
		t.Errorf("Addr() with IPv6 host = %q, want brackets", got)
	}
}

func TestTimeout(t *testing.T) {
	if got := valid().Timeout(); got != 10*time.Second {
		t.Errorf("Timeout() = %s", got)
	}
}
