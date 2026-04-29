package config

// NodeConfig represents a single PostgreSQL instance in the cluster.
type NodeConfig struct {
	ID           string               `mapstructure:"id" json:"id"`
	Host         string               `mapstructure:"host" json:"host"`
	Port         int                  `mapstructure:"port" json:"port"`
	DeclaredRole string               `mapstructure:"declared_role" json:"declared_role"`
	Connection   NodeConnectionConfig `mapstructure:"connection" json:"connection"`
	Pool         NodePoolConfig       `mapstructure:"pool" json:"pool"`
}

type NodeConnectionConfig struct {
	User                  string `mapstructure:"user" json:"user"`
	Password              string `mapstructure:"password" json:"password"`
	Database              string `mapstructure:"database" json:"database"`
	ConnectTimeoutSeconds int    `mapstructure:"connect_timeout_seconds" json:"connect_timeout_seconds"`
	SSLMode               string `mapstructure:"ssl_mode" json:"ssl_mode"`
}

type NodePoolConfig struct {
	MinConns          int `mapstructure:"min_conns" json:"min_conns"`
	MaxConns          int `mapstructure:"max_conns" json:"max_conns"`
	MaxConnLifetime   int `mapstructure:"max_conn_lifetime" json:"max_conn_lifetime"`
	MaxConnIdleTime   int `mapstructure:"max_conn_idle_time" json:"max_conn_idle_time"`
	HealthCheckPeriod int `mapstructure:"health_check_period" json:"health_check_period"`
}
