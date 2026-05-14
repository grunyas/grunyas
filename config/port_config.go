package config

// PortConfig holds configuration for a single listen port (write, read, compat).
type PortConfig struct {
	ListenAddr         string `mapstructure:"listen_addr" json:"listen_addr"`
	PoolMode           string `mapstructure:"pool_mode" json:"pool_mode"`
	SSLMode            string `mapstructure:"ssl_mode" json:"ssl_mode"`
	SSLCert            string `mapstructure:"ssl_cert" json:"ssl_cert"`
	SSLKey             string `mapstructure:"ssl_key" json:"ssl_key"`
	PostWriteWindowMs  int    `mapstructure:"post_write_window_ms" json:"post_write_window_ms"`
	AutoUpgrade        bool   `mapstructure:"auto_upgrade" json:"auto_upgrade"`
}
