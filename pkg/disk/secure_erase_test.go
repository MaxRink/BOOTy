//go:build linux

package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hdparm -I output of an ATA drive with the security feature set. A
// security-frozen drive prints "frozen" without the "not" column.
const hdparmSecurityNotFrozen = `/dev/sda:

ATA device, with non-removable media
	Model Number:       TEST SSD
Security: 
	Master password revision code = 65534
		supported
	not	enabled
	not	locked
	not	frozen
	not	expired: security count
		supported: enhanced erase
	2min for SECURITY ERASE UNIT. 2min for ENHANCED SECURITY ERASE UNIT.
Logical Unit WWN Device Identifier: 5000000000000000
`

var hdparmSecurityFrozen = strings.Replace(hdparmSecurityNotFrozen, "\tnot\tfrozen", "\t\tfrozen", 1)

const hdparmSecurityNotSupported = `/dev/sda:

ATA device, with non-removable media
Security: 
	Master password revision code = 65534
	not	supported
	not	enabled
	not	locked
	not	frozen
Logical Unit WWN Device Identifier: 5000000000000000
`

const hdparmNoSecuritySection = `/dev/sda:

ATA device, with non-removable media
	Model Number:       QEMU HARDDISK
Checksum: correct
`

// eraseFixture returns a manager whose sysfs has the disks of vendors
// (name -> device/vendor content, "" for no vendor file).
func eraseFixture(t *testing.T, vendors map[string]string) (*Manager, *mockCommander) {
	t.Helper()
	sys := t.TempDir()
	for name, vendor := range vendors {
		dir := filepath.Join(sys, "block", name, "device")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if vendor != "" {
			if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte(vendor), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	cmd := newMockCommander()
	return newManagerWithSysfs(cmd, sys), cmd
}

func notFound(name string) error { return fmt.Errorf("exec %s: %w", name, exec.ErrNotFound) }

func exitCode(name string, code int) error {
	return fmt.Errorf("exec %s: %w", name, makeExitError(code))
}

func TestSecureEraseDiskPerMediaType(t *testing.T) {
	vendors := map[string]string{
		"nvme0n1": "",
		"sda":     "ATA     \n",
		"sdb":     "SEAGATE \n",
		"sdc":     "",
		"vda":     "",
	}
	tests := []struct {
		name        string
		device      string
		setup       func(*mockCommander)
		wantCalls   []string // in order; a prefix of the calls
		wantErr     string   // "" = success
		unsupported bool     // errors.Is(err, ErrSecureEraseUnsupported)
		noCalls     []string // command names that must not run
	}{
		{
			name:      "nvme format",
			device:    "/dev/nvme0n1",
			wantCalls: []string{"nvme format /dev/nvme0n1 --ses=1 --force"},
			noCalls:   []string{"wipefs", "hdparm"},
		},
		{
			name:    "nvme format fails",
			device:  "/dev/nvme0n1",
			setup:   func(c *mockCommander) { c.setResult("nvme format", []byte("Invalid Field"), exitCode("nvme", 1)) },
			wantErr: "nvme format --ses=1 failed: Invalid Field",
			noCalls: []string{"wipefs"},
		},
		{
			name:        "nvme tool missing",
			device:      "/dev/nvme0n1",
			setup:       func(c *mockCommander) { c.setResult("nvme format", nil, notFound("nvme")) },
			wantErr:     "the nvme tool is not in the initramfs",
			unsupported: true,
			noCalls:     []string{"wipefs"},
		},
		{
			name:   "ata security erase",
			device: "/dev/sda",
			setup:  func(c *mockCommander) { c.setResult("hdparm -I", []byte(hdparmSecurityNotFrozen), nil) },
			wantCalls: []string{
				"hdparm -I /dev/sda",
				"hdparm --user-master u --security-set-pass",
				"hdparm --user-master u --security-erase",
				"hdparm --user-master u --security-disable",
			},
			noCalls: []string{"wipefs", "sg_sanitize", "sg_format"},
		},
		{
			name:        "ata frozen",
			device:      "/dev/sda",
			setup:       func(c *mockCommander) { c.setResult("hdparm -I", []byte(hdparmSecurityFrozen), nil) },
			wantErr:     "security-frozen",
			unsupported: true,
			noCalls:     []string{"wipefs"},
		},
		{
			name:        "ata security not supported",
			device:      "/dev/sda",
			setup:       func(c *mockCommander) { c.setResult("hdparm -I", []byte(hdparmSecurityNotSupported), nil) },
			wantErr:     "no ATA security feature set",
			unsupported: true,
			noCalls:     []string{"wipefs"},
		},
		{
			name:        "ata without security section",
			device:      "/dev/sda",
			setup:       func(c *mockCommander) { c.setResult("hdparm -I", []byte(hdparmNoSecuritySection), nil) },
			wantErr:     "no ATA security feature set",
			unsupported: true,
			noCalls:     []string{"wipefs"},
		},
		{
			name:    "ata identify fails",
			device:  "/dev/sda",
			setup:   func(c *mockCommander) { c.setResult("hdparm -I", []byte("SG_IO: bad"), exitCode("hdparm", 1)) },
			wantErr: "hdparm -I failed: SG_IO: bad",
			noCalls: []string{"wipefs"},
		},
		{
			name:        "hdparm missing",
			device:      "/dev/sda",
			setup:       func(c *mockCommander) { c.setResult("hdparm -I", nil, notFound("hdparm")) },
			wantErr:     "the hdparm tool is not in the initramfs",
			unsupported: true,
			noCalls:     []string{"wipefs"},
		},
		{
			name:   "ata erase command fails and clears the password",
			device: "/dev/sda",
			setup: func(c *mockCommander) {
				c.setResult("hdparm -I", []byte(hdparmSecurityNotFrozen), nil)
				c.setSequence("hdparm --user-master", mockResult{}, mockResult{err: exitCode("hdparm", 5)}, mockResult{})
			},
			wantCalls: []string{
				"hdparm -I /dev/sda",
				"hdparm --user-master u --security-set-pass",
				"hdparm --user-master u --security-erase",
				"hdparm --user-master u --security-disable",
			},
			wantErr: "ATA security erase failed",
			noCalls: []string{"wipefs"},
		},
		{
			name:   "ata set password fails",
			device: "/dev/sda",
			setup: func(c *mockCommander) {
				c.setResult("hdparm -I", []byte(hdparmSecurityNotFrozen), nil)
				c.setSequence("hdparm --user-master", mockResult{err: exitCode("hdparm", 5)})
			},
			wantErr: "failed to set security password",
			noCalls: []string{"wipefs"},
		},
		{
			name:      "sas sanitize crypto",
			device:    "/dev/sdb",
			wantCalls: []string{"sg_sanitize --quick --crypto /dev/sdb"},
			noCalls:   []string{"hdparm", "sg_format", "wipefs"},
		},
		{
			name:   "sas sanitize falls through rejected actions",
			device: "/dev/sdb",
			setup: func(c *mockCommander) {
				c.setSequence("sg_sanitize --quick",
					mockResult{err: exitCode("sg_sanitize", sgExitIllegalRequest)},
					mockResult{err: exitCode("sg_sanitize", sgExitInvalidOp)},
					mockResult{})
			},
			wantCalls: []string{
				"sg_sanitize --quick --crypto /dev/sdb",
				"sg_sanitize --quick --block /dev/sdb",
				"sg_sanitize --quick --overwrite --zero /dev/sdb",
			},
			noCalls: []string{"hdparm", "sg_format", "wipefs"},
		},
		{
			name:   "sas format unit when sanitize is rejected",
			device: "/dev/sdb",
			setup: func(c *mockCommander) {
				rejected := mockResult{err: exitCode("sg_sanitize", sgExitIllegalRequest)}
				c.setSequence("sg_sanitize --quick", rejected, rejected, rejected)
			},
			wantCalls: []string{
				"sg_sanitize --quick --crypto /dev/sdb",
				"sg_sanitize --quick --block /dev/sdb",
				"sg_sanitize --quick --overwrite --zero /dev/sdb",
				"sg_format --format --quick /dev/sdb",
			},
			noCalls: []string{"hdparm", "wipefs"},
		},
		{
			name:   "sas everything rejected",
			device: "/dev/sdb",
			setup: func(c *mockCommander) {
				rejected := mockResult{err: exitCode("sg_sanitize", sgExitIllegalRequest)}
				c.setSequence("sg_sanitize --quick", rejected, rejected, rejected)
				c.setResult("sg_format --format", nil, exitCode("sg_format", sgExitInvalidOp))
			},
			wantErr:     "SANITIZE --crypto rejected, SANITIZE --block rejected, SANITIZE --overwrite rejected, FORMAT UNIT rejected",
			unsupported: true,
			noCalls:     []string{"wipefs"},
		},
		{
			name:   "sas tools missing",
			device: "/dev/sdb",
			setup: func(c *mockCommander) {
				c.setResult("sg_sanitize --quick", nil, notFound("sg_sanitize"))
				c.setResult("sg_format --format", nil, notFound("sg_format"))
			},
			wantCalls:   []string{"sg_sanitize --quick --crypto /dev/sdb", "sg_format --format --quick /dev/sdb"},
			wantErr:     "sg_sanitize is not in the initramfs, sg_format is not in the initramfs",
			unsupported: true,
			noCalls:     []string{"hdparm", "wipefs"},
		},
		{
			name:   "sas sanitize fails",
			device: "/dev/sdb",
			setup: func(c *mockCommander) {
				c.setResult("sg_sanitize --quick", []byte("medium error"), exitCode("sg_sanitize", 3))
			},
			wantErr: "sg_sanitize --crypto failed: medium error",
			noCalls: []string{"sg_format", "wipefs"},
		},
		{
			name:   "sas format unit fails",
			device: "/dev/sdb",
			setup: func(c *mockCommander) {
				c.setResult("sg_sanitize --quick", nil, notFound("sg_sanitize"))
				c.setResult("sg_format --format", []byte("hardware error"), exitCode("sg_format", 4))
			},
			wantErr: "sg_format --format failed: hardware error",
			noCalls: []string{"wipefs"},
		},
		{
			name:        "scsi disk without vendor",
			device:      "/dev/sdc",
			wantErr:     "no secure erase method for this device type",
			unsupported: true,
			noCalls:     []string{"hdparm", "sg_sanitize", "wipefs"},
		},
		{
			name:        "virtio disk",
			device:      "/dev/vda",
			wantErr:     "no secure erase method for this device type",
			unsupported: true,
			noCalls:     []string{"hdparm", "nvme", "sg_sanitize", "wipefs"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, cmd := eraseFixture(t, vendors)
			if tt.setup != nil {
				tt.setup(cmd)
			}
			err := m.SecureEraseDisk(context.Background(), tt.device)
			calls := callList(cmd.calls)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("SecureEraseDisk(%s) = %v, want nil; calls %v", tt.device, err, calls)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("SecureEraseDisk(%s) = %v, want error with %q; calls %v", tt.device, err, tt.wantErr, calls)
			}
			if got := errors.Is(err, ErrSecureEraseUnsupported); got != tt.unsupported {
				t.Fatalf("errors.Is(%v, ErrSecureEraseUnsupported) = %v, want %v", err, got, tt.unsupported)
			}
			if len(calls) < len(tt.wantCalls) {
				t.Fatalf("calls %v, want prefix %v", calls, tt.wantCalls)
			}
			for i, want := range tt.wantCalls {
				if !strings.HasPrefix(calls[i], want) {
					t.Fatalf("call %d = %q, want %q; all calls %v", i, calls[i], want, calls)
				}
			}
			for _, c := range cmd.calls {
				if slices.Contains(tt.noCalls, c.name) {
					t.Fatalf("%s ran: %v", c.name, calls)
				}
			}
			if fb := m.SecureEraseFallbacks(); len(fb) != 0 {
				t.Fatalf("SecureEraseFallbacks() = %v without SECURE_ERASE_FALLBACK=wipe", fb)
			}
		})
	}
}

func TestSecureEraseFallbackWipe(t *testing.T) {
	vendors := map[string]string{"sda": "ATA\n", "nvme0n1": ""}

	t.Run("frozen drive gets the wipe and is reported", func(t *testing.T) {
		m, cmd := eraseFixture(t, vendors)
		m = m.WithQuickWipe(true)
		m.SetSecureEraseFallbackWipe(true)
		cmd.setResult("hdparm -I", []byte(hdparmSecurityFrozen), nil)
		if err := m.SecureEraseDisk(context.Background(), "/dev/sda"); err != nil {
			t.Fatalf("SecureEraseDisk() = %v, want the wipe fallback", err)
		}
		if !slices.Contains(callList(cmd.calls), "wipefs -af /dev/sda") {
			t.Fatalf("no wipe of /dev/sda: %v", callList(cmd.calls))
		}
		if got := m.SecureEraseFallbacks(); !slices.Equal(got, []string{"/dev/sda"}) {
			t.Fatalf("SecureEraseFallbacks() = %v, want [/dev/sda]", got)
		}
	})

	t.Run("failed nvme format gets the wipe", func(t *testing.T) {
		m, cmd := eraseFixture(t, vendors)
		m = m.WithQuickWipe(true)
		m.SetSecureEraseFallbackWipe(true)
		cmd.setResult("nvme format", nil, exitCode("nvme", 1))
		if err := m.SecureEraseDisk(context.Background(), "/dev/nvme0n1"); err != nil {
			t.Fatalf("SecureEraseDisk() = %v, want the wipe fallback", err)
		}
		if got := m.SecureEraseFallbacks(); !slices.Equal(got, []string{"/dev/nvme0n1"}) {
			t.Fatalf("SecureEraseFallbacks() = %v", got)
		}
	})

	t.Run("successful erase is not a fallback", func(t *testing.T) {
		m, cmd := eraseFixture(t, vendors)
		m.SetSecureEraseFallbackWipe(true)
		if err := m.SecureEraseDisk(context.Background(), "/dev/nvme0n1"); err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(cmd.calls, func(c mockCall) bool { return c.name == "wipefs" }) {
			t.Fatalf("wipe after a successful erase: %v", callList(cmd.calls))
		}
		if got := m.SecureEraseFallbacks(); len(got) != 0 {
			t.Fatalf("SecureEraseFallbacks() = %v, want none", got)
		}
	})

	t.Run("failed wipe reports both errors", func(t *testing.T) {
		m, cmd := eraseFixture(t, vendors)
		m = m.WithQuickWipe(true)
		m.SetSecureEraseFallbackWipe(true)
		cmd.setResult("hdparm -I", []byte(hdparmSecurityFrozen), nil)
		cmd.setResult("wipefs -af", nil, errors.New("wipefs broke"))
		err := m.SecureEraseDisk(context.Background(), "/dev/sda")
		if err == nil || !errors.Is(err, ErrSecureEraseUnsupported) || !strings.Contains(err.Error(), "wipe fallback failed: wipefs broke") {
			t.Fatalf("SecureEraseDisk() = %v, want the erase and the wipe error", err)
		}
		if got := m.SecureEraseFallbacks(); len(got) != 0 {
			t.Fatalf("SecureEraseFallbacks() = %v after a failed wipe", got)
		}
	})

	t.Run("canceled run does not wipe", func(t *testing.T) {
		m, cmd := eraseFixture(t, vendors)
		m.SetSecureEraseFallbackWipe(true)
		cmd.setResult("hdparm -I", []byte(hdparmSecurityFrozen), nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := m.SecureEraseDisk(ctx, "/dev/sda"); err == nil {
			t.Fatal("SecureEraseDisk() = nil on a canceled run")
		}
		if slices.ContainsFunc(cmd.calls, func(c mockCall) bool { return c.name == "wipefs" }) {
			t.Fatalf("wipe on a canceled run: %v", callList(cmd.calls))
		}
	})
}

func TestSecureEraseAllDisksUsesMediaMethodAndFails(t *testing.T) {
	m, cmd := eraseFixture(t, map[string]string{"nvme0n1": "", "sda": "ATA\n", "loop0": ""})
	cmd.setResult("hdparm -I", []byte(hdparmNoSecuritySection), nil)
	_, err := m.SecureEraseAllDisks(context.Background())
	if !errors.Is(err, ErrSecureEraseUnsupported) {
		t.Fatalf("SecureEraseAllDisks() = %v, want the unsupported sda error", err)
	}
	calls := callList(cmd.calls)
	if !slices.Contains(calls, "nvme format /dev/nvme0n1 --ses=1 --force") || !slices.Contains(calls, "hdparm -I /dev/sda") {
		t.Fatalf("calls %v, want nvme format on nvme0n1 and hdparm on sda", calls)
	}
	if slices.ContainsFunc(cmd.calls, func(c mockCall) bool { return c.name == "wipefs" }) {
		t.Fatalf("silent wipe fallback: %v", calls)
	}
}

func TestATASecurityState(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		out                     string
		wantSupported, wantFroz bool
	}{
		{"not frozen", hdparmSecurityNotFrozen, true, false},
		{"frozen", hdparmSecurityFrozen, true, true},
		{"not supported", hdparmSecurityNotSupported, false, false},
		{"no section", hdparmNoSecuritySection, false, false},
		// The state words after the section belong to another section.
		{"words after the section", "Security: \n\tnot\tsupported\nOther:\n\t\tsupported\n\t\tfrozen\n", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			supported, frozen := ataSecurityState(tt.out)
			if supported != tt.wantSupported || frozen != tt.wantFroz {
				t.Fatalf("ataSecurityState() = %v, %v; want %v, %v", supported, frozen, tt.wantSupported, tt.wantFroz)
			}
		})
	}
}
