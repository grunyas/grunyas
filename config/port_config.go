package config

// PortConfig holds configuration for a single listen port (write, read, compat).
type PortConfig struct {
	ListenAddr string `mapstructure:"listen_addr"`
	PoolMode   string `mapstructure:"pool_mode"`
	SSLMode    string `mapstructure:"ssl_mode"`
	SSLCert    string `mapstructure:"ssl_cert"`
	SSLKey     string `mapstructure:"ssl_key"`
}
