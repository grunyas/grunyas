package config

type PolicyConfig struct {
	Name       string                 `mapstructure:"name" json:"name"`
	Template   string                 `mapstructure:"template" json:"template"`
	Scope      string                 `mapstructure:"scope" json:"scope"`
	Parameters map[string]interface{} `mapstructure:"parameters" json:"parameters"`
	Timing     PolicyTimingConfig     `mapstructure:"timing" json:"timing"`
}

type PolicyTimingConfig struct {
	DwellMs   int `mapstructure:"dwell_ms" json:"dwell_ms"`
	ReleaseMs int `mapstructure:"release_ms" json:"release_ms"`
}
