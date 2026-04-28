package config

// NodeConfig represents a single PostgreSQL instance in the cluster.
type NodeConfig struct {
	ID           string               `mapstructure:"id"`
	Host         string               `mapstructure:"host"`
	Port         int                  `mapstructure:"port"`
	DeclaredRole string               `mapstructure:"declared_role"`
	Connection   NodeConnectionConfig `mapstructure:"connection"`
	Pool         NodePoolConfig       `mapstructure:"pool"`
}

// NodeConnectionConfig holds connection credentials for a node.
type NodeConnectionConfig struct {
	User                  string `mapstructure:"user"`
	Password              string `mapstructure:"password"`
	Database              string `mapstructure:"database"`
	ConnectTimeoutSeconds int    `mapstructure:"connect_timeout_seconds"`
	SSLMode               string `mapstructure:"ssl_mode"` // reserved for M5; only "disable" accepted in M1
}

// NodePoolConfig holds pool settings for a node.
type NodePoolConfig struct {
	MinConns          int `mapstructure:"min_conns"`
	MaxConns          int `mapstructure:"max_conns"`
	MaxConnLifetime   int `mapstructure:"max_conn_lifetime"`
	MaxConnIdleTime   int `mapstructure:"max_conn_idle_time"`
	HealthCheckPeriod int `mapstructure:"health_check_period"`
}
