package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogFlushTimeout(t *testing.T) {
	for _, tc := range []struct {
		value *int
		want  time.Duration
		ok    bool
	}{
		{nil, 5 * time.Second, true},
		{new(0), 0, true},
		{new(60), time.Minute, true},
		{new(-1), 0, false},
		{new(61), 0, false},
	} {
		cfg := Config{Transport: TransportConfig{LogFlushTimeoutSec: tc.value}}
		if err := cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("logFlushTimeoutSec=%v: Validate() = %v, want ok %v", tc.value, err, tc.ok)
			continue
		}
		if tc.ok && cfg.Transport.LogFlushTimeout() != tc.want {
			t.Errorf("LogFlushTimeout() = %v, want %v", cfg.Transport.LogFlushTimeout(), tc.want)
		}
	}
}

func TestLogFlushTimeoutYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "booty.yaml")
	if err := os.WriteFile(path, []byte("transport:\n  logFlushTimeoutSec: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if v := cfg.Transport.LogFlushTimeoutSec; v == nil || *v != 0 {
		t.Fatalf("LogFlushTimeoutSec = %v, want an explicit 0", v)
	}
	if err := os.WriteFile(path, []byte("transport:\n  logFlushTimeoutSec: 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOptions(LoadOptions{Path: path, Strict: true}); err == nil {
		t.Fatal("Load accepted logFlushTimeoutSec: 90")
	}
}
