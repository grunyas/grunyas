package config

// ServerConfig holds server-wide settings and port definitions.
type ServerConfig struct {
	// Admin holds the admin HTTP API server configuration (M2+).
	Admin AdminConfig `mapstructure:"admin" json:"admin"`

	// MaxSessions is the maximum number of concurrent client sessions allowed.
	MaxSessions int `mapstructure:"max_sessions" json:"max_sessions"`

	// ClientIdleTimeout, in seconds, is the duration a client can stay connected
	// without any activity before the connection is closed.
	ClientIdleTimeout int `mapstructure:"client_idle_timeout" json:"client_idle_timeout"`

	// KeepAliveTimeout, in seconds, is the idle time before the first keep-alive probe.
	KeepAliveTimeout int `mapstructure:"keep_alive_timeout" json:"keep_alive_timeout"`

	// KeepAliveInterval, in seconds, is the time between keep-alive probes.
	KeepAliveInterval int `mapstructure:"keep_alive_interval" json:"keep_alive_interval"`

	// KeepAliveCount is the max unanswered keep-alive probes before dropping.
	KeepAliveCount int `mapstructure:"keep_alive_count" json:"keep_alive_count"`

	// StartupProbeTimeoutSeconds is the max time to wait for first probe cycle.
	StartupProbeTimeoutSeconds int `mapstructure:"startup_probe_timeout_seconds" json:"startup_probe_timeout_seconds"`

	// Ports holds configuration for each listen port (write, read, compat).
	Ports map[string]PortConfig `mapstructure:"ports" json:"ports"`

	// PprofAddr enables the Go pprof HTTP server.
	PprofAddr string `mapstructure:"pprof_addr" json:"pprof_addr"`

	// AdminTokens holds bearer tokens for admin API authentication (M2+).
	AdminTokens AdminTokenConfig `mapstructure:"admin_tokens" json:"admin_tokens"`

	// Decisions holds SSE streaming configuration for the decisions endpoint (M3).
	Decisions DecisionsConfig `mapstructure:"decisions" json:"decisions"`

	// --- Deprecated fields (M1 compatibility) -----------------------------
	// AdminAddr is the legacy flat admin_addr (M1). It populates Admin.ListenAddr
	// during Normalize() if Admin.ListenAddr is empty. Accepted with a
	// deprecation warning for one release, then removed.
	AdminAddr string `mapstructure:"admin_addr" json:"admin_addr,omitempty"`

	// --- Derived legacy fields (M1 shim) ----------------------------------
	ListenAddr string   `mapstructure:"-" json:"-"`
	PoolMode   PoolMode `mapstructure:"-" json:"-"`
	SSLMode    string   `mapstructure:"-" json:"-"`
	SSLCert    string   `mapstructure:"-" json:"-"`
	SSLKey     string   `mapstructure:"-" json:"-"`
}

// GetWritePort returns the write port configuration.
func (s ServerConfig) GetWritePort() (PortConfig, bool) {
	if s.Ports == nil {
		return PortConfig{}, false
	}
	port, ok := s.Ports["write"]
	return port, ok
}

// GetWritePoolMode returns the pool mode for the write port. Defaults to session.
func (s ServerConfig) GetWritePoolMode() PoolMode {
	if port, ok := s.GetWritePort(); ok {
		if m, ok := PoolModeFromString(port.PoolMode); ok {
			return m
		}
	}
	return PoolModeSession
}

// PoolMode represents the connection pooling mode.
type PoolMode string

const (
	PoolModeSession     PoolMode = "session"
	PoolModeTransaction PoolMode = "transaction"
)

// PoolModeFromString parses a string into a PoolMode.
func PoolModeFromString(s string) (PoolMode, bool) {
	switch s {
	case "":
		return PoolModeSession, true
	case "session":
		return PoolModeSession, true
	case "transaction":
		return PoolModeTransaction, true
	default:
		return "", false
	}
}

// DecisionsConfig holds SSE streaming configuration for the decisions endpoint.
type DecisionsConfig struct {
	MaxSubscribers       int `mapstructure:"max_subscribers" json:"max_subscribers"`
	PerSubscriberBuffer  int `mapstructure:"per_subscriber_buffer" json:"per_subscriber_buffer"`
}

// DefaultDecisionsConfig returns sensible defaults for decisions streaming.
func DefaultDecisionsConfig() DecisionsConfig {
	return DecisionsConfig{
		MaxSubscribers:      64,
		PerSubscriberBuffer: 256,
	}
}
