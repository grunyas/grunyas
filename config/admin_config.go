package config

// AdminConfig holds the admin HTTP API server configuration.
type AdminConfig struct {
	ListenAddr string `mapstructure:"listen_addr" json:"listen_addr"`

	TLSEnabled bool `mapstructure:"tls_enabled" json:"tls_enabled"`

	TLSCertFile string `mapstructure:"tls_cert_file" json:"tls_cert_file"`

	TLSKeyFile string `mapstructure:"tls_key_file" json:"tls_key_file"`

	Metrics AdminMetricsConfig `mapstructure:"metrics" json:"metrics"`
}

// AdminMetricsConfig holds configuration for the Prometheus metrics endpoint
// served on the admin port (or on a dedicated listener).
type AdminMetricsConfig struct {
	ListenAddr string `mapstructure:"listen_addr" json:"listen_addr"`

	AuthRequired bool `mapstructure:"auth_required" json:"auth_required"`

	TLSEnabled bool `mapstructure:"tls_enabled" json:"tls_enabled"`
}

// DefaultAdminConfig returns sensible defaults for the admin server.
func DefaultAdminConfig() AdminConfig {
	return AdminConfig{
		ListenAddr: "127.0.0.1:5712",
		TLSEnabled: false,
		Metrics: AdminMetricsConfig{
			ListenAddr:   "",
			AuthRequired: false,
			TLSEnabled:   false,
		},
	}
}

// AdminTokenConfig holds the admin bearer tokens loaded from TOML.
type AdminTokenConfig struct {
	Tokens map[string]AdminTokenEntry `mapstructure:"tokens" json:"tokens"`
}

type AdminTokenEntry struct {
	Role string `mapstructure:"role" json:"role"`
}


