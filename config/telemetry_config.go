package config

// TelemetryConfig holds minimal OpenTelemetry exporter stubs.
type TelemetryConfig struct {
	OTLPEndpoint string `mapstructure:"otlp_endpoint" json:"otlp_endpoint"`
	Insecure     bool   `mapstructure:"insecure" json:"insecure"`
	ServiceName  string `mapstructure:"service_name" json:"service_name"`
}
