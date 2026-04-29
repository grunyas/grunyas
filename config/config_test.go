package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsInvalidWritePortAddress(t *testing.T) {
	cfg := Default()
	cfg.ServerConfig.Ports["write"] = PortConfig{
		ListenAddr: "invalid",
		PoolMode:   "session",
		SSLMode:    "never",
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid write port listen addr")
	}

	if !strings.Contains(err.Error(), "listen_addr") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsInvalidNodePort(t *testing.T) {
	cfg := Default()
	cfg.Nodes[0].Port = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid node port")
	}

	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRequiresAtLeastOneNode(t *testing.T) {
	cfg := Default()
	cfg.Nodes = []NodeConfig{}

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for empty nodes")
	}

	if !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRequiresExactlyOnePrimary(t *testing.T) {
	cfg := Default()
	// Try to add another primary (should fail validation)
	cfg.Nodes = append(cfg.Nodes, NodeConfig{
		ID:           "primary-2", // This is the bug - should be a second primary
		Host:         "localhost",
		Port:         5433,
		DeclaredRole: "primary", // This should be "primary" to test the validation
		Connection: NodeConnectionConfig{
			User:     "postgres",
			Password: "postgres",
			Database: "postgres",
		},
		Pool: NodePoolConfig{
			MinConns: 2,
			MaxConns: 10,
		},
	})

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for multiple primaries")
	}

	if !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsInvalidRole(t *testing.T) {
	cfg := Default()
	cfg.Nodes[0].DeclaredRole = "invalid"

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid role")
	}

	if !strings.Contains(err.Error(), "role") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsInvalidPoolMode(t *testing.T) {
	cfg := Default()
	w := cfg.ServerConfig.Ports["write"]
	w.PoolMode = "garbage"
	cfg.ServerConfig.Ports["write"] = w

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid pool_mode")
	}
	if !strings.Contains(err.Error(), "pool_mode") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsInvalidSSLMode(t *testing.T) {
	cfg := Default()
	w := cfg.ServerConfig.Ports["write"]
	w.SSLMode = "weird"
	cfg.ServerConfig.Ports["write"] = w

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid ssl_mode")
	}
	if !strings.Contains(err.Error(), "ssl_mode") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRequiresCertWhenSSLMandatory(t *testing.T) {
	cfg := Default()
	w := cfg.ServerConfig.Ports["write"]
	w.SSLMode = "mandatory"
	w.SSLCert = ""
	w.SSLKey = ""
	cfg.ServerConfig.Ports["write"] = w

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for missing certs")
	}
	if !strings.Contains(err.Error(), "ssl_cert") || !strings.Contains(err.Error(), "ssl_key") {
		t.Fatalf("expected both ssl_cert and ssl_key errors, got: %v", err)
	}
}

func TestValidateRejectsMissingWritePort(t *testing.T) {
	cfg := Default()
	delete(cfg.ServerConfig.Ports, "write")

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for missing write port")
	}
	if !strings.Contains(err.Error(), "server.ports.write") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsUpstreamSSLOtherThanDisable(t *testing.T) {
	cfg := Default()
	cfg.Nodes[0].Connection.SSLMode = "require"

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for upstream ssl_mode != disable in M1")
	}
	if !strings.Contains(err.Error(), "ssl_mode") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsBadProbeInterval(t *testing.T) {
	cfg := Default()
	cfg.ProbeConfig.IntervalMs = 50

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for probe interval < 100ms")
	}
	if !strings.Contains(err.Error(), "probe.interval_ms") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsUppercaseNodeID(t *testing.T) {
	cfg := Default()
	cfg.Nodes[0].ID = "Primary-1"

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for uppercase node ID")
	}
	if !strings.Contains(err.Error(), "lowercase URL-safe") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestNormalizePopulatesLegacyShim(t *testing.T) {
	cfg := defaultConfig()
	// Pre-condition: derived fields are zero before Normalize.
	if cfg.ServerConfig.ListenAddr != "" {
		t.Fatalf("expected ListenAddr to be empty before Normalize, got %q", cfg.ServerConfig.ListenAddr)
	}

	cfg.Normalize()

	if cfg.ServerConfig.ListenAddr != "127.0.0.1:5711" {
		t.Fatalf("expected derived ListenAddr=127.0.0.1:5711, got %q", cfg.ServerConfig.ListenAddr)
	}
	if cfg.ServerConfig.PoolMode != PoolModeSession {
		t.Fatalf("expected derived PoolMode=session, got %q", cfg.ServerConfig.PoolMode)
	}
}

func TestPoolModeFromString(t *testing.T) {
	cases := []struct {
		in    string
		want  PoolMode
		valid bool
	}{
		{"", PoolModeSession, true},
		{"session", PoolModeSession, true},
		{"transaction", PoolModeTransaction, true},
		{"garbage", "", false},
		{"SESSION", "", false}, // case-sensitive
	}
	for _, c := range cases {
		got, ok := PoolModeFromString(c.in)
		if got != c.want || ok != c.valid {
			t.Errorf("PoolModeFromString(%q) = (%q, %v); want (%q, %v)", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestValidateRejectsDuplicateNodeIDs(t *testing.T) {
	cfg := Default()
	cfg.Nodes = append(cfg.Nodes, NodeConfig{
		ID:           "primary-1", // duplicate
		Host:         "localhost",
		Port:         5433,
		DeclaredRole: "replica",
		Connection: NodeConnectionConfig{
			User:     "postgres",
			Password: "postgres",
			Database: "postgres",
		},
		Pool: NodePoolConfig{
			MinConns: 2,
			MaxConns: 10,
		},
	})

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for duplicate node IDs")
	}

	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
