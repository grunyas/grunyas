package config

// ServerConfig holds server-wide settings and port definitions.
type ServerConfig struct {
	AdminAddr string `mapstructure:"admin_addr"` // Formatted as host:port

	// MaxSessions is the maximum number of concurrent client sessions allowed.
	// If zero, there is no limit.
	MaxSessions int `mapstructure:"max_sessions"`

	// ClientIdleTimeout, in seconds, is the duration a client can stay connected without any activity
	// (no queries sent to the server) before the connection is closed. If zero (default),
	// idle connections are not closed.
	ClientIdleTimeout int `mapstructure:"client_idle_timeout"`

	// KeepAliveTimeout, in seconds, is the time that the connection must be idle before
	// the first keep-alive probe is sent.
	// If zero, a default value of 15 seconds is used.
	KeepAliveTimeout int `mapstructure:"keep_alive_timeout"`

	// KeepAliveInterval, in seconds, is the time between keep-alive probes.
	// If zero, a default value of 15 seconds is used.
	KeepAliveInterval int `mapstructure:"keep_alive_interval"`

	// KeepAliveCount is the maximum number of keep-alive probes that
	// can go unanswered before dropping a connection.
	// If zero, a default value of 9 is used.
	KeepAliveCount int `mapstructure:"keep_alive_count"`

	// StartupProbeTimeoutSeconds is the maximum time (in seconds) to wait for the first
	// probe cycle on all nodes during startup. If zero, defaults to 10 seconds.
	StartupProbeTimeoutSeconds int `mapstructure:"startup_probe_timeout_seconds"`

	// Ports holds configuration for each listen port (write, read, compat).
	Ports map[string]PortConfig `mapstructure:"ports"` // Keys: "write", "read", "compat"

	// PprofAddr enables the Go pprof HTTP server on the given address.
	// Example: "0.0.0.0:6060". If empty (default), pprof is disabled.
	PprofAddr string `mapstructure:"pprof_addr"`

	// --- Derived legacy fields (M1 shim) -----------------------------------
	// These mirror the write-port settings so existing call sites that have
	// not yet been migrated to per-port lookups keep compiling. Populated by
	// Config.Normalize() after Unmarshal. Not read from TOML/env directly.
	// Removed in M1 PR 2 once call sites use Topology + per-port routing.
	ListenAddr string   `mapstructure:"-"`
	PoolMode   PoolMode `mapstructure:"-"`
	SSLMode    string   `mapstructure:"-"`
	SSLCert    string   `mapstructure:"-"`
	SSLKey     string   `mapstructure:"-"`
}

// GetWritePort returns the write port configuration.
// Returns an empty PortConfig and false if the write port is not configured.
func (s ServerConfig) GetWritePort() (PortConfig, bool) {
	if s.Ports == nil {
		return PortConfig{}, false
	}
	port, ok := s.Ports["write"]
	return port, ok
}

// GetWritePoolMode returns the pool mode for the write port.
// Defaults to "session" if not configured or invalid.
func (s ServerConfig) GetWritePoolMode() PoolMode {
	if port, ok := s.GetWritePort(); ok {
		if m, ok := PoolModeFromString(port.PoolMode); ok {
			return m
		}
	}
	return PoolModeSession // default
}

// PoolMode represents the connection pooling mode.
type PoolMode string

const (
	PoolModeSession     PoolMode = "session"
	PoolModeTransaction PoolMode = "transaction"
)

// PoolModeFromString parses a string into a PoolMode.
// Returns ("", false) for unknown values so the validator can detect them.
// Empty string is treated as a known value (session default).
func PoolModeFromString(s string) (PoolMode, bool) {
	switch s {
	case "":
		return PoolModeSession, true // default
	case "session":
		return PoolModeSession, true
	case "transaction":
		return PoolModeTransaction, true
	default:
		return "", false
	}
}
