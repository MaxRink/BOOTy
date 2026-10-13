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

// CAPRF_LEGACY_COMPAT presets the legacy curl semantics: plain HTTP is
// allowed for callbacks and images, certificates are not verified and the
// vars are exported. The native keys select each one, and an explicit
// value wins over the preset.
func TestParseVarsTransportOptions(t *testing.T) {
	type flags struct{ insecure, skipTLS, imageHTTP, export bool }
	for _, tt := range []struct {
		vars string
		want flags
	}{
		{vars: "HOSTNAME=x\n"},
		{vars: "CAPRF_LEGACY_COMPAT=true\n", want: flags{true, true, true, true}},
		{vars: "INSECURE_TRANSPORT=true\nSKIP_TLS_VERIFY=true\nIMAGE_ALLOW_INSECURE_HTTP=true\nEXPORT_VARS=true\n", want: flags{true, true, true, true}},
		{vars: "SKIP_TLS_VERIFY=1\n", want: flags{skipTLS: true}},
		{vars: "EXPORT_VARS=yes\n", want: flags{export: true}},
		{vars: "CAPRF_LEGACY_COMPAT=true\nSKIP_TLS_VERIFY=false\nEXPORT_VARS=false\n", want: flags{insecure: true, imageHTTP: true}},
		{vars: "INSECURE_TRANSPORT=false\nIMAGE_ALLOW_INSECURE_HTTP=false\nCAPRF_LEGACY_COMPAT=true\n", want: flags{skipTLS: true, export: true}},
	} {
		cfg, err := ParseVars(strings.NewReader(tt.vars))
		if err != nil {
			t.Fatalf("ParseVars(%q): %v", tt.vars, err)
		}
		got := flags{
			insecure:  config.BoolOr(cfg.Transport.Insecure, false),
			skipTLS:   config.BoolOr(cfg.Transport.SkipTLSVerify, false),
			imageHTTP: config.BoolOr(cfg.Provision.Image.AllowInsecureHTTP, false),
			export:    config.BoolOr(cfg.Transport.ExportVars, false),
		}
		if got != tt.want {
			t.Errorf("ParseVars(%q) = %+v, want %+v", tt.vars, got, tt.want)
		}
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
		{name: "SKIP_TLS_VERIFY accepts untrusted certificate", skip: true, wantOK: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Transport.Token = "token"
			cfg.Transport.InitURL = srv.URL + "/status/init"
			cfg.Transport.SkipTLSVerify = new(tt.skip)
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

func TestNewExportVarsOption(t *testing.T) {
	for _, tt := range []struct {
		name, vars string
		want       string
	}{
		{name: "native", vars: "export EXPORT_VARS=true\nexport CUSTOM_NATIVE_VAR=\"set\"\n", want: "set"},
		{name: "explicit false wins over preset", vars: "export CAPRF_LEGACY_COMPAT=true\nexport EXPORT_VARS=false\nexport CUSTOM_NATIVE_VAR=\"set\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CUSTOM_NATIVE_VAR", "")
			if _, err := New(writeVarsFile(t, tt.vars)); err != nil {
				t.Fatalf("New() = %v", err)
			}
			if got := os.Getenv("CUSTOM_NATIVE_VAR"); got != tt.want {
				t.Errorf("CUSTOM_NATIVE_VAR = %q, want %q", got, tt.want)
			}
		})
	}
}
