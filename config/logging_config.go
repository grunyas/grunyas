package config

// LoggingConfig configures structured logging.
type LoggingConfig struct {
	Level       string `mapstructure:"level" json:"level"`
	Development bool   `mapstructure:"development" json:"development"`
}
