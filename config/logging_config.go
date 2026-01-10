package config

// LoggingConfig configures structured logging.
type LoggingConfig struct {
	// Level is the minimum log level (debug, info, warn, error).
	Level string `mapstructure:"level"`
	// Development toggles zap's development settings (human-friendly output).
	Development bool `mapstructure:"development"`
}
