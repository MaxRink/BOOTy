package caprf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func writeVarsFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vars")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// CAPRF_LEGACY_COMPAT implies the legacy curl semantics: plain HTTP is
// allowed for callbacks and images, and certificates are not verified.
func TestParseVarsLegacyCompatTransport(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader("CAPRF_LEGACY_COMPAT=true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Transport.Insecure || !cfg.Transport.SkipTLSVerify || !cfg.Provision.Image.AllowInsecureHTTP {
		t.Fatalf("transport = %+v, image allow HTTP = %v; want all legacy curl semantics",
			cfg.Transport, cfg.Provision.Image.AllowInsecureHTTP)
	}

	cfg, err = ParseVars(strings.NewReader("HOSTNAME=x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport.Insecure || cfg.Transport.SkipTLSVerify || cfg.Provision.Image.AllowInsecureHTTP {
		t.Fatal("default transport must stay strict")
	}
}

func TestReportStatusSkipTLSVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, tt := range []struct {
		name   string
		skip   bool
		wantOK bool
	}{
		{name: "verification on rejects untrusted certificate"},
		{name: "legacy compat accepts untrusted certificate", skip: true, wantOK: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Transport.Token = "token"
			cfg.Transport.InitURL = srv.URL + "/status/init"
			cfg.Transport.SkipTLSVerify = tt.skip
			err := NewFromConfig(cfg).ReportStatus(context.Background(), config.StatusInit, "")
			if (err == nil) != tt.wantOK {
				t.Fatalf("ReportStatus() = %v, want ok %v", err, tt.wantOK)
			}
		})
	}
}

// The legacy scripts `source` the vars file, so every entry, including
// proxy settings from AdditionalVars, is in the environment.
func TestNewLegacyCompatExportsVars(t *testing.T) {
	t.Setenv("https_proxy", "")
	t.Setenv("CUSTOM_LEGACY_VAR", "")
	path := writeVarsFile(t, `export CAPRF_LEGACY_COMPAT="true"
export https_proxy="http://proxy.example:3128"
export CUSTOM_LEGACY_VAR="a b"
export not-an-identifier="x"
`)
	if _, err := New(path); err != nil {
		t.Fatalf("New() = %v", err)
	}
	if got := os.Getenv("https_proxy"); got != "http://proxy.example:3128" {
		t.Errorf("https_proxy = %q", got)
	}
	if got := os.Getenv("CUSTOM_LEGACY_VAR"); got != "a b" {
		t.Errorf("CUSTOM_LEGACY_VAR = %q", got)
	}
}

func TestNewDefaultDoesNotExportVars(t *testing.T) {
	t.Setenv("CUSTOM_DEFAULT_VAR", "")
	path := writeVarsFile(t, "export CUSTOM_DEFAULT_VAR=\"set\"\n")
	if _, err := New(path); err != nil {
		t.Fatalf("New() = %v", err)
	}
	if got := os.Getenv("CUSTOM_DEFAULT_VAR"); got != "" {
		t.Errorf("CUSTOM_DEFAULT_VAR = %q, want unset without legacy compat", got)
	}
}
