package config

import (
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
)

type Config struct {
	ServerConfig ServerConfig    `mapstructure:"server"`
	Nodes        []NodeConfig    `mapstructure:"nodes"`
	ProbeConfig  ProbeConfig     `mapstructure:"probe"`
	Logging      LoggingConfig   `mapstructure:"logging"`
	Telemetry    TelemetryConfig `mapstructure:"telemetry"`
	Auth         AuthConfig      `mapstructure:"auth"`

	// BackendConfig is a derived legacy view of the primary node (M1 shim).
	// Populated by Normalize(); not read from TOML/env. Removed in M1 PR 2
	// when pool manager is refactored to per-node + Topology lookups.
	BackendConfig DatabasePoolConfig `mapstructure:"-"`
}

// PrimaryNode returns a pointer to the node with declared_role="primary",
// or nil if none is configured. M1 guarantees exactly one primary at validation.
func (c *Config) PrimaryNode() *NodeConfig {
	for i := range c.Nodes {
		if strings.EqualFold(c.Nodes[i].DeclaredRole, "primary") {
			return &c.Nodes[i]
		}
	}
	return nil
}

// Normalize populates the derived legacy shim fields (ServerConfig.ListenAddr,
// SSLMode/Cert/Key, PoolMode and BackendConfig) from the new schema. Call this
// after Unmarshal and before passing the config to the proxy. Idempotent.
//
// This is an M1-only bridge. PR 2 removes the legacy fields and updates call
// sites to read from the new schema directly.
func (c *Config) Normalize() {
	if write, ok := c.ServerConfig.GetWritePort(); ok {
		c.ServerConfig.ListenAddr = write.ListenAddr
		c.ServerConfig.SSLMode = write.SSLMode
		c.ServerConfig.SSLCert = write.SSLCert
		c.ServerConfig.SSLKey = write.SSLKey
		c.ServerConfig.PoolMode = c.ServerConfig.GetWritePoolMode()
	}

	if p := c.PrimaryNode(); p != nil {
		c.BackendConfig = DatabasePoolConfig{
			DatabaseHost:                  p.Host,
			DatabasePort:                  p.Port,
			DatabaseUser:                  p.Connection.User,
			DatabasePassword:              p.Connection.Password,
			DatabaseName:                  p.Connection.Database,
			DatabaseConnectTimeoutSeconds: p.Connection.ConnectTimeoutSeconds,
			PoolMinConns:                  p.Pool.MinConns,
			PoolMaxConns:                  p.Pool.MaxConns,
			PoolMaxConnLifetime:           p.Pool.MaxConnLifetime,
			PoolMaxConnIdleTime:           p.Pool.MaxConnIdleTime,
			PoolHealthCheckPeriod:         p.Pool.HealthCheckPeriod,
		}
	}
}

// Default returns sensible defaults for local use. The returned config is
// already Normalize()d, so legacy shim fields (BackendConfig, ServerConfig
// ListenAddr/PoolMode/SSL*) are populated.
func Default() Config {
	c := defaultConfig()
	c.Normalize()
	return c
}

func defaultConfig() Config {
	return Config{
		ServerConfig: ServerConfig{
			AdminAddr:                  "127.0.0.1:5712",
			MaxSessions:                1000,
			ClientIdleTimeout:          300, // seconds
			KeepAliveTimeout:           15,
			KeepAliveInterval:          15,
			KeepAliveCount:             9,
			StartupProbeTimeoutSeconds: 10,
			Ports: map[string]PortConfig{
				"write": {
					ListenAddr: "127.0.0.1:5711",
					PoolMode:   "session",
					SSLMode:    "never",
					SSLCert:    "",
					SSLKey:     "",
				},
			},
		},
		Nodes: []NodeConfig{
			{
				ID:           "primary-1",
				Host:         "localhost",
				Port:         5432,
				DeclaredRole: "primary",
				Connection: NodeConnectionConfig{
					User:                  "postgres",
					Password:              "postgres",
					Database:              "postgres",
					ConnectTimeoutSeconds: 5,
					SSLMode:               "disable",
				},
				Pool: NodePoolConfig{
					MinConns:          2,
					MaxConns:          10,
					MaxConnLifetime:   3600, // 1 hour
					MaxConnIdleTime:   1800, // 30 minutes
					HealthCheckPeriod: 60,   // 1 minute
				},
			},
		},
		ProbeConfig: DefaultProbeConfig(),
		Logging: LoggingConfig{
			Level:       "info",
			Development: true,
		},
		Telemetry: TelemetryConfig{
			ServiceName: "grunyas",
		},
		Auth: AuthConfig{
			Method:   "md5",
			Username: "postgres",
			Password: "postgres",
		},
	}
}

