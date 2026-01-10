package config

// AuthConfig holds static credentials and auth method.
// Password may be plain text, an md5 hash (prefixed with "md5"),
// or a SCRAM-SHA-256 stored secret (prefixed with "SCRAM-SHA-256$").
type AuthConfig struct {
	Method   string `mapstructure:"method"`   // "plain", "md5", or "scram-sha-256"
	Username string `mapstructure:"username"` // required
	Password string `mapstructure:"password"` // required
}
