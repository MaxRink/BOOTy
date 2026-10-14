package caprf

import (
	"fmt"
	"time"

	"github.com/telekom/BOOTy/pkg/auth"
	"github.com/telekom/BOOTy/pkg/config"
)

// applyTokenVar sets the token expiry and renewal options.
func applyTokenVar(cfg *config.MachineConfig, key, value string) (bool, error) {
	var field *int
	switch key {
	case "TOKEN_EXPIRES_AT":
		cfg.Transport.TokenExpiresAt = value
		return true, nil
	case "TOKEN_EXPIRES_IN":
		field = &cfg.Transport.TokenExpiresInSeconds
	case "TOKEN_RENEW_MARGIN_SECONDS":
		field = &cfg.Transport.TokenRenewMarginSeconds
	case "TOKEN_RENEW_RETRIES":
		field = &cfg.Transport.TokenRenewRetries
	case "TOKEN_RENEW_BACKOFF_SECONDS":
		field = &cfg.Transport.TokenRenewBackoffSeconds
	default:
		return false, nil
	}
	if err := setIntField(field, value); err != nil {
		return true, fmt.Errorf("invalid %s: %w", key, err)
	}
	return true, nil
}

// renewalPolicy returns the token renewal settings of the transport config.
func renewalPolicy(t *config.TransportConfig) auth.RenewalPolicy {
	return auth.RenewalPolicy{
		Margin:  time.Duration(t.TokenRenewMarginSeconds) * time.Second,
		Retries: t.TokenRenewRetries,
		Backoff: time.Duration(t.TokenRenewBackoffSeconds) * time.Second,
	}
}
