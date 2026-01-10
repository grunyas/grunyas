package config

type ServerConfig struct {
	ListenAddr string `mapstructure:"listen_addr"` // Formatted as host:port
	AdminAddr  string `mapstructure:"admin_addr"`  // Formatted as host:port

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

	// SSLConfig holds the configuration for SSL/TLS connections.
	SSLCert string `mapstructure:"ssl_cert"` // Path to the certificate file
	SSLKey  string `mapstructure:"ssl_key"`  // Path to the key file

	// SSLMode determines the enforcement of SSL connections.
	// Supported values: "never", "optional", "mandatory".
	// Default: "never".
	SSLMode string `mapstructure:"ssl_mode"`
}
