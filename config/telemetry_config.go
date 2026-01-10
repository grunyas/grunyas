package config

// TelemetryConfig holds minimal OpenTelemetry exporter stubs.
type TelemetryConfig struct {
	// OTLPEndpoint is the target OTLP endpoint (host:port). Empty disables OTLP.
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
	// Insecure toggles TLS off for OTLP gRPC.
	Insecure bool `mapstructure:"insecure"`
	// ServiceName sets the service.name resource attribute.
	ServiceName string `mapstructure:"service_name"`
}
