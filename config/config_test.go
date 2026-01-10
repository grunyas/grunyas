package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsInvalidAddresses(t *testing.T) {
	cfg := Default()
	cfg.ServerConfig.ListenAddr = "invalid"

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid listen addr")
	}

	if !strings.Contains(err.Error(), "server.listen_addr") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestValidateRejectsInvalidBackendPort(t *testing.T) {
	cfg := Default()
	cfg.BackendConfig.DatabasePort = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected validation error for invalid backend port")
	}

	if !strings.Contains(err.Error(), "backend.port") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
