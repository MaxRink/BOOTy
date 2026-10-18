package logging

import (
	"strings"
	"testing"
)

func TestRedactText(t *testing.T) {
	for _, tc := range []struct {
		in     string
		secret string
	}{
		{"Authorization: Bearer hdr-token", "hdr-token"},
		{"bearer abc.def", "abc.def"},
		{"https://robot:urlpass@host/x", "urlpass"},
		{"got eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl ok", "eyJzdWIiOiJ4In0"},
		{"-----BEGIN PRIVATE KEY-----\nMIIEsecret\n-----END PRIVATE KEY----- tail", "MIIEsecret"},
		{"-----BEGIN EC PRIVATE KEY-----\nMIIEcut", "MIIEcut"},
		{"BGP_AUTH_PASSWORD=s3cr3t", "s3cr3t"},
		{"luks_key: lukssecret", "lukssecret"},
		{"LUKS_PASSPHRASE=lukspass", "lukspass"},
		{"MK dump:\tde ad be ef\n\t\tca fe ba be", "ca fe ba be"},
		{"Volume key: 0a 1b 2c", "0a 1b 2c"},
		{"token=query-secret&x=1", "query-secret"},
		{"https://s3.example/img.raw?X-Amz-Date=1&X-Amz-Signature=abc123def&y=2", "abc123def"},
		{"https://blob.example/img?sv=2024&sig=azsig%2Bvalue", "azsig"},
		{"https://cdn.example/img?Expires=1&Signature=cfsig~value", "cfsig"},
	} {
		got := RedactText(tc.in)
		if strings.Contains(got, tc.secret) {
			t.Errorf("RedactText(%q) = %q leaks %q", tc.in, got, tc.secret)
		}
		if !strings.Contains(got, "[REDACTED") {
			t.Errorf("RedactText(%q) = %q has no redaction marker", tc.in, got)
		}
	}
}

func TestRedactTextKeepsPlainText(t *testing.T) {
	for _, in := range []string{
		"monkey=banana interface=eth0",
		"image sha256 3f2a9c0d1e4b5a6978e1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d5e4f3a2b",
		"[INFO] disk selected device=/dev/nvme0n1 size_gb=960",
		"eyJ short.not.jwt",
		"dry-run mode: skipping active network setup and token renewal",
		"failed to start token renewal",
		"token can not be renewed after the task completed",
		"token can not be renewed, stopping the task",
		"failed to report the token failure",
		"failed to clear temporary security password after erase",
		"secret store opened",
		"credential helper not configured",
		"authorization policy loaded",
		"passphrase file missing",
		"module verification failed: signature and/or required key missing",
		"design=final signals=3",
	} {
		if got := RedactText(in); got != in {
			t.Errorf("RedactText(%q) = %q, want unchanged", in, got)
		}
	}
}
