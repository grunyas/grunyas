package config

import (
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
)

type Config struct {
	ServerConfig  ServerConfig       `mapstructure:"server"`
	BackendConfig DatabasePoolConfig `mapstructure:"backend"`
	Logging       LoggingConfig      `mapstructure:"logging"`
	Telemetry     TelemetryConfig    `mapstructure:"telemetry"`
	Auth          AuthConfig         `mapstructure:"auth"`
}

// Default returns sensible defaults for local use.
func Default() Config {
	return Config{
		ServerConfig: ServerConfig{
			ListenAddr:        "127.0.0.1:5711",
			AdminAddr:         "127.0.0.1:5712",
			MaxSessions:       1000,
			ClientIdleTimeout: 300, // seconds
			KeepAliveTimeout:  15,
			KeepAliveInterval: 15,
			KeepAliveCount:    9,
			PoolMode:          "session",
		},
		BackendConfig: DatabasePoolConfig{
			DatabaseConnectTimeoutSeconds: 5,
			PoolMinConns:                  2,
			PoolMaxConns:                  10,
			PoolMaxConnLifetime:           3600, // 1 hour
			PoolMaxConnIdleTime:           1800, // 30 minutes
			PoolHealthCheckPeriod:         60,   // 1 minute
		},
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

	if err := validateHostPort("server.listen_addr", c.ServerConfig.ListenAddr); err != nil {
		errs = append(errs, err.Error())
	}

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

	switch strings.ToLower(c.ServerConfig.SSLMode) {
	case "", "never":
		// No validation needed for certs if SSL is disabled
	case "optional", "mandatory":
		if c.ServerConfig.SSLCert == "" {
			errs = append(errs, "server.ssl_cert is required when ssl_mode is optional or mandatory")
		}
		if c.ServerConfig.SSLKey == "" {
			errs = append(errs, "server.ssl_key is required when ssl_mode is optional or mandatory")
		}
	default:
		errs = append(errs, "server.ssl_mode must be one of: never, optional, mandatory")
	}

	switch strings.ToLower(c.ServerConfig.PoolMode) {
	case "", "session":
		// default handled elsewhere
	case "transaction":
	default:
		errs = append(errs, "server.pool_mode must be one of: session, transaction")
	}

	if c.BackendConfig.DatabaseHost == "" {
		errs = append(errs, "backend.host is required")
	}

	if c.BackendConfig.DatabasePort <= 0 || c.BackendConfig.DatabasePort > 65535 {
		errs = append(errs, "backend.port must be in the range 1-65535")
	}

	if c.BackendConfig.DatabaseName == "" {
		errs = append(errs, "backend.database is required")
	}

	if c.BackendConfig.DatabaseConnectTimeoutSeconds < 0 {
		errs = append(errs, "backend.connect_timeout_seconds must be >= 0")
	}

	if c.BackendConfig.PoolMinConns < 0 {
		errs = append(errs, "backend.pool_min_conns must be >= 0")
	}

	if c.BackendConfig.PoolMaxConns <= 0 {
		errs = append(errs, "backend.pool_max_conns must be > 0")
	}

	if c.BackendConfig.PoolMinConns > c.BackendConfig.PoolMaxConns {
		errs = append(errs, "backend.pool_min_conns must be <= pool_max_conns")
	}

	if c.BackendConfig.PoolMaxConnLifetime < 0 {
		errs = append(errs, "backend.pool_max_conn_lifetime must be >= 0")
	}

	if c.BackendConfig.PoolMaxConnIdleTime < 0 {
		errs = append(errs, "backend.pool_max_conn_idle_time must be >= 0")
	}

	if c.BackendConfig.PoolHealthCheckPeriod < 0 {
		errs = append(errs, "backend.pool_health_check_period must be >= 0")
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
