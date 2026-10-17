package config

import (
	"strings"
	"testing"
)

func TestTimeConfigDefaults(t *testing.T) {
	var tc TimeConfig
	if tc.SyncMode() != TimeSyncAuto || tc.MaxSkew() != DefaultTimeMaxSkewSeconds {
		t.Fatalf("defaults = %q, %d", tc.SyncMode(), tc.MaxSkew())
	}
	tc = TimeConfig{Sync: TimeSyncOff, MaxSkewSeconds: 30}
	if tc.SyncMode() != TimeSyncOff || tc.MaxSkew() != 30 {
		t.Fatalf("explicit = %q, %d", tc.SyncMode(), tc.MaxSkew())
	}
}

func TestValidateTime(t *testing.T) {
	tests := []struct {
		name string
		tc   TimeConfig
		want string
	}{
		{"defaults", TimeConfig{}, ""},
		{"require with servers", TimeConfig{Sync: TimeSyncRequire, NTPServers: []string{"ntp.example.com", "192.0.2.1:123", "2001:db8::1", "[2001:db8::1]:123"}, MaxSkewSeconds: 60}, ""},
		{"unknown mode", TimeConfig{Sync: "always"}, "time.sync (TIME_SYNC) must be off, auto or require"},
		{"negative skew", TimeConfig{MaxSkewSeconds: -1}, "TIME_MAX_SKEW_SEC"},
		{"skew above a day", TimeConfig{MaxSkewSeconds: MaxTimeMaxSkewSeconds + 1}, "TIME_MAX_SKEW_SEC"},
		{"url", TimeConfig{NTPServers: []string{"ntp://ntp.example.com"}}, "not a URL"},
		{"bad port", TimeConfig{NTPServers: []string{"ntp.example.com:0"}}, "invalid port"},
		{"port not a number", TimeConfig{NTPServers: []string{"ntp.example.com:ntp"}}, "invalid port"},
		{"bad host", TimeConfig{NTPServers: []string{"ntp_1.example.com"}}, "not a valid host name"},
		{"bracketed ipv6 without port", TimeConfig{NTPServers: []string{"[2001:db8::1]"}}, "not a valid host name"},
		{"empty entry", TimeConfig{NTPServers: []string{""}}, "must not be empty"},
		{"too many", TimeConfig{NTPServers: strings.Fields("a b c d e f g h i")}, "at most 8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := strings.Join(validateTime(&tt.tc), "; ")
			if tt.want == "" && errs != "" {
				t.Fatalf("validateTime = %q, want no error", errs)
			}
			if !strings.Contains(errs, tt.want) {
				t.Fatalf("validateTime = %q, want %q", errs, tt.want)
			}
		})
	}
}

func TestValidateRejectsBadTimeConfig(t *testing.T) {
	cfg := &Config{Time: TimeConfig{Sync: "sometimes"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "TIME_SYNC") {
		t.Fatalf("Validate = %v, want a TIME_SYNC error", err)
	}
}
