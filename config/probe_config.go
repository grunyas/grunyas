package config

// ProbeConfig holds configuration for node probing (health, role, system_identifier).
// All durations are in milliseconds.
type ProbeConfig struct {
	IntervalMs           int `mapstructure:"interval_ms"`
	LivenessFailureCount int `mapstructure:"liveness_failure_count"`
	LivenessMaxAgeMs     int `mapstructure:"liveness_max_age_ms"`
	RoleMaxAgeMs         int `mapstructure:"role_max_age_ms"`
}

// DefaultProbeConfig returns sensible defaults.
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{
		IntervalMs:           1000, // 1 second
		LivenessFailureCount: 3,    // 3 consecutive failures before marking down
		LivenessMaxAgeMs:     5000, // 5 seconds
		RoleMaxAgeMs:         5000, // 5 seconds
	}
}
