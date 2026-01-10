package config

// DatabasePoolConfig holds PostgreSQL connection settings
type DatabasePoolConfig struct {
	DatabaseHost                  string `mapstructure:"host"`
	DatabasePort                  int    `mapstructure:"port"`
	DatabaseUser                  string `mapstructure:"user"`
	DatabasePassword              string `mapstructure:"password"`
	DatabaseName                  string `mapstructure:"database"`
	DatabaseConnectTimeoutSeconds int    `mapstructure:"connect_timeout_seconds"`

	// Connection pool settings
	PoolMinConns          int `mapstructure:"pool_min_conns"`           // Minimum connections to maintain
	PoolMaxConns          int `mapstructure:"pool_max_conns"`           // Maximum connections allowed
	PoolMaxConnLifetime   int `mapstructure:"pool_max_conn_lifetime"`   // Max lifetime in seconds
	PoolMaxConnIdleTime   int `mapstructure:"pool_max_conn_idle_time"`  // Max idle time in seconds
	PoolHealthCheckPeriod int `mapstructure:"pool_health_check_period"` // Health check interval in seconds
}
