package config

import "time"

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

	// TokenExpiresAt is the expiry of Token as an RFC 3339 time, for a token
	// that BOOTy can not renew (no TokenURL). It needs a correct ramdisk
	// clock (RTC); BOOTy does not sync the time. Prefer
	// TokenExpiresInSeconds. BOOTy stops the run
	// TokenRenewMarginSeconds before it, reports the reason and ends like a
	// failed run (FailureAction).
	// Default: "" (expiry unknown, no shutdown)
	TokenExpiresAt string `yaml:"tokenExpiresAt" json:"tokenExpiresAt"`

	// TokenExpiresInSeconds is the lifetime of Token in seconds, counted from
	// the start of BOOTy, for a token that BOOTy can not renew. It does not
	// depend on the ramdisk clock. BOOTy starts after the virtual media
	// insert and the boot, so the count runs late by that time: CAPRF must
	// subtract a boot allowance. It wins over TokenExpiresAt. With TokenURL
	// both are ignored: the expires_in of the exchange is authoritative.
	// Default: 0 (unset)
	TokenExpiresInSeconds int `yaml:"tokenExpiresInSeconds" json:"tokenExpiresInSeconds"`

	// TokenRenewMarginSeconds is how long before the token expiry BOOTy
	// renews the token (TokenURL) or, without TokenURL, stops at
	// TokenExpiresAt. 0 means 20% of the token lifetime.
	// Default: 0
	TokenRenewMarginSeconds int `yaml:"tokenRenewMarginSeconds" json:"tokenRenewMarginSeconds"`

	// TokenRenewRetries is the number of renewal attempts before the
	// renewal fails for good and BOOTy stops the run. An expired token
	// stops the attempts early. 0 means 5.
	// Default: 0 (5)
	TokenRenewRetries int `yaml:"tokenRenewRetries" json:"tokenRenewRetries"`

	// TokenRenewBackoffSeconds is the wait after the first failed renewal
	// attempt. It doubles after each further failure, up to 60 seconds, and
	// never goes past the expiry. 0 means 1 second.
	// Default: 0 (1)
	TokenRenewBackoffSeconds int `yaml:"tokenRenewBackoffSeconds" json:"tokenRenewBackoffSeconds"`

	// LogURL is the endpoint for shipping log lines to the server.
	// Default: ""
	LogURL string `yaml:"logURL" json:"logURL"`

	// LogFlushTimeoutSec bounds how long BOOTy waits before kexec, reboot,
	// power off or the rescue shell for the buffered LogURL lines to reach
	// the server. 0 sends nothing more. nil means
	// DefaultLogFlushTimeoutSec.
	// Default: nil (5)
	LogFlushTimeoutSec *int `yaml:"logFlushTimeoutSec,omitempty" json:"logFlushTimeoutSec,omitempty"`

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

// DefaultLogFlushTimeoutSec is the LogFlushTimeoutSec default.
const DefaultLogFlushTimeoutSec = 5

// MaxLogFlushTimeoutSec caps LogFlushTimeoutSec, so a slow log server can
// not hold the handoff for long.
const MaxLogFlushTimeoutSec = 60

// LogFlushTimeout returns LogFlushTimeoutSec as a duration, with the default
// for an unset value.
func (t *TransportConfig) LogFlushTimeout() time.Duration {
	if t.LogFlushTimeoutSec == nil {
		return DefaultLogFlushTimeoutSec * time.Second
	}
	return time.Duration(*t.LogFlushTimeoutSec) * time.Second
}
