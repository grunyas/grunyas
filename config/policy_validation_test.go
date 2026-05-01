package config

import (
	"strings"
	"testing"
)

func TestValidateCompatSessionRejected(t *testing.T) {
	cfg := Default()
	port := cfg.ServerConfig.Ports["compat"]
	if port.ListenAddr == "" {
		// Ensure compat port exists
		cfg.ServerConfig.Ports["compat"] = PortConfig{
			ListenAddr: "127.0.0.1:5714",
			PoolMode:   "session",
			SSLMode:    "never",
		}
	} else {
		port.PoolMode = "session"
		cfg.ServerConfig.Ports["compat"] = port
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected compat x session to be rejected")
	}
	if !strings.Contains(err.Error(), "ports.compat.pool_mode") {
		t.Fatalf("expected error about compat pool_mode, got: %v", err)
	}
}

func TestValidateDwellBelowProbeIntervalRejected(t *testing.T) {
	cfg := Default()
	cfg.ProbeConfig.IntervalMs = 1000
	cfg.Policies = []PolicyConfig{
		{
			Name:     "tight-lag",
			Template: "lag-filter",
			Scope:    "cluster",
			Timing:   PolicyTimingConfig{DwellMs: 500, ReleaseMs: 5000},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected dwell < probe interval to be rejected")
	}
	if !strings.Contains(err.Error(), "dwell_ms") {
		t.Fatalf("expected error about dwell_ms, got: %v", err)
	}
}

func TestValidateDuplicatePolicyNameRejected(t *testing.T) {
	cfg := Default()
	cfg.Policies = []PolicyConfig{
		{
			Name:     "dup",
			Template: "health-filter",
			Scope:    "cluster",
			Timing:   PolicyTimingConfig{DwellMs: 5000, ReleaseMs: 5000},
		},
		{
			Name:     "dup",
			Template: "lag-filter",
			Scope:    "cluster",
			Timing:   PolicyTimingConfig{DwellMs: 5000, ReleaseMs: 5000},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected duplicate policy name to be rejected")
	}
	if !strings.Contains(err.Error(), "'dup' is duplicate") {
		t.Fatalf("expected duplicate name error, got: %v", err)
	}
}

func TestValidateValidPoliciesPassed(t *testing.T) {
	cfg := Default()
	cfg.ProbeConfig.IntervalMs = 1000
	cfg.Policies = []PolicyConfig{
		{
			Name:     "custom-lag",
			Template: "lag-filter",
			Scope:    "cluster",
			Timing:   PolicyTimingConfig{DwellMs: 2000, ReleaseMs: 10000},
			Parameters: map[string]interface{}{
				"threshold_ms": float64(50),
			},
		},
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("valid policy config should pass validation: %v", err)
	}
}
