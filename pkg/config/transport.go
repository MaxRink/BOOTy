package config

// TransportConfig defines how BOOTy communicates with the CAPRF provisioning
// server. It holds authentication tokens, status reporting URLs, and transport
// security settings. Used by all operating modes that report status.
type TransportConfig struct {
	// Token is the bearer token for authenticating with the CAPRF server.
	// In JWT mode, this is the bootstrap token exchanged for a JWT.
	// Default: ""
	Token string `yaml:"token" json:"token"`

	// TokenURL is the JWT token endpoint for token exchange/renewal.
	// When set, the bootstrap Token is exchanged for a JWT after network is ready.
	// Default: "" (no JWT exchange, use Token directly)
	TokenURL string `yaml:"tokenURL" json:"tokenURL"`

	// TokenAlgorithm is the JWT signing algorithm.
	// Valid values: "RS256", "ES256"
	// Default: "" (server default)
	TokenAlgorithm string `yaml:"tokenAlgorithm" json:"tokenAlgorithm"`

	// LogURL is the endpoint for shipping log lines to the server.
	// Default: ""
	LogURL string `yaml:"logURL" json:"logURL"`

	// InitURL is the endpoint to report provisioning start.
	// Also used as the primary connectivity check target.
	// Default: ""
	InitURL string `yaml:"initURL" json:"initURL"`

	// ErrorURL is the endpoint to report provisioning errors.
	// Default: ""
	ErrorURL string `yaml:"errorURL" json:"errorURL"`

	// SuccessURL is the endpoint to report provisioning success.
	// Default: ""
	SuccessURL string `yaml:"successURL" json:"successURL"`

	// DebugURL is the endpoint for debug information uploads.
	// Default: ""
	DebugURL string `yaml:"debugURL" json:"debugURL"`

	// Insecure allows bearer tokens over plain HTTP.
	// WARNING: Only for testing. Never enable in production.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	Insecure *bool `yaml:"insecure,omitempty" json:"insecure,omitempty"`

	// SkipTLSVerify disables TLS certificate verification for CAPRF
	// callbacks, the connectivity check and image downloads, like the
	// `curl --insecure` calls of the CAPRF legacy provisioner. TokenURL and
	// the crash artifact upload always verify TLS.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	SkipTLSVerify *bool `yaml:"skipTLSVerify,omitempty" json:"skipTLSVerify,omitempty"`

	// ExportVars exports every vars file entry whose key is a valid shell
	// identifier to the process environment, like `source vars` in the
	// CAPRF legacy scripts. Proxy entries such as https_proxy then apply to
	// the CAPRF callbacks, the connectivity check and image downloads. Only
	// the vars file input uses it.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	ExportVars *bool `yaml:"exportVars,omitempty" json:"exportVars,omitempty"`
}
