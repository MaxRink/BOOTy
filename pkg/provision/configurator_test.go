//go:build linux

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestEfiLoaderNames(t *testing.T) {
	tests := []struct {
		name     string
		arch     string
		wantShim string
		wantGrub string
	}{
		{"amd64", "amd64", "shimx64.efi", "grubx64.efi"},
		{"arm64", "arm64", "shimaa64.efi", "grubaa64.efi"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shim, grub, err := efiLoaderNames(tc.arch)
			if err != nil {
				t.Fatalf("efiLoaderNames(%q): %v", tc.arch, err)
			}
			if shim != tc.wantShim {
				t.Errorf("shim = %q, want %q", shim, tc.wantShim)
			}
			if grub != tc.wantGrub {
				t.Errorf("grub = %q, want %q", grub, tc.wantGrub)
			}
		})
	}
}

func TestEfiLoaderNamesUnsupported(t *testing.T) {
	_, _, err := efiLoaderNames("s390x")
	if err == nil {
		t.Fatal("expected error for unsupported architecture")
	}
	if !strings.Contains(err.Error(), "unsupported architecture") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEfiLoaderPath(t *testing.T) {
	root := t.TempDir()
	efiDir := filepath.Join(root, "boot", "efi", "EFI", "ubuntu")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No shim but grub present -> falls back to grub.
	if err := os.WriteFile(filepath.Join(efiDir, "grubx64.efi"), []byte("grub"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err := efiLoaderPath(root, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	wantGrub := "\\EFI\\ubuntu\\grubx64.efi"
	if loader != wantGrub {
		t.Errorf("got %q, want grub fallback %q", loader, wantGrub)
	}

	// Create shim -> should prefer shim.
	if err := os.WriteFile(filepath.Join(efiDir, "shimx64.efi"), []byte("shim"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err = efiLoaderPath(root, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	wantShim := "\\EFI\\ubuntu\\shimx64.efi"
	if loader != wantShim {
		t.Errorf("got %q, want shim %q", loader, wantShim)
	}

	// ARM64 without shim but with grub -> grub fallback.
	if err := os.WriteFile(filepath.Join(efiDir, "grubaa64.efi"), []byte("grub-arm64"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err = efiLoaderPath(root, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	wantArm := "\\EFI\\ubuntu\\grubaa64.efi"
	if loader != wantArm {
		t.Errorf("got %q, want arm64 grub fallback %q", loader, wantArm)
	}

	// ARM64 with shim -> should prefer shimaa64.
	if err := os.WriteFile(filepath.Join(efiDir, "shimaa64.efi"), []byte("shim-arm64"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err = efiLoaderPath(root, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	wantArmShim := "\\EFI\\ubuntu\\shimaa64.efi"
	if loader != wantArmShim {
		t.Errorf("got %q, want arm64 shim %q", loader, wantArmShim)
	}
}

func TestEfiLoaderPathDebian(t *testing.T) {
	root := t.TempDir()
	efiDir := filepath.Join(root, "boot", "efi", "EFI", "debian")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(efiDir, "grubx64.efi"), []byte("grub"), 0o644); err != nil {
		t.Fatal(err)
	}

	loader, err := efiLoaderPath(root, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	want := "\\EFI\\debian\\grubx64.efi"
	if loader != want {
		t.Errorf("got %q, want Debian grub fallback %q", loader, want)
	}
}

func TestEfiBootEntryLabel(t *testing.T) {
	tests := []struct {
		name   string
		loader string
		want   string
	}{
		{
			name:   "ubuntu shim",
			loader: `\EFI\ubuntu\shimx64.efi`,
			want:   "ubuntu",
		},
		{
			name:   "debian grub",
			loader: `\EFI\debian\grubx64.efi`,
			want:   "debian",
		},
		{
			name:   "removable fallback preserves legacy label",
			loader: `\EFI\BOOT\BOOTX64.EFI`,
			want:   "ubuntu",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := efiBootEntryLabel(tt.loader); got != tt.want {
				t.Fatalf("efiBootEntryLabel(%q) = %q, want %q", tt.loader, got, tt.want)
			}
		})
	}
}

func TestManagedEFIBootEntryLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{line: "Boot0001* ubuntu", want: true},
		{line: "Boot0002* debian", want: true},
		{line: "Boot0003* rescue-ubuntu-old HD(1,GPT,...)/File(\\EFI\\BOOT\\BOOTX64.EFI)", want: false},
		{line: "Boot0004* Windows Boot Manager HD(1,GPT,...)/File(\\EFI\\ubuntu\\shimx64.efi)", want: false},
		{line: "Boot0005* Windows Boot Manager", want: false},
		{line: "BootOrder: 0001,0002", want: false},
		{line: "Boot0006* Fedora", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := isManagedEFIBootEntryLine(tt.line); got != tt.want {
				t.Fatalf("isManagedEFIBootEntryLine(%q) = %t, want %t", tt.line, got, tt.want)
			}
		})
	}
}

func TestEfiLoaderPath_MissingLoaders(t *testing.T) {
	root := t.TempDir()
	efiDir := filepath.Join(root, "boot", "efi", "EFI", "ubuntu")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := efiLoaderPath(root, "amd64")
	if err == nil {
		t.Fatal("expected error when both shim and grub are missing")
	}
	if !strings.Contains(err.Error(), "no EFI loader found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRedactCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "password equals",
			input: "setup --password=s3cr3t --user=admin",
			want:  "setup --password=[REDACTED] --user=admin",
		},
		{
			name:  "token colon",
			input: "curl -H token:myBearerXYZ http://example.com",
			want:  "curl -H token:[REDACTED] http://example.com",
		},
		{
			name:  "secret uppercase",
			input: "configure SECRET=abc123",
			want:  "configure SECRET=[REDACTED]",
		},
		{
			name:  "key equals",
			input: "deploy key=opensesame region=us-east-1",
			want:  "deploy key=[REDACTED] region=us-east-1",
		},
		{
			name:  "credential equals",
			input: "connect credential=user:pass@host",
			want:  "connect credential=[REDACTED]",
		},
		{
			name:  "auth equals",
			input: "login auth=Bearer_token123",
			want:  "login auth=[REDACTED]",
		},
		{
			name:  "no sensitive data",
			input: "apt-get install -y curl",
			want:  "apt-get install -y curl",
		},
		{
			name:  "multiple sensitive keys",
			input: "setup password=abc token=xyz region=eu",
			want:  "setup password=[REDACTED] token=[REDACTED] region=eu",
		},
		{
			name:  "compound assignment secret",
			input: "AWS_SECRET_ACCESS_KEY=abc /usr/bin/true",
			want:  "AWS_SECRET_ACCESS_KEY=[REDACTED] /usr/bin/true",
		},
		{
			name:  "compound assignment password",
			input: "BGP_AUTH_PASSWORD=s3cr3t /usr/bin/true",
			want:  "BGP_AUTH_PASSWORD=[REDACTED] /usr/bin/true",
		},
		{
			name:  "compound flag secret",
			input: "--aws-secret-access-key=abc --region eu",
			want:  "--aws-secret-access-key=[REDACTED] --region eu",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "spaces around equals",
			input: "run password = secret123 --verbose",
			want:  "run password=[REDACTED] --verbose",
		},
		{
			name:  "double-dash password space",
			input: "--password secret --verbose",
			want:  "--password [REDACTED] --verbose",
		},
		{
			name:  "double-dash token space",
			input: "--token mytoken",
			want:  "--token [REDACTED]",
		},
		{
			name:  "single-dash password space",
			input: "-password s3cr3t",
			want:  "-password [REDACTED]",
		},
		{
			name:  "double-quoted value with spaces",
			input: `--password "very secret" --verbose`,
			want:  `--password [REDACTED] --verbose`,
		},
		{
			name:  "single-quoted value with spaces",
			input: "--password 'secret value'",
			want:  "--password [REDACTED]",
		},
		{
			name:  "tab-delimited flag value",
			input: "--password\tsecret",
			want:  "--password [REDACTED]",
		},
		{
			name:  "form-feed-delimited flag value",
			input: "--token\fsecret",
			want:  "--token [REDACTED]",
		},
		{
			name:  "assignment with double-quoted spaced value",
			input: `password="secret with space"`,
			want:  `password=[REDACTED]`,
		},
		{
			name:  "flag value containing colon",
			input: "--password abc:def --verbose",
			want:  "--password [REDACTED] --verbose",
		},
		{
			name:  "flag value containing equals",
			input: "--token foo=bar",
			want:  "--token [REDACTED]",
		},
		{
			name:  "flag quoted value containing colon",
			input: `--password "abc:def"`,
			want:  `--password [REDACTED]`,
		},
		{
			name:  "no redaction for substring match in key",
			input: "monkey=banana",
			want:  "monkey=banana",
		},
		{
			name:  "redact url",
			input: "curl https://user:pass@example.com?token=123",
			want:  "curl https://[REDACTED]@example.com?token=[REDACTED]",
		},
		{
			name:  "redact authorization header",
			input: "curl -H \"Authorization: secret123\" https://example.com",
			want:  "curl -H \"Authorization: [REDACTED]\" https://example.com",
		},
		{
			name:  "redact bearer token",
			input: "curl -H \"Auth: Bearer secret123\" https://example.com",
			want:  "curl -H \"Auth: Bearer [REDACTED] https://example.com",
		},
		{
			name:  "no redaction for monkey prefix",
			input: "cmd --monkey=banana",
			want:  "cmd --monkey=banana",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactCommand(tc.input)
			if got != tc.want {
				t.Errorf("redactCommand(%q)\n got  %q\n want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestConfigureDNSEmptyResolvers(t *testing.T) {
	t.Helper()
	c := &Configurator{rootDir: t.TempDir()}
	cfg := &config.MachineConfig{}
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("expected nil for empty resolvers, got: %v", err)
	}
}

func TestConfigureDNSSuccess(t *testing.T) {
	root := t.TempDir()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Configurator{rootDir: root}
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "8.8.8.8, 1.1.1.1"
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(etcDir, "resolv.conf"))
	if err != nil {
		t.Fatalf("cannot read resolv.conf: %v", err)
	}
	if !strings.Contains(string(data), "nameserver 8.8.8.8") {
		t.Errorf("missing nameserver 8.8.8.8 in %s", data)
	}
	if !strings.Contains(string(data), "nameserver 1.1.1.1") {
		t.Errorf("missing nameserver 1.1.1.1 in %s", data)
	}
}

func TestConfigureDNSReplacesDanglingResolvConfSymlink(t *testing.T) {
	root := t.TempDir()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(etcDir, "resolv.conf")
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", path); err != nil {
		t.Fatal(err)
	}

	c := &Configurator{rootDir: root}
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "8.8.8.8"
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat resolv.conf: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("resolv.conf is still a symlink")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if string(data) != "nameserver 8.8.8.8\n" {
		t.Fatalf("resolv.conf = %q", data)
	}
}

func TestConfigureDNSRejectsInjectedResolverDirective(t *testing.T) {
	root := t.TempDir()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &Configurator{rootDir: root}
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "8.8.8.8\nsearch evil.example"
	err := c.ConfigureDNS(cfg)
	if err == nil {
		t.Fatal("expected invalid resolver error")
	}
	if !strings.Contains(err.Error(), "invalid DNS resolver") {
		t.Fatalf("unexpected error: %v", err)
	}
	path := filepath.Join(etcDir, "resolv.conf")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("resolv.conf should not be written, stat error: %v", statErr)
	}
}

func TestConfigureDNSMissingEtcDir(t *testing.T) {
	root := t.TempDir()
	// Don't create /etc — ConfigureDNS should skip gracefully.
	c := &Configurator{rootDir: root}
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "8.8.8.8"
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("expected nil when etc/ doesn't exist, got: %v", err)
	}
}

func TestCopyTreeCancelledBeforeStart(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := copyTree(ctx, src, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestCopyTreeReplacesDanglingDestinationSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcEtc := filepath.Join(src, "etc")
	if err := os.MkdirAll(srcEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcEtc, "resolv.conf"), []byte("nameserver 192.0.2.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstEtc := filepath.Join(dst, "etc")
	if err := os.MkdirAll(dstEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", filepath.Join(dstEtc, "resolv.conf")); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(context.Background(), src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	dstFile := filepath.Join(dstEtc, "resolv.conf")
	if info, err := os.Lstat(dstFile); err != nil {
		t.Fatalf("expected resolv.conf: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("expected destination symlink to be replaced with a regular file")
	}
	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "nameserver 192.0.2.53\n" {
		t.Fatalf("resolv.conf = %q", data)
	}
}

func TestCopyTreeRejectsDestinationParentSymlinkEscape(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	outside := t.TempDir()

	srcEtc := filepath.Join(src, "etc")
	if err := os.MkdirAll(srcEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcEtc, "shadow"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "etc")); err != nil {
		t.Fatal(err)
	}

	err := copyTree(context.Background(), src, dst)
	if err == nil {
		t.Fatal("expected destination parent symlink escape to be rejected")
	}
	if !strings.Contains(err.Error(), "target escapes provisioned root") {
		t.Fatalf("expected target-root escape error, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "shadow")); !os.IsNotExist(err) {
		t.Fatalf("outside file was written: %v", err)
	}
}

func TestCopyTreeAllowsDestinationParentSymlinkInsideRoot(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcEtc := filepath.Join(src, "etc")
	if err := os.MkdirAll(srcEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcEtc, "hostname"), []byte("booty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	realEtc := filepath.Join(dst, "real-etc")
	if err := os.MkdirAll(realEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-etc", filepath.Join(dst, "etc")); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(context.Background(), src, dst); err != nil {
		t.Fatalf("copyTree returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(realEtc, "hostname"))
	if err != nil {
		t.Fatalf("expected file through safe destination parent symlink: %v", err)
	}
	if string(data) != "booty\n" {
		t.Fatalf("hostname = %q", data)
	}
}

func TestCopyFileCancelledBeforeStart(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcFile := filepath.Join(src, "file.txt")
	if err := os.WriteFile(srcFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := copyFile(ctx, srcFile, filepath.Join(dst, "file.txt"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestCopyFileReplacesDanglingDestinationSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcFile := filepath.Join(src, "file.txt")
	if err := os.WriteFile(srcFile, []byte("new data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstFile := filepath.Join(dst, "file.txt")
	if err := os.Symlink("missing-target", dstFile); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(context.Background(), srcFile, dstFile); err != nil {
		t.Fatalf("copyFile returned error: %v", err)
	}
	if info, err := os.Lstat(dstFile); err != nil {
		t.Fatalf("expected copied file: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("expected destination symlink to be replaced with a regular file")
	}
	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new data" {
		t.Fatalf("file content = %q", data)
	}
}

func TestValidateProvisionCommandBlockedTokens(t *testing.T) {
	// Semicolon and other shell metacharacters must be rejected to prevent
	// command chaining (e.g. "/bin/true; /bin/rm -rf /").
	blocked := []struct {
		name string
		cmd  string
	}{
		{"semicolon", "/bin/true; /bin/false"},
		{"pipe", "/bin/true | /bin/false"},
		{"and", "/bin/true && /bin/false"},
		{"or", "/bin/true || /bin/false"},
		{"backtick", "/bin/true `id`"},
		{"dollar-paren", "/bin/true $(id)"},
		{"redirect-out", "/bin/true > /tmp/x"},
		{"redirect-in", "/bin/true < /tmp/x"},
		{"newline", "/bin/true\n/bin/false"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateProvisionCommand(tc.cmd); err == nil {
				t.Errorf("validateProvisionCommand(%q): expected error, got nil", tc.cmd)
			}
		})
	}

	// Explicitly verify the blocked-token error message for semicolon so this
	// test fails if ";" is ever removed from blockedShellTokens.
	t.Run("semicolon-error-message", func(t *testing.T) {
		err := validateProvisionCommand("/bin/true; /bin/false")
		if err == nil {
			t.Fatal("expected error for semicolon command, got nil")
		}
		if !strings.Contains(err.Error(), `blocked shell token ";"`) {
			t.Errorf("expected error to mention blocked shell token, got: %v", err)
		}
	})

	if err := validateProvisionCommand("/bin/echo hello"); err != nil {
		t.Errorf("validateProvisionCommand safe command: unexpected error: %v", err)
	}
}

func TestConfigureGRUBDoesNotFollowSymlinkedDropIn(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	root := c.rootDir
	outside := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(outside, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	grubDir := filepath.Join(root, "etc", "default", "grub.d")
	if err := os.MkdirAll(grubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A machine-file link left by an earlier run before a resume.
	if err := os.Symlink(outside, filepath.Join(grubDir, "10-caprf-kernel-params.cfg")); err != nil {
		t.Fatal(err)
	}

	if err := c.ConfigureGRUB(context.Background(), &config.MachineConfig{}); err != nil {
		t.Fatalf("ConfigureGRUB() = %v", err)
	}

	if data, err := os.ReadFile(outside); err != nil || string(data) != "host" {
		t.Fatalf("ConfigureGRUB wrote through the symlink: %q, %v", data, err)
	}
	dropIn := filepath.Join(grubDir, "10-caprf-kernel-params.cfg")
	info, err := os.Lstat(dropIn)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("drop-in = %v (err %v), want a regular file that replaced the link", info, err)
	}
	if data, _ := os.ReadFile(dropIn); !strings.Contains(string(data), "GRUB_CMDLINE_LINUX") {
		t.Fatalf("drop-in content = %q, want the GRUB kernel parameters", data)
	}
}

func TestEFILoaderPathRejectsLinkedLoader(t *testing.T) {
	root := t.TempDir()
	hostLoader := filepath.Join(t.TempDir(), "shimx64.efi")
	if err := os.WriteFile(hostLoader, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	efiDir := filepath.Join(root, "boot", "efi", "EFI", "ubuntu")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A kept machine-file link must not count as a loader on the ESP.
	if err := os.Symlink(hostLoader, filepath.Join(efiDir, "shimx64.efi")); err != nil {
		t.Fatal(err)
	}
	if loader, err := efiLoaderPath(root, "amd64"); err == nil {
		t.Fatalf("efiLoaderPath() = %q, want error for a linked loader", loader)
	}
}