// Validate checks that all config fields are well-formed.
func (c *Config) Validate() error {
	var errs []string

	if _, err := zap.ParseAtomicLevel(c.Logging.Level); err != nil {
		errs = append(errs, fmt.Sprintf("logging.level: %v", err))
	}

	if c.Telemetry.OTLPEndpoint != "" {
		if _, _, err := net.SplitHostPort(c.Telemetry.OTLPEndpoint); err != nil {
			errs = append(errs, fmt.Sprintf("telemetry.otlp_endpoint must be host:port: %v", err))
		}
	}

	switch strings.ToLower(c.Auth.Method) {
	case "plain", "md5", "scram-sha-256":
	default:
		errs = append(errs, "auth.method must be one of: plain, md5, scram-sha-256")
	}

	if c.Auth.Username == "" {
		errs = append(errs, "auth.username is required")
	}

	if c.Auth.Password == "" {
		errs = append(errs, "auth.password is required")
	}

	// Server config validation
	if err := validateHostPort("server.admin_addr", c.ServerConfig.AdminAddr); err != nil {
		errs = append(errs, err.Error())
	}

	if c.ServerConfig.MaxSessions < 0 {
		errs = append(errs, "server.max_sessions must be >= 0")
	}

	if c.ServerConfig.ClientIdleTimeout < 0 {
		errs = append(errs, "server.client_idle_timeout must be >= 0")
	}

	if c.ServerConfig.KeepAliveTimeout < 0 {
		errs = append(errs, "server.keep_alive_timeout must be >= 0")
	}

	if c.ServerConfig.KeepAliveInterval < 0 {
		errs = append(errs, "server.keep_alive_interval must be >= 0")
	}

	if c.ServerConfig.KeepAliveCount < 0 {
		errs = append(errs, "server.keep_alive_count must be >= 0")
	}

	if c.ServerConfig.StartupProbeTimeoutSeconds < 0 {
		errs = append(errs, "server.startup_probe_timeout_seconds must be >= 0")
	}

	// Port validation
	if len(c.ServerConfig.Ports) == 0 {
		errs = append(errs, "server.ports is required")
	}

	if _, ok := c.ServerConfig.Ports["write"]; !ok {
		errs = append(errs, "server.ports.write is required")
	}

	for portID, port := range c.ServerConfig.Ports {
		if port.ListenAddr == "" {
			errs = append(errs, fmt.Sprintf("server.ports.%s.listen_addr is required", portID))
		} else if err := validateHostPort(fmt.Sprintf("server.ports.%s.listen_addr", portID), port.ListenAddr); err != nil {
			errs = append(errs, err.Error())
		}

		if _, ok := PoolModeFromString(port.PoolMode); !ok {
			errs = append(errs, fmt.Sprintf("server.ports.%s.pool_mode must be one of: session, transaction (got %q)", portID, port.PoolMode))
		}

		switch strings.ToLower(port.SSLMode) {
		case "", "never":
			// No validation needed for certs if SSL is disabled
		case "optional", "mandatory":
			if port.SSLCert == "" {
				errs = append(errs, fmt.Sprintf("server.ports.%s.ssl_cert is required when ssl_mode is optional or mandatory", portID))
			}
			if port.SSLKey == "" {
				errs = append(errs, fmt.Sprintf("server.ports.%s.ssl_key is required when ssl_mode is optional or mandatory", portID))
			}
		default:
			errs = append(errs, fmt.Sprintf("server.ports.%s.ssl_mode must be one of: never, optional, mandatory", portID))
		}
	}

	// Nodes validation
	if len(c.Nodes) == 0 {
		errs = append(errs, "nodes must contain at least one entry")
	}

	seenIDs := make(map[string]bool)
	seenHostPorts := make(map[string]bool)
	primaryCount := 0

	for i, node := range c.Nodes {
		prefix := fmt.Sprintf("nodes[%d]", i)

		// ID validation
		if node.ID == "" {
			errs = append(errs, fmt.Sprintf("%s.id is required", prefix))
		} else if seenIDs[node.ID] {
			errs = append(errs, fmt.Sprintf("%s.id '%s' is duplicate", prefix, node.ID))
		} else {
			seenIDs[node.ID] = true
			// Validate ID format: URL-safe lowercase characters
			for _, r := range node.ID {
				if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
					errs = append(errs, fmt.Sprintf("%s.id must be lowercase URL-safe (a-z0-9-)", prefix))
					break
				}
			}
		}

		// Host validation
		if node.Host == "" {
			errs = append(errs, fmt.Sprintf("%s.host is required", prefix))
		}

		// Port validation
		if node.Port <= 0 || node.Port > 65535 {
			errs = append(errs, fmt.Sprintf("%s.port must be in the range 1-65535", prefix))
		}

		// Host:port uniqueness
		hostPort := fmt.Sprintf("%s:%d", node.Host, node.Port)
		if seenHostPorts[hostPort] {
			errs = append(errs, fmt.Sprintf("%s host:port tuple '%s' is duplicate", prefix, hostPort))
		}
		seenHostPorts[hostPort] = true

		// Declared role validation
		switch strings.ToLower(node.DeclaredRole) {
		case "primary":
			primaryCount++
		case "replica":
			// ok
		default:
			errs = append(errs, fmt.Sprintf("%s.declared_role must be 'primary' or 'replica', got '%s'", prefix, node.DeclaredRole))
		}

		// Connection config validation
		if node.Connection.User == "" {
			errs = append(errs, fmt.Sprintf("%s.connection.user is required", prefix))
		}
		if node.Connection.Database == "" {
			errs = append(errs, fmt.Sprintf("%s.connection.database is required", prefix))
		}
		if node.Connection.ConnectTimeoutSeconds < 0 {
			errs = append(errs, fmt.Sprintf("%s.connection.connect_timeout_seconds must be >= 0", prefix))
		}
		// In M1, only "disable" is accepted for upstream SSL
		if node.Connection.SSLMode != "" && node.Connection.SSLMode != "disable" {
			errs = append(errs, fmt.Sprintf("%s.connection.ssl_mode must be 'disable' in M1", prefix))
		}

		// Pool config validation
		if node.Pool.MinConns < 0 {
			errs = append(errs, fmt.Sprintf("%s.pool.min_conns must be >= 0", prefix))
		}
		if node.Pool.MaxConns <= 0 {
			errs = append(errs, fmt.Sprintf("%s.pool.max_conns must be > 0", prefix))
		}
		if node.Pool.MinConns > node.Pool.MaxConns {
			errs = append(errs, fmt.Sprintf("%s.pool.min_conns must be <= pool.max_conns", prefix))
		}
		if node.Pool.MaxConnLifetime < 0 {
			errs = append(errs, fmt.Sprintf("%s.pool.max_conn_lifetime must be >= 0", prefix))
		}
		if node.Pool.MaxConnIdleTime < 0 {
			errs = append(errs, fmt.Sprintf("%s.pool.max_conn_idle_time must be >= 0", prefix))
		}
		if node.Pool.HealthCheckPeriod < 0 {
			errs = append(errs, fmt.Sprintf("%s.pool.health_check_period must be >= 0", prefix))
		}
	}

	// Exactly one primary required
	if primaryCount != 1 {
		errs = append(errs, fmt.Sprintf("exactly one node with declared_role='primary' required, got %d", primaryCount))
	}

	// Probe config validation
	if c.ProbeConfig.IntervalMs < 100 {
		errs = append(errs, "probe.interval_ms must be >= 100")
	}
	if c.ProbeConfig.LivenessFailureCount < 0 {
		errs = append(errs, "probe.liveness_failure_count must be >= 0")
	}
	if c.ProbeConfig.LivenessMaxAgeMs < 0 {
		errs = append(errs, "probe.liveness_max_age_ms must be >= 0")
	}
	if c.ProbeConfig.RoleMaxAgeMs < 0 {
		errs = append(errs, "probe.role_max_age_ms must be >= 0")
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

func validateHostPort(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}

	if _, _, err := net.SplitHostPort(value); err != nil {
		return fmt.Errorf("%s must be formatted as host:port: %v", field, err)
	}

	return nil
}
