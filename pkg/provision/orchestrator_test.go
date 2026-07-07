//go:build linux

package provision

import (
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/telekom/BOOTy/pkg/cloudinit"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/firmware"
	"github.com/telekom/BOOTy/pkg/image"
	networkpersist "github.com/telekom/BOOTy/pkg/network/persist"
)

// newTestOrchestrator builds an Orchestrator with a mock provider and disk manager
// suitable for unit testing individual steps.
func newTestOrchestrator(t *testing.T, cfg *config.MachineConfig, provider *mockProvider) *Orchestrator {
	t.Helper()
	o, _ := newTestOrchestratorWithCommander(t, cfg, provider)
	return o
}

func newTestOrchestratorWithCommander(t *testing.T, cfg *config.MachineConfig, provider *mockProvider) (*Orchestrator, *mockCommander) {
	t.Helper()
	cmd := newMockCommander()
	mgr := disk.NewManager(cmd)
	cfg.Provision.Image.AllowInsecureHTTP = true
	cfg.Provision.Image.AllowInsecureHTTP = true
	o := NewOrchestrator(cfg, provider, mgr)
	o.config.rootDir = t.TempDir()
	return o, cmd
}

func withProcCmdline(t *testing.T, cmdline string) {
	t.Helper()
	previous := readProcCmdline
	readProcCmdline = func() ([]byte, error) {
		return []byte(cmdline), nil
	}
	t.Cleanup(func() {
		readProcCmdline = previous
	})
}

func withABSlotStateMount(t *testing.T, slot string) {
	t.Helper()
	previous := mountReadOnlyPart
	mountReadOnlyPart = func(_ context.Context, _ *disk.Manager, _ string, mountpoint string) error {
		stateDir := filepath.Join(mountpoint, "etc", "booty")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(stateDir, "ab-slot.env"), []byte("BOOTY_AB_BOOTED_SLOT="+slot+"\n"), 0o644)
	}
	t.Cleanup(func() {
		mountReadOnlyPart = previous
	})
}

func sfdiskJSON(t *testing.T, parts []disk.Partition) []byte {
	t.Helper()
	var out struct {
		PartitionTable struct {
			Partitions []disk.Partition `json:"partitions"`
		} `json:"partitiontable"`
	}
	out.PartitionTable.Partitions = parts
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal sfdisk json: %v", err)
	}
	return data
}

func hasCommandName(calls []mockCall, name string) bool {
	for _, call := range calls {
		if call.name == name {
			return true
		}
	}
	return false
}

func hasCommandCall(calls []mockCall, name string, args ...string) bool {
	for _, call := range calls {
		if call.name != name || len(call.args) != len(args) {
			continue
		}
		match := true
		for i := range args {
			if call.args[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func requireStepIndex(t *testing.T, steps []Step, name string) int {
	t.Helper()
	for i, step := range steps {
		if step.Name == name {
			return i
		}
	}
	t.Fatalf("missing step %q", name)
	return -1
}

func commandIndex(calls []mockCall, name string, args ...string) int {
	for i, call := range calls {
		if call.name != name || len(call.args) != len(args) {
			continue
		}
		match := true
		for j := range args {
			if call.args[j] != args[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func wipeCommandCalls(calls []mockCall) []mockCall {
	var out []mockCall
	for _, call := range calls {
		if call.name == "sgdisk" || call.name == "wipefs" {
			out = append(out, call)
		}
	}
	return out
}

func TestClassifyImageStreamErrorChecksumMismatchIsPermanent(t *testing.T) {
	err := classifyImageStreamError("https://images.local/node.raw", errors.New("checksum mismatch: computed=bad want=good"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !isPermanent(err) {
		t.Fatalf("checksum mismatch should be permanent: %T %[1]v", err)
	}
	if !strings.Contains(err.Error(), "streaming https://images.local/node.raw") {
		t.Fatalf("error should include image URL context, got %q", err.Error())
	}
}

func TestClassifyImageStreamErrorNonChecksumRemainsRetryable(t *testing.T) {
	err := classifyImageStreamError("https://images.local/node.raw", errors.New("connection reset by peer"))
	if err == nil {
		t.Fatal("expected error")
	}
	if isPermanent(err) {
		t.Fatalf("transient stream failure should not be permanent: %T %[1]v", err)
	}
}

func TestClassifyImageStreamErrorRedactsSensitiveURLParts(t *testing.T) {
	rawURL := "https://user:secret@images.local/node.raw?token=abc#frag"
	cause := fmt.Errorf("fetching %s failed", rawURL)
	err := classifyImageStreamError(rawURL, cause)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("error leaked sensitive URL parts: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "https://images.local/node.raw") {
		t.Fatalf("error = %q, want redacted URL context", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error should preserve original cause: %v", err)
	}
}

func TestClassifyImageStreamErrorRedactsSensitiveURLWithoutUserinfo(t *testing.T) {
	rawURL := "https://images.local/node.raw?token=abc#frag"
	cause := fmt.Errorf("fetching %s failed", rawURL)
	err := classifyImageStreamError(rawURL, cause)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("error leaked sensitive URL parts: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "https://images.local/node.raw") {
		t.Fatalf("error = %q, want redacted URL context", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error should preserve original cause: %v", err)
	}
}

func TestProvisionStepCount(t *testing.T) {
	// Verify the pipeline has the expected number of steps.
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	// Use the shared provisionSteps() method from orchestrator.go.
	steps := o.provisionSteps()
	if len(steps) != 44 {
		t.Fatalf("expected 44 provisioning steps, got %d", len(steps))
	}
}

func TestProvisionStepsPrepareOCIPrePullsAfterResizeBeforeBootConfig(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.provisionSteps()

	resizeIdx := requireStepIndex(t, steps, "resize-filesystem")
	prePullIdx := requireStepIndex(t, steps, "prepare-oci-prepulls")
	grubIdx := requireStepIndex(t, steps, "configure-grub")
	successIdx := requireStepIndex(t, steps, "report-success")

	if resizeIdx >= prePullIdx {
		t.Fatalf("prepare-oci-prepulls index %d must be after resize-filesystem index %d", prePullIdx, resizeIdx)
	}
	if prePullIdx >= grubIdx {
		t.Fatalf("prepare-oci-prepulls index %d must be before configure-grub index %d", prePullIdx, grubIdx)
	}
	if prePullIdx >= successIdx {
		t.Fatalf("prepare-oci-prepulls index %d must be before report-success index %d", prePullIdx, successIdx)
	}
}

func TestProvisionStepsValidateImageSourceBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.provisionSteps()

	indices := map[string]int{}
	for i, step := range steps {
		indices[step.Name] = i
	}
	validateIdx, ok := indices["validate-provision-inputs"]
	if !ok {
		t.Fatal("missing validate-provision-inputs step")
	}
	for _, name := range []string{"stop-raid", "disable-lvm", "setup-nvme-namespaces", "setup-raid", "detect-disk", "wipe-disks"} {
		stepIdx, ok := indices[name]
		if !ok {
			t.Fatalf("missing %s step", name)
		}
		if validateIdx >= stepIdx {
			t.Fatalf("validate-provision-inputs index %d must be before %s index %d", validateIdx, name, stepIdx)
		}
	}
}

func TestProvisionStepsVerifyImageBeforeDestructiveStorage(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.provisionSteps()

	indices := map[string]int{}
	for i, step := range steps {
		indices[step.Name] = i
	}
	verifyIdx, ok := indices["verify-image"]
	if !ok {
		t.Fatal("missing verify-image step")
	}
	validateIdx, ok := indices["validate-provision-inputs"]
	if !ok {
		t.Fatal("missing validate-provision-inputs step")
	}
	if validateIdx >= verifyIdx {
		t.Fatalf("validate-provision-inputs index %d must be before verify-image index %d", validateIdx, verifyIdx)
	}
	for _, name := range []string{"stop-raid", "disable-lvm", "setup-nvme-namespaces", "setup-raid", "detect-disk", "wipe-disks"} {
		stepIdx, ok := indices[name]
		if !ok {
			t.Fatalf("missing %s step", name)
		}
		if verifyIdx >= stepIdx {
			t.Fatalf("verify-image index %d must be before %s index %d", verifyIdx, name, stepIdx)
		}
	}
}

func TestProvisionStepsDisableLVMBeforeStoppingRAID(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.provisionSteps()

	disableIdx := requireStepIndex(t, steps, "disable-lvm")
	stopIdx := requireStepIndex(t, steps, "stop-raid")
	wipeIdx := requireStepIndex(t, steps, "wipe-disks")

	if disableIdx >= stopIdx {
		t.Fatalf("disable-lvm index %d must be before stop-raid index %d", disableIdx, stopIdx)
	}
	if stopIdx >= wipeIdx {
		t.Fatalf("stop-raid index %d must be before wipe-disks index %d", stopIdx, wipeIdx)
	}
}

func TestProvisionStepsSetupRAIDAfterNVMeBeforeDetectDisk(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.provisionSteps()

	indices := map[string]int{}
	for i, step := range steps {
		indices[step.Name] = i
	}
	for _, name := range []string{"setup-nvme-namespaces", "setup-raid", "detect-disk"} {
		if _, ok := indices[name]; !ok {
			t.Fatalf("missing step %q", name)
		}
	}
	if indices["setup-nvme-namespaces"] >= indices["setup-raid"] ||
		indices["setup-raid"] >= indices["detect-disk"] {
		t.Fatalf("unexpected raid setup order: %#v", indices)
	}
}

func TestHardDeprovisionStepsDisableLVMBeforeStoppingRAID(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "hard"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.deprovisionSteps("hard")

	selectIdx := requireStepIndex(t, steps, "select-deprovision-disk")
	disableIdx := requireStepIndex(t, steps, "disable-lvm")
	stopIdx := requireStepIndex(t, steps, "stop-raid")
	wipeIdx := requireStepIndex(t, steps, "wipe-disks")

	if selectIdx >= disableIdx {
		t.Fatalf("select-deprovision-disk index %d must be before disable-lvm index %d", selectIdx, disableIdx)
	}
	if disableIdx >= stopIdx {
		t.Fatalf("disable-lvm index %d must be before stop-raid index %d", disableIdx, stopIdx)
	}
	if stopIdx >= wipeIdx {
		t.Fatalf("stop-raid index %d must be before wipe-disks index %d", stopIdx, wipeIdx)
	}
}

func TestValidateImageSourceConfiguredRejectsMissingImage(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected missing image source error")
	}
	if !strings.Contains(err.Error(), "no image URLs configured") {
		t.Fatalf("error = %q, want no image URLs context", err.Error())
	}
}

func TestValidateImageSourceConfiguredRejectsBlankImage(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{" ", "\t"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected missing image source error")
	}
	if !strings.Contains(err.Error(), "no image URLs configured") {
		t.Fatalf("error = %q, want no image URLs context", err.Error())
	}
}

func TestValidateImageSourceConfiguredAllowsImage(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()
	pinnedOCI := "oci://registry.example.invalid/tcaas/node@sha256:" + strings.Repeat("a", 64)

	tests := []struct {
		name     string
		source   string
		checksum string
		bestURL  string
	}{
		{
			name:    "https",
			source:  "https://images.example.invalid/node.raw",
			bestURL: pinnedOCI,
		},
		{
			name:   "http",
			source: srv.URL + "/node.raw",
		},
		{
			name:     "oci tag with checksum",
			source:   "oci://registry.example.invalid/tcaas/node:v1",
			checksum: strings.Repeat("b", 64),
		},
		{
			name:     "oci uppercase scheme with checksum",
			source:   "OCI://registry.example.invalid/tcaas/node:v1",
			checksum: strings.Repeat("b", 64),
		},
		{
			name:   "oci digest",
			source: "oci://registry.example.invalid/tcaas/node@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			name:   "oci uppercase digest",
			source: "OCI://registry.example.invalid/tcaas/node@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.TargetOS = config.TargetOSLinux
			cfg.Provision.Image.URLs = []string{tt.source}
			cfg.Provision.Image.Checksum = tt.checksum
			o := newTestOrchestrator(t, cfg, &mockProvider{})
			o.bestImageURL = tt.bestURL

			if err := o.validateProvisionInputs(context.Background()); err != nil {
				t.Fatalf("validateProvisionInputs: %v", err)
			}
		})
	}
}

func TestValidateImageSourceConfiguredRejectsInvalidSources(t *testing.T) {
	tests := []struct {
		name    string
		sources []string
		want    string
	}{
		{
			name:    "file URL",
			sources: []string{"file:///tmp/node.raw"},
			want:    `unsupported image source scheme "file"`,
		},
		{
			name:    "typo scheme",
			sources: []string{"htps://images.example.invalid/node.raw"},
			want:    `unsupported image source scheme "htps"`,
		},
		{
			name:    "relative path",
			sources: []string{"node.raw"},
			want:    "unsupported image source without scheme",
		},
		{
			name:    "https missing host",
			sources: []string{"https:///node.raw"},
			want:    "missing host",
		},
		{
			name:    "https missing hostname",
			sources: []string{"https://:443/node.raw"},
			want:    "missing host",
		},
		{
			name:    "bad oci ref",
			sources: []string{"oci://registry.example.invalid/%zz"},
			want:    "invalid OCI image source",
		},
		{
			name:    "valid source plus invalid fallback",
			sources: []string{"https://images.example.invalid/node.raw", "file:///tmp/node.raw"},
			want:    `unsupported image source scheme "file"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.TargetOS = config.TargetOSLinux
			cfg.Provision.Image.URLs = tt.sources
			o := newTestOrchestrator(t, cfg, &mockProvider{})

			err := o.validateProvisionInputs(context.Background())
			if err == nil {
				t.Fatal("expected invalid image source error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestValidateProvisionInputsRejectsQCOW2WithoutQemuImg(t *testing.T) {
	srv := newTestImageServer(t, append([]byte{0x51, 0x46, 0x49, 0xfb}, []byte("qcow2 payload")...))
	defer srv.Close()
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{srv.URL + "/node.qcow2"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected qcow2 prerequisite error")
	}
	if !strings.Contains(err.Error(), "before destructive storage") ||
		!strings.Contains(err.Error(), "qemu-img") {
		t.Fatalf("error = %q, want pre-wipe qemu-img context", err.Error())
	}
}

func TestValidateProvisionInputsRejectsQCOW2ByNameWithoutQemuImg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"https://127.0.0.1:1/node.qcow2.gz"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected qcow2 prerequisite error")
	}
	if !strings.Contains(err.Error(), "before destructive storage") ||
		!strings.Contains(err.Error(), "qemu-img") {
		t.Fatalf("error = %q, want pre-wipe qemu-img context", err.Error())
	}
}

func TestValidateProvisionInputsAllowsRawWithoutQemuImg(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{srv.URL + "/node.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("expected raw image preflight to pass, got %v", err)
	}
}

func TestValidateProvisionInputsAllowsTransientRawProbeFailure(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"https://127.0.0.1:1/node.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("expected transient image probe failure to warn and continue, got %v", err)
	}
}

func TestValidateImageSourceConfiguredNormalizesImageSources(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()

	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"  " + srv.URL + "/node.raw  ", "\t"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("validateProvisionInputs: %v", err)
	}
	if got := cfg.Provision.Image.URLs; got[0] != srv.URL+"/node.raw" || got[1] != "" {
		t.Fatalf("image URLs were not normalized: %#v", got)
	}
}

func TestValidateImageSourceConfiguredRedactsSensitiveInvalidSource(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"https://robot:secret@images.example.invalid/%zz?token=abc#frag"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected invalid image source error")
	}
	for _, leaked := range []string{"robot", "secret", "token=abc", "#frag"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error leaked %q: %q", leaked, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "[redacted invalid URL]") {
		t.Fatalf("error = %q, want invalid URL redaction", err.Error())
	}
	requireWrappedOriginalError(t, err)
}

func TestValidateImageSourceConfiguredRedactsSensitiveOCIReference(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"oci://robot:secret@registry.example.invalid/%zz?token=abc#frag"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected invalid OCI image source error")
	}
	for _, leaked := range []string{"robot", "secret", "token=abc", "#frag"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error leaked %q: %q", leaked, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "[redacted invalid URL]") {
		t.Fatalf("error = %q, want invalid OCI source redaction", err.Error())
	}
	requireWrappedOriginalError(t, err)
}

func requireWrappedOriginalError(t *testing.T, err error) {
	t.Helper()
	redactedErr := errors.Unwrap(err)
	if redactedErr == nil {
		t.Fatalf("error %q does not wrap a redacted parser error", err)
	}
	if originalErr := errors.Unwrap(redactedErr); originalErr == nil {
		t.Fatalf("redacted parser error %q does not wrap the original parser error", redactedErr)
	}
}

func TestValidateImageSourceRejectsUnpinnedOCIWithoutChecksum(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{
		"https://images.example.invalid/node.raw",
		"oci://registry.example.invalid/org/node:latest",
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected unpinned OCI source error")
	}
	if !strings.Contains(err.Error(), "must use a digest reference or IMAGE_CHECKSUM") {
		t.Fatalf("error = %q, want OCI pinning context", err.Error())
	}
}

func TestValidateImageSourceAllowsOCIDigest(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"oci://registry.example.invalid/org/node@sha256:" + strings.Repeat("a", 64)}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("validateProvisionInputs: %v", err)
	}
}

func TestValidateImageSourceAllowsOCITagWithChecksum(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"oci://registry.example.invalid/org/node:latest"}
	cfg.Provision.Image.Checksum = "  " + strings.Repeat("b", 64) + "  "
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("validateProvisionInputs: %v", err)
	}
	if cfg.Provision.Image.Checksum != strings.Repeat("b", 64) {
		t.Fatalf("checksum was not normalized: %q", cfg.Provision.Image.Checksum)
	}
}

func TestValidateImageSourceRejectsMalformedOCI(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"oci://registry.example.invalid/%zz"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected malformed OCI source error")
	}
	if !strings.Contains(err.Error(), "invalid OCI image source") {
		t.Fatalf("error = %q, want invalid OCI context", err.Error())
	}
}

func TestValidateProvisionInputsRejectsGPGSignatureWithoutChecksum(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/node.raw"}
	cfg.Provision.Image.SignatureURL = "https://images.example.invalid/node.raw.sig"
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected missing checksum error")
	}
	if !strings.Contains(err.Error(), "image signature URL (IMAGE_SIGNATURE_URL) requires image checksum (IMAGE_CHECKSUM)") {
		t.Fatalf("error = %q, want signature checksum context", err.Error())
	}
}

func TestValidateProvisionInputsAllowsGPGSignatureWithChecksum(t *testing.T) {
	srv := newTestImageServer(t, []byte("booty raw image"))
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{srv.URL + "/node.raw"}
	cfg.Provision.Image.SignatureURL = " https://images.example.invalid/node.raw.sig "
	cfg.Provision.Image.Checksum = strings.Repeat("a", 64)
	cfg.Provision.Image.ChecksumType = "sha256"
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("validateProvisionInputs: %v", err)
	}
	if got := cfg.Provision.Image.SignatureURL; got != "https://images.example.invalid/node.raw.sig" {
		t.Fatalf("SignatureURL = %q, want trimmed URL", got)
	}
}

func TestValidateProvisionInputsRequiresTargetOSBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/linux.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("validateProvisionInputs() error = nil, want missing target OS")
	}
	if !strings.Contains(err.Error(), "provision.targetOS required") ||
		!strings.Contains(err.Error(), "before destructive storage steps") {
		t.Fatalf("error = %q, want missing target OS preflight context", err.Error())
	}
}

func TestValidateProvisionInputsRejectsWindowsTargetBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = "windows"
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/windows.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("validateProvisionInputs() error = nil, want unsupported Windows target")
	}
	if !strings.Contains(err.Error(), "Windows targets are not supported") ||
		!strings.Contains(err.Error(), "before destructive storage steps") {
		t.Fatalf("error = %q, want unsupported Windows preflight context", err.Error())
	}
}

func TestValidateProvisionInputsRejectsESXiTargetBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = "vmware-esxi"
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/esxi.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("validateProvisionInputs() error = nil, want unsupported ESXi target")
	}
	if !strings.Contains(err.Error(), "VMware ESXi targets are not supported") ||
		!strings.Contains(err.Error(), "before destructive storage steps") {
		t.Fatalf("error = %q, want unsupported ESXi preflight context", err.Error())
	}
}

func TestValidateProvisionInputsRejectsRHELLikeFamilyBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.OSFamily = "rhel"
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/rhel.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("validateProvisionInputs() error = nil, want unsupported RHEL-like target")
	}
	if !strings.Contains(err.Error(), "rhel-like target bootloader support is not implemented") ||
		!strings.Contains(err.Error(), `osFamily="rhel"`) ||
		!strings.Contains(err.Error(), "before destructive storage steps") {
		t.Fatalf("error = %q, want unsupported RHEL-like preflight context", err.Error())
	}
}

func TestValidateProvisionInputsAllowsLinuxTarget(t *testing.T) {
	srv := newTestImageServer(t, []byte("booty raw image"))
	defer srv.Close()

	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = " Linux "
	cfg.Provision.Image.URLs = []string{srv.URL + "/linux.raw"}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.validateProvisionInputs(context.Background()); err != nil {
		t.Fatalf("validateProvisionInputs: %v", err)
	}
	if cfg.Provision.TargetOS != config.TargetOSLinux {
		t.Fatalf("Provision.TargetOS = %q, want %q", cfg.Provision.TargetOS, config.TargetOSLinux)
	}
}

func TestValidateProvisionInputsRejectsPartitionModeWithPartitionLayout(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.Mode = config.ImageModePartition
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/node.raw"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected partition image mode to reject declarative partition layout")
	}
	if !strings.Contains(err.Error(), "cannot be combined with partition layout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateProvisionInputsRejectsUnsupportedPartitionLayoutMountpoint(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/node.raw"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Filesystem: "ext4", Mountpoint: "/"},
			{Label: "var", Filesystem: "ext4", Mountpoint: "/var"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected unsupported non-root mountpoint error")
	}
	if !strings.Contains(err.Error(), `mountpoint "/var" is not supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateProvisionInputsRejectsLVMBootEFIMountpoint(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Image.URLs = []string{"https://images.example.invalid/node.raw"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "pv", SizeMB: 8192},
		},
		LVM: &config.LVMConfig{
			VolumeGroup: "sysvg",
			PVPartition: 1,
			Volumes: []config.LVVolume{
				{Name: "root", Filesystem: "ext4", Mountpoint: "/"},
				{Name: "efi", Filesystem: "vfat", Mountpoint: "/boot/efi"},
			},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected LVM /boot/efi mountpoint to fail")
	}
	if !strings.Contains(err.Error(), `lvm volume mountpoint "/boot/efi" is not supported`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMountBootAndSharedDataStepsPrecedeProvisioningWrites(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	steps := o.provisionSteps()

	indices := map[string]int{}
	for i, step := range steps {
		indices[step.Name] = i
	}
	for _, name := range []string{"mount-root", "mount-boot", "mount-shared-data", "copy-provisioner-files", "apply-sysexts", "configure-grub", "install-efi-fallback", "verify-secureboot-chain", "create-efi-boot-entry", "teardown-chroot"} {
		if _, ok := indices[name]; !ok {
			t.Fatalf("missing step %q", name)
		}
	}
	if indices["mount-root"] >= indices["mount-boot"] ||
		indices["mount-boot"] >= indices["mount-shared-data"] ||
		indices["mount-shared-data"] >= indices["copy-provisioner-files"] ||
		indices["mount-shared-data"] >= indices["apply-sysexts"] ||
		indices["apply-sysexts"] >= indices["configure-grub"] ||
		indices["configure-grub"] >= indices["install-efi-fallback"] ||
		indices["install-efi-fallback"] >= indices["verify-secureboot-chain"] ||
		indices["verify-secureboot-chain"] >= indices["create-efi-boot-entry"] ||
		indices["create-efi-boot-entry"] >= indices["teardown-chroot"] {
		t.Fatalf("unexpected boot mount ordering: %#v", indices)
	}
}

func TestResumeStateStepsRerunMountSharedDataForCleanupState(t *testing.T) {
	stateSteps := resumeStateSteps()
	if _, ok := stateSteps["validate-provision-inputs"]; !ok {
		t.Fatal("validate-provision-inputs must rerun on resume before destructive storage steps")
	}
	if _, ok := stateSteps["verify-image"]; !ok {
		t.Fatal("verify-image must rerun on resume so the verified and streamed image source cannot diverge")
	}
	if _, ok := stateSteps["mount-efivarfs"]; !ok {
		t.Fatal("mount-efivarfs must rerun on resume because efivarfs is volatile after restart")
	}
	if _, ok := stateSteps["enable-lvm"]; !ok {
		t.Fatal("enable-lvm must rerun on resume because activated LVM devices are volatile after restart")
	}
	if _, ok := stateSteps["setup-raid"]; ok {
		t.Fatal("setup-raid must not rerun on resume because it wipes member devices before creating arrays")
	}
	if _, ok := stateSteps["mount-shared-data"]; !ok {
		t.Fatal("mount-shared-data must rerun on resume to rebuild sharedMounts for teardown cleanup")
	}
	if _, ok := stateSteps["setup-nvme-namespaces"]; ok {
		t.Fatal("setup-nvme-namespaces must not rerun on resume because it deletes and recreates namespaces")
	}
	if _, ok := stateSteps["teardown-chroot"]; !ok {
		t.Fatal("teardown-chroot must rerun on resume when setup-chroot-binds reruns")
	}
	if _, ok := stateSteps["set-hostname"]; ok {
		t.Fatal("set-hostname should remain skippable after resume")
	}
}

func TestMountSharedDataMountsSystemABDataPartitions(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.Scheme = config.ABSchemeSystemAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.DataPartitions = []config.ABDataPartition{
		{Label: "BOOTY-VAR", Mountpoint: "/var", SizeMB: 1024},
		{Label: "BOOTY-HOME", Mountpoint: "/home"},
	}
	layout, err := cfg.Provision.AB.PartitionLayout("/dev/sda")
	if err != nil {
		t.Fatalf("PartitionLayout: %v", err)
	}
	cfg.Provision.Disk.PartitionLayout = layout
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	oldMountPoint := isMountPoint
	isMountPoint = func(string) bool { return false }
	t.Cleanup(func() { isMountPoint = oldMountPoint })

	oldMountShared := mountSharedDataPart
	var mounted []string
	mountSharedDataPart = func(_ context.Context, _ *disk.Manager, device, mountpoint string) error {
		mounted = append(mounted, device+"="+mountpoint)
		return nil
	}
	t.Cleanup(func() { mountSharedDataPart = oldMountShared })

	if err := o.mountSharedData(context.Background()); err != nil {
		t.Fatalf("mountSharedData: %v", err)
	}
	want := []string{"/dev/sda4=/newroot/var", "/dev/sda5=/newroot/home"}
	if strings.Join(mounted, ",") != strings.Join(want, ",") {
		t.Fatalf("mounted = %#v, want %#v", mounted, want)
	}
}

func TestMountSharedDataRecordsAlreadyMountedSystemABDataPartitions(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.Scheme = config.ABSchemeSystemAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.DataPartitions = []config.ABDataPartition{
		{Label: "BOOTY-VAR", Mountpoint: "/var", SizeMB: 1024},
		{Label: "BOOTY-HOME", Mountpoint: "/home"},
	}
	layout, err := cfg.Provision.AB.PartitionLayout("/dev/sda")
	if err != nil {
		t.Fatalf("PartitionLayout: %v", err)
	}
	cfg.Provision.Disk.PartitionLayout = layout
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool {
		return path == filepath.Join(newroot, "var") || path == filepath.Join(newroot, "home")
	}
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountedSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		switch path {
		case filepath.Join(newroot, "var"):
			return "/dev/sda4", true
		case filepath.Join(newroot, "home"):
			return "/dev/sda5", true
		default:
			return "", false
		}
	}
	t.Cleanup(func() { mountedSource = oldMountedSource })

	oldMountShared := mountSharedDataPart
	mountSharedDataPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		t.Fatal("already-mounted shared data partitions must not be mounted again")
		return nil
	}
	t.Cleanup(func() { mountSharedDataPart = oldMountShared })

	if err := o.mountSharedData(context.Background()); err != nil {
		t.Fatalf("mountSharedData: %v", err)
	}
	want := []string{filepath.Join(newroot, "var"), filepath.Join(newroot, "home")}
	if strings.Join(o.sharedMounts, ",") != strings.Join(want, ",") {
		t.Fatalf("sharedMounts = %#v, want %#v", o.sharedMounts, want)
	}
}

func TestMountSharedDataFailsWhenAlreadyMountedFromUnexpectedPartition(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.Scheme = config.ABSchemeSystemAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.DataPartitions = []config.ABDataPartition{
		{Label: "BOOTY-VAR", Mountpoint: "/var", SizeMB: 1024},
	}
	layout, err := cfg.Provision.AB.PartitionLayout("/dev/sda")
	if err != nil {
		t.Fatalf("PartitionLayout: %v", err)
	}
	cfg.Provision.Disk.PartitionLayout = layout
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	target := filepath.Join(newroot, "var")
	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return path == target }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountedSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		if path == target {
			return "/dev/sdb4", true
		}
		return "", false
	}
	t.Cleanup(func() { mountedSource = oldMountedSource })
	oldMountShared := mountSharedDataPart
	mountSharedDataPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		t.Fatal("mismatched shared data partition must not be mounted")
		return nil
	}
	t.Cleanup(func() { mountSharedDataPart = oldMountShared })

	err = o.mountSharedData(context.Background())
	if err == nil {
		t.Fatal("expected shared data mount source mismatch error")
	}
	if !strings.Contains(err.Error(), "expected shared data partition /dev/sda4") {
		t.Fatalf("mountSharedData error = %q", err.Error())
	}
}

func TestCleanupSharedDataSeedMountKeepsMountedTreeOnUnmountFailure(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	seedMount := filepath.Join(t.TempDir(), "seed")
	if err := os.Mkdir(seedMount, 0o755); err != nil {
		t.Fatalf("create seed mount: %v", err)
	}
	sentinel := filepath.Join(seedMount, "shared-data")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	oldUnmountShared := unmountSharedDataPart
	unmountSharedDataPart = func(_ *disk.Manager, _ string) error {
		return errors.New("device busy")
	}
	t.Cleanup(func() { unmountSharedDataPart = oldUnmountShared })

	o.cleanupSharedDataSeedMount(seedMount, true)

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel should remain after unmount failure: %v", err)
	}
}

func TestInterruptedSharedDataSeedIsCleanedForRetry(t *testing.T) {
	dst := t.TempDir()
	if err := os.Mkdir(filepath.Join(dst, "lost+found"), 0o700); err != nil {
		t.Fatalf("create lost+found: %v", err)
	}
	if err := writeSeedInProgressMarker(dst); err != nil {
		t.Fatalf("write seed marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "partial", "nested"), 0o755); err != nil {
		t.Fatalf("create partial directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "partial", "nested", "state"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write partial file: %v", err)
	}

	state, err := seedDirectoryState(dst)
	if err != nil {
		t.Fatalf("seedDirectoryState: %v", err)
	}
	if state != seedStateInProgress {
		t.Fatalf("seedDirectoryState = %v, want in-progress", state)
	}
	if err := cleanInterruptedSeed(dst); err != nil {
		t.Fatalf("cleanInterruptedSeed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "lost+found")); err != nil {
		t.Fatalf("lost+found should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "partial")); !os.IsNotExist(err) {
		t.Fatalf("partial content should be removed, got err=%v", err)
	}
	if empty, err := directoryEmptyForSeed(dst); err != nil {
		t.Fatalf("directoryEmptyForSeed: %v", err)
	} else if !empty {
		t.Fatal("cleaned interrupted seed should be empty")
	}
}

func TestInvalidSharedDataSeedMarkerPreservesExistingContent(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, sharedDataSeedInProgressMarker), []byte("user content"), 0o600); err != nil {
		t.Fatalf("write marker-shaped user file: %v", err)
	}

	state, err := seedDirectoryState(dst)
	if err != nil {
		t.Fatalf("seedDirectoryState: %v", err)
	}
	if state != seedStateExistingContent {
		t.Fatalf("seedDirectoryState = %v, want existing-content", state)
	}
}

func TestSeedSharedDataPartitionRejectsSymlinkedSourceBeforeMount(t *testing.T) {
	root := t.TempDir()
	realTarget := filepath.Join(root, "real-var")
	if err := os.Mkdir(realTarget, 0o755); err != nil {
		t.Fatalf("create real target: %v", err)
	}
	target := filepath.Join(root, "var")
	if err := os.Symlink(realTarget, target); err != nil {
		t.Fatalf("create symlinked seed source: %v", err)
	}

	oldMountShared := mountSharedDataPart
	mountCalled := false
	mountSharedDataPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		mountCalled = true
		return nil
	}
	t.Cleanup(func() { mountSharedDataPart = oldMountShared })

	err := (&Orchestrator{}).seedSharedDataPartition(context.Background(), "/dev/test", target)
	if err == nil || !strings.Contains(err.Error(), "got symlink") {
		t.Fatalf("seedSharedDataPartition error = %v, want symlink rejection", err)
	}
	if mountCalled {
		t.Fatal("seedSharedDataPartition must reject symlinked source before mounting seed target")
	}
}

func TestSharedDataSeedCopyPreservesSymlinksAndIgnoresLostFound(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.Mkdir(filepath.Join(dst, "lost+found"), 0o700); err != nil {
		t.Fatalf("create lost+found: %v", err)
	}
	empty, err := directoryEmptyForSeed(dst)
	if err != nil {
		t.Fatalf("directoryEmptyForSeed: %v", err)
	}
	if !empty {
		t.Fatal("lost+found-only seed target should be considered empty")
	}
	if err := os.MkdirAll(filepath.Join(src, "lib"), 0o755); err != nil {
		t.Fatalf("create source directory: %v", err)
	}
	srcFile := filepath.Join(src, "lib", "state")
	if err := os.WriteFile(srcFile, []byte("kept"), 0o640); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	if err := os.Symlink("../lib/state", filepath.Join(src, "state-link")); err != nil {
		t.Fatalf("create source symlink: %v", err)
	}
	srcModTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(srcFile, srcModTime, srcModTime); err != nil {
		t.Fatalf("set source file timestamps: %v", err)
	}
	if err := os.Chmod(filepath.Join(src, "lib"), 0o750); err != nil {
		t.Fatalf("set source directory mode: %v", err)
	}
	if err := os.Chtimes(filepath.Join(src, "lib"), srcModTime, srcModTime); err != nil {
		t.Fatalf("set source directory timestamps: %v", err)
	}

	if err := copyTreeWithSymlinks(context.Background(), src, dst); err != nil {
		t.Fatalf("copyTreeWithSymlinks: %v", err)
	}
	link, err := os.Readlink(filepath.Join(dst, "state-link"))
	if err != nil {
		t.Fatalf("read copied symlink: %v", err)
	}
	if link != "../lib/state" {
		t.Fatalf("copied symlink target = %q, want ../lib/state", link)
	}
	data, err := os.ReadFile(filepath.Join(dst, "lib", "state"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "kept" {
		t.Fatalf("copied file = %q", string(data))
	}
	fileInfo, err := os.Stat(filepath.Join(dst, "lib", "state"))
	if err != nil {
		t.Fatalf("stat copied file: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o640 {
		t.Fatalf("copied file mode = %v, want 0640", fileInfo.Mode().Perm())
	}
	if !fileInfo.ModTime().Equal(srcModTime) {
		t.Fatalf("copied file mtime = %v, want %v", fileInfo.ModTime(), srcModTime)
	}
	dirInfo, err := os.Stat(filepath.Join(dst, "lib"))
	if err != nil {
		t.Fatalf("stat copied directory: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o750 {
		t.Fatalf("copied directory mode = %v, want 0750", dirInfo.Mode().Perm())
	}
	if !dirInfo.ModTime().Equal(srcModTime) {
		t.Fatalf("copied directory mtime = %v, want %v", dirInfo.ModTime(), srcModTime)
	}
}

func TestMountRootSkipsAlreadyMountedNewroot(t *testing.T) {
	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return path == newroot }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountedSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		if path == newroot {
			return "/dev/sda2", true
		}
		return "", false
	}
	t.Cleanup(func() { mountedSource = oldMountedSource })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.rootPartition = "/dev/sda2"

	if err := o.mountRoot(context.Background()); err != nil {
		t.Fatalf("mountRoot with mounted newroot: %v", err)
	}
}

func TestMountRootFailsWhenNewrootMountedFromUnexpectedPartition(t *testing.T) {
	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return path == newroot }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountedSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		if path == newroot {
			return "/dev/sdb2", true
		}
		return "", false
	}
	t.Cleanup(func() { mountedSource = oldMountedSource })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.rootPartition = "/dev/sda2"

	err := o.mountRoot(context.Background())
	if err == nil {
		t.Fatal("expected mounted root partition mismatch error")
	}
	if !strings.Contains(err.Error(), "expected root partition /dev/sda2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMountBootSkipsWhenNoBootPartition(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.mountBoot(context.Background()); err != nil {
		t.Fatalf("mountBoot without boot partition: %v", err)
	}
}

func TestMountBootSkipsAlreadyMountedExpectedPartition(t *testing.T) {
	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return path == bootEFIMountPoint() }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountedSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		if path == bootEFIMountPoint() {
			return "/dev/sda1", true
		}
		return "", false
	}
	t.Cleanup(func() { mountedSource = oldMountedSource })
	oldMountBootPart := mountBootPart
	mountBootPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		t.Fatal("already-mounted boot partition must not be mounted again")
		return nil
	}
	t.Cleanup(func() { mountBootPart = oldMountBootPart })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.bootPartition = "/dev/sda1"

	if err := o.mountBoot(context.Background()); err != nil {
		t.Fatalf("mountBoot with mounted ESP: %v", err)
	}
}

func TestMountBootFailsWhenAlreadyMountedFromUnexpectedPartition(t *testing.T) {
	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return path == bootEFIMountPoint() }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountedSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		if path == bootEFIMountPoint() {
			return "/dev/sdb1", true
		}
		return "", false
	}
	t.Cleanup(func() { mountedSource = oldMountedSource })
	oldMountBootPart := mountBootPart
	mountBootPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		t.Fatal("mismatched boot partition must not be mounted")
		return nil
	}
	t.Cleanup(func() { mountBootPart = oldMountBootPart })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.bootPartition = "/dev/sda1"

	err := o.mountBoot(context.Background())
	if err == nil {
		t.Fatal("expected boot mount source mismatch error")
	}
	if !strings.Contains(err.Error(), "expected boot partition /dev/sda1") {
		t.Fatalf("mountBoot error = %q", err.Error())
	}
}

func TestMountBootMountsWhenEFIRuntimeUnavailable(t *testing.T) {
	oldRuntime := efiRuntimeReady
	efiRuntimeReady = func() (bool, string) {
		t.Fatal("mountBoot must not require EFI runtime; only boot-entry operations need efivarfs")
		return false, "unit test"
	}
	t.Cleanup(func() { efiRuntimeReady = oldRuntime })
	oldMountPoint := isMountPoint
	isMountPoint = func(string) bool { return false }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountBootPart := mountBootPart
	mounted := false
	mountBootPart = func(_ context.Context, _ *disk.Manager, device, mountpoint string) error {
		mounted = true
		if device != "/dev/sda1" {
			t.Fatalf("mounted device = %q, want /dev/sda1", device)
		}
		if mountpoint != bootEFIMountPoint() {
			t.Fatalf("mountpoint = %q, want %q", mountpoint, bootEFIMountPoint())
		}
		return nil
	}
	t.Cleanup(func() { mountBootPart = oldMountBootPart })

	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.bootPartition = "/dev/sda1"

	if err := o.mountBoot(context.Background()); err != nil {
		t.Fatalf("mountBoot without EFI runtime: %v", err)
	}
	if !mounted {
		t.Fatal("mountBoot did not mount the ESP")
	}
}

func TestMountBootSkipsUnsupportedBootPartitionWhenEFIRuntimeUnavailable(t *testing.T) {
	oldRuntime := efiRuntimeReady
	efiRuntimeReady = func() (bool, string) { return false, "unit test" }
	t.Cleanup(func() { efiRuntimeReady = oldRuntime })
	oldMountPoint := isMountPoint
	isMountPoint = func(string) bool { return false }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountBootPart := mountBootPart
	mountBootPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		return errors.New("mounting /dev/sda1 at /newroot/boot/efi: tried ext4=invalid argument, vfat=invalid argument")
	}
	t.Cleanup(func() { mountBootPart = oldMountBootPart })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.bootPartition = "/dev/sda1"

	if err := o.mountBoot(context.Background()); err != nil {
		t.Fatalf("mountBoot unsupported partition without EFI runtime: %v", err)
	}
}

func TestMountBootFailsUnsupportedBootPartitionWhenEFIRuntimeReady(t *testing.T) {
	oldRuntime := efiRuntimeReady
	efiRuntimeReady = func() (bool, string) { return true, "" }
	t.Cleanup(func() { efiRuntimeReady = oldRuntime })
	oldMountPoint := isMountPoint
	isMountPoint = func(string) bool { return false }
	t.Cleanup(func() { isMountPoint = oldMountPoint })
	oldMountBootPart := mountBootPart
	mountBootPart = func(_ context.Context, _ *disk.Manager, _, _ string) error {
		return errors.New("mounting /dev/sda1 at /newroot/boot/efi: tried ext4=invalid argument, vfat=invalid argument")
	}
	t.Cleanup(func() { mountBootPart = oldMountBootPart })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.bootPartition = "/dev/sda1"

	if err := o.mountBoot(context.Background()); err == nil {
		t.Fatal("mountBoot succeeded with unsupported boot partition and EFI runtime")
	}
}

func TestBootEFIMountPointUsesMountedRoot(t *testing.T) {
	if got, want := bootEFIMountPoint(), filepath.Join(newroot, "boot", "efi"); got != want {
		t.Fatalf("bootEFIMountPoint = %q, want %q", got, want)
	}
}

func TestInstallEFIFallbackLoaderSkipsNonABMode(t *testing.T) {
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.bootPartition = "/dev/sda1"

	if err := o.installEFIFallbackLoader(context.Background()); err != nil {
		t.Fatalf("installEFIFallbackLoader non-A/B: %v", err)
	}
	if hasCommandName(cmd.calls, "chroot") {
		t.Fatal("fallback installer ran for non-A/B mode")
	}
}

func TestInstallEFIFallbackLoaderSkipsPreserveExistingAB(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.bootPartition = "/dev/sda1"

	if err := o.installEFIFallbackLoader(context.Background()); err != nil {
		t.Fatalf("installEFIFallbackLoader preserveExisting: %v", err)
	}
	if hasCommandName(cmd.calls, "chroot") {
		t.Fatal("fallback installer ran for preserveExisting A/B mode")
	}
}

func TestInstallEFIFallbackLoaderRequiresMountedESP(t *testing.T) {
	oldMountPoint := isMountPoint
	isMountPoint = func(string) bool { return false }
	t.Cleanup(func() { isMountPoint = oldMountPoint })

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.bootPartition = "/dev/sda1"

	err := o.installEFIFallbackLoader(context.Background())
	if err == nil {
		t.Fatal("installEFIFallbackLoader succeeded with unmounted ESP")
	}
	if !strings.Contains(err.Error(), "boot partition /dev/sda1 is not mounted") {
		t.Fatalf("error = %v, want unmounted ESP message", err)
	}
}

func TestInstallEFIFallbackLoaderRunsForInitialABMode(t *testing.T) {
	oldMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return path == bootEFIMountPoint() }
	t.Cleanup(func() { isMountPoint = oldMountPoint })

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.bootPartition = "/dev/sda1"

	if err := o.installEFIFallbackLoader(context.Background()); err != nil {
		t.Fatalf("installEFIFallbackLoader initial A/B: %v", err)
	}
	if !hasCommandName(cmd.calls, "chroot") {
		t.Fatal("fallback installer did not run for initial A/B mode")
	}
}

func TestCreateEFIBootEntrySkipsABMode(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.bootPartition = "/dev/sda1"

	if err := o.createEFIBootEntry(context.Background()); err != nil {
		t.Fatalf("createEFIBootEntry A/B: %v", err)
	}
	if hasCommandName(cmd.calls, "chroot") {
		t.Fatalf("A/B createEFIBootEntry should not chroot, calls=%#v", cmd.calls)
	}
}

func TestSetupNVMeNamespaces_NoOp(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.setupNVMeNamespaces(context.Background()); err != nil {
		t.Fatalf("setupNVMeNamespaces no-op: %v", err)
	}
	if cfg.Provision.Disk.Device != "" {
		t.Fatalf("DiskDevice = %q, want empty", cfg.Provision.Disk.Device)
	}
}

func TestSetupNVMeNamespaces_InvalidJSON(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.NVMeNamespaces = `{bad}`
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.setupNVMeNamespaces(context.Background())
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parsing nvme namespace layout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetupNVMeNamespaces_HappyPathSetsDiskDevice(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.NVMeNamespaces = `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`
	provider := &mockProvider{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, provider)

	cmd.setResult("nvme id-ctrl", []byte(`{"nn":32,"tnvmcap":1024000}`), nil)
	cmd.setResult("nvme create-ns", []byte("create-ns: Success, created nsid:5\n"), nil)

	if err := o.setupNVMeNamespaces(context.Background()); err != nil {
		t.Fatalf("setupNVMeNamespaces: %v", err)
	}
	if cfg.Provision.Disk.Device != "/dev/nvme0n5" {
		t.Fatalf("DiskDevice = %q, want /dev/nvme0n5", cfg.Provision.Disk.Device)
	}
	if o.nvmeTargetDevice != "/dev/nvme0n5" {
		t.Fatalf("nvmeTargetDevice = %q, want /dev/nvme0n5", o.nvmeTargetDevice)
	}
}

func TestCheckpointRecordsNVMeTargetDevice(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.NVMeNamespaces = `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	cmd.setResult("nvme id-ctrl", []byte(`{"nn":32,"tnvmcap":1024000}`), nil)
	cmd.setResult("nvme create-ns", []byte("create-ns: Success, created nsid:5\n"), nil)

	cp := &Checkpoint{}
	step := Step{"setup-nvme-namespaces", o.setupNVMeNamespaces}
	if err := o.executeStep(context.Background(), step, cp); err != nil {
		t.Fatalf("execute setup-nvme-namespaces: %v", err)
	}
	if cp.NVMeTargetDevice != "/dev/nvme0n5" {
		t.Fatalf("checkpoint NVMeTargetDevice = %q, want /dev/nvme0n5", cp.NVMeTargetDevice)
	}
	if !cp.IsCompleted("setup-nvme-namespaces") {
		t.Fatal("checkpoint should mark setup-nvme-namespaces complete")
	}
}

func TestCheckpointResumeRestoresNVMeTargetDevice(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.NVMeNamespaces = `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	cp := &Checkpoint{
		CompletedSteps:   []string{"setup-nvme-namespaces"},
		NVMeTargetDevice: "/dev/nvme0n5",
	}

	o.restoreCheckpointDerivedState(cp)

	if cfg.Provision.Disk.Device != "/dev/nvme0n5" {
		t.Fatalf("DiskDevice = %q, want checkpoint target", cfg.Provision.Disk.Device)
	}
	if o.nvmeTargetDevice != "/dev/nvme0n5" {
		t.Fatalf("nvmeTargetDevice = %q, want checkpoint target", o.nvmeTargetDevice)
	}
}

func TestCheckpointResumeDoesNotOverrideExplicitDiskDevice(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/dev/disk/by-id/operator-selected"
	cfg.Provision.Disk.NVMeNamespaces = `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	cp := &Checkpoint{NVMeTargetDevice: "/dev/nvme0n5"}

	o.restoreCheckpointDerivedState(cp)

	if cfg.Provision.Disk.Device != "/dev/disk/by-id/operator-selected" {
		t.Fatalf("DiskDevice = %q, want explicit config to win", cfg.Provision.Disk.Device)
	}
}

func TestCheckpointResumeDoesNotRerunNVMeNamespaceSetup(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.NVMeNamespaces = `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	cp := &Checkpoint{
		CompletedSteps:   []string{"setup-nvme-namespaces"},
		NVMeTargetDevice: "/dev/nvme0n5",
	}
	o.restoreCheckpointDerivedState(cp)

	stateSteps := resumeStateSteps()
	step := Step{"setup-nvme-namespaces", o.setupNVMeNamespaces}
	_, mustRun := stateSteps[step.Name]
	if cp.IsCompleted(step.Name) && !mustRun {
		if len(cmd.calls) != 0 {
			t.Fatalf("completed nvme setup should skip without nvme commands, got %#v", cmd.calls)
		}
		return
	}
	if err := o.executeStep(context.Background(), step, cp); err != nil {
		t.Fatalf("setup-nvme-namespaces reran unexpectedly: %v", err)
	}
	t.Fatalf("setup-nvme-namespaces must remain skippable on resume; calls=%#v", cmd.calls)
}

func TestCheckpointRecordsRAIDTargetDevice(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, _ := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	cp := &Checkpoint{}
	step := Step{"setup-raid", o.setupRAID}
	if err := o.executeStep(context.Background(), step, cp); err != nil {
		t.Fatalf("execute setup-raid: %v", err)
	}
	if cp.RAIDTargetDevice != "/dev/md0" {
		t.Fatalf("checkpoint RAIDTargetDevice = %q, want /dev/md0", cp.RAIDTargetDevice)
	}
	if !cp.IsCompleted("setup-raid") {
		t.Fatal("checkpoint should mark setup-raid complete")
	}
}

func TestCheckpointResumeRestoresRAIDTargetDevice(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	cp := &Checkpoint{
		CompletedSteps:   []string{"setup-raid"},
		RAIDTargetDevice: "/dev/md0",
	}

	o.restoreCheckpointDerivedState(cp)

	if cfg.Provision.Disk.Device != "/dev/md0" {
		t.Fatalf("DiskDevice = %q, want checkpoint raid target", cfg.Provision.Disk.Device)
	}
	if o.targetDisk != "/dev/md0" {
		t.Fatalf("targetDisk = %q, want checkpoint raid target", o.targetDisk)
	}
}

func TestCheckpointResumeDoesNotRerunRAIDSetup(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	cp := &Checkpoint{
		CompletedSteps:   []string{"setup-raid"},
		RAIDTargetDevice: "/dev/md0",
	}
	o.restoreCheckpointDerivedState(cp)

	stateSteps := resumeStateSteps()
	step := Step{"setup-raid", o.setupRAID}
	_, mustRun := stateSteps[step.Name]
	if cp.IsCompleted(step.Name) && !mustRun {
		if len(cmd.calls) != 0 {
			t.Fatalf("completed raid setup should skip without destructive commands, got %#v", cmd.calls)
		}
		return
	}
	if err := o.executeStep(context.Background(), step, cp); err != nil {
		t.Fatalf("setup-raid reran unexpectedly: %v", err)
	}
	t.Fatalf("setup-raid must remain skippable on resume; calls=%#v", cmd.calls)
}

func TestSetupRAIDCreatesConfiguredArrayAndSetsSingleArrayTarget(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{" /dev/sda ", "/dev/sdb", "/dev/sda"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	if err := o.setupRAID(context.Background()); err != nil {
		t.Fatalf("setupRAID: %v", err)
	}
	if cfg.Provision.Disk.Device != "/dev/md0" {
		t.Fatalf("DiskDevice = %q, want /dev/md0", cfg.Provision.Disk.Device)
	}
	if o.targetDisk != "/dev/md0" {
		t.Fatalf("targetDisk = %q, want /dev/md0", o.targetDisk)
	}
	createArgs := []string{"--create", "/dev/md0", "--level", "1", "--raid-devices", "2", "--run", "--force", "--metadata", "1.2", "/dev/sda", "/dev/sdb"}
	if !hasCommandCall(cmd.calls, "mdadm", createArgs...) {
		t.Fatalf("expected mdadm create for trimmed unique members, got %#v", cmd.calls)
	}
	createIdx := commandIndex(cmd.calls, "mdadm", createArgs...)
	wipeIdx := commandIndex(cmd.calls, "wipefs", "-af", "/dev/sdb")
	if wipeIdx == -1 || createIdx == -1 || wipeIdx >= createIdx {
		t.Fatalf("expected member wipes before mdadm create, calls: %#v", cmd.calls)
	}
	if got := commandIndex(cmd.calls, "wipefs", "-af", "/dev/sda"); got == -1 {
		t.Fatalf("expected /dev/sda member wipe, calls: %#v", cmd.calls)
	}
}

func TestSetupRAIDAllowsNestedMDArrayName(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md/boot",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	if err := o.setupRAID(context.Background()); err != nil {
		t.Fatalf("setupRAID: %v", err)
	}
	if cfg.Provision.Disk.Device != "/dev/md/boot" {
		t.Fatalf("DiskDevice = %q, want /dev/md/boot", cfg.Provision.Disk.Device)
	}
	if o.targetDisk != "/dev/md/boot" {
		t.Fatalf("targetDisk = %q, want /dev/md/boot", o.targetDisk)
	}
	createArgs := []string{"--create", "/dev/md/boot", "--level", "1", "--raid-devices", "2", "--run", "--force", "--metadata", "1.2", "/dev/sda", "/dev/sdb"}
	if !hasCommandCall(cmd.calls, "mdadm", createArgs...) {
		t.Fatalf("expected mdadm create for nested md path, got %#v", cmd.calls)
	}
}

func TestSetupRAIDTreatsNVMeAutoTargetAsUnset(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/dev/nvme0n5"
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/nvme0n1", "/dev/nvme1n1"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.nvmeTargetDevice = "/dev/nvme0n5"

	if err := o.setupRAID(context.Background()); err != nil {
		t.Fatalf("setupRAID: %v", err)
	}
	if cfg.Provision.Disk.Device != "/dev/md0" {
		t.Fatalf("DiskDevice = %q, want /dev/md0", cfg.Provision.Disk.Device)
	}
	if o.targetDisk != "/dev/md0" {
		t.Fatalf("targetDisk = %q, want /dev/md0", o.targetDisk)
	}
	createArgs := []string{"--create", "/dev/md0", "--level", "1", "--raid-devices", "2", "--run", "--force", "--metadata", "1.2", "/dev/nvme0n1", "/dev/nvme1n1"}
	if !hasCommandCall(cmd.calls, "mdadm", createArgs...) {
		t.Fatalf("expected mdadm create after nvme auto-target reset, got %#v", cmd.calls)
	}
}

func TestSetupRAIDRequiresTargetBeforeMultipleArrayWork(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{
		{Name: "md0", Level: 1, Devices: []string{"/dev/sda", "/dev/sdb"}},
		{Name: "md1", Level: 1, Devices: []string{"/dev/sdc", "/dev/sdd"}},
	}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.setupRAID(context.Background())
	if err == nil {
		t.Fatal("expected explicit target device error")
	}
	if !strings.Contains(err.Error(), "provision.disk.device is required") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no destructive calls before target is selected, got %#v", cmd.calls)
	}
}

func TestSetupRAIDRejectsTooFewUniqueMembersBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", " /dev/sda "},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.setupRAID(context.Background())
	if err == nil {
		t.Fatal("expected unique member count error")
	}
	if !strings.Contains(err.Error(), "requires at least 2 unique member devices") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no destructive commands before validation, got %#v", cmd.calls)
	}
}

func TestSetupRAIDRejectsUnsupportedLevelBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   7,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.setupRAID(context.Background())
	if err == nil {
		t.Fatal("expected unsupported raid level error")
	}
	if !strings.Contains(err.Error(), "unsupported level 7") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no destructive commands before validation, got %#v", cmd.calls)
	}
}

func TestSetupRAIDRejectsDuplicateMembersAcrossArraysBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RAID = []config.RAIDConfig{
		{Name: "md0", Level: 1, Devices: []string{"/dev/sda", "/dev/sdb"}},
		{Name: "md1", Level: 1, Devices: []string{"/dev/sdb", "/dev/sdc"}},
	}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.setupRAID(context.Background())
	if err == nil {
		t.Fatal("expected duplicate raid member error")
	}
	if !strings.Contains(err.Error(), "configured in both") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no destructive commands before validation, got %#v", cmd.calls)
	}
}

func TestSetupRAIDRejectsUnsafeArrayNameBeforeWipe(t *testing.T) {
	for _, name := range []string{"md/../sda", "sda", "md/boot/extra", "mdboot"} {
		name := name
		t.Run(name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.Disk.RAID = []config.RAIDConfig{{
				Name:    name,
				Level:   1,
				Devices: []string{"/dev/sda", "/dev/sdb"},
			}}
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

			err := o.setupRAID(context.Background())
			if err == nil {
				t.Fatal("expected invalid raid array name error")
			}
			if !strings.Contains(err.Error(), "invalid raid array name") {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cmd.calls) != 0 {
				t.Fatalf("expected no destructive commands before validation, got %#v", cmd.calls)
			}
		})
	}
}

func TestSetupRAIDRejectsExplicitNonArrayTargetBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/dev/sda"
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.setupRAID(context.Background())
	if err == nil {
		t.Fatal("expected explicit target mismatch error")
	}
	if !strings.Contains(err.Error(), "must match one of the configured raid array devices") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no destructive commands before target validation, got %#v", cmd.calls)
	}
}

func TestProvisionReportsErrorOnStepFailure(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	// Override the pipeline with a single step that always fails.
	testErr := fmt.Errorf("simulated failure")
	steps := []Step{
		{"report-init", o.reportInit},
		{"failing-step", func(_ context.Context) error { return testErr }},
	}

	var gotErr error
	for _, step := range steps {
		if err := step.Fn(context.Background()); err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected error from failing step")
	}
	if !errors.Is(gotErr, testErr) {
		t.Errorf("expected simulated failure, got %v", gotErr)
	}
	// Verify init was still reported before the failure.
	if len(provider.statuses) != 1 || provider.statuses[0].status != config.StatusInit {
		t.Error("expected StatusInit before failure")
	}
}

func TestReportInit(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.reportInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(provider.statuses) != 1 {
		t.Fatalf("expected 1 status report, got %d", len(provider.statuses))
	}
	if provider.statuses[0].status != config.StatusInit {
		t.Errorf("expected StatusInit, got %v", provider.statuses[0].status)
	}
}

func TestReportSuccess(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.reportSuccess(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(provider.statuses) != 1 {
		t.Fatalf("expected 1 status report, got %d", len(provider.statuses))
	}
	if provider.statuses[0].status != config.StatusSuccess {
		t.Errorf("expected StatusSuccess, got %v", provider.statuses[0].status)
	}
	if provider.statuses[0].message != "provisioning complete" {
		t.Errorf("expected default success message, got %q", provider.statuses[0].message)
	}
}

func TestReportSuccessSignalsSecureBootReEnable(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.SecureBoot.ReEnable = true
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.reportSuccess(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(provider.statuses) != 1 {
		t.Fatalf("expected 1 status report, got %d", len(provider.statuses))
	}
	if provider.statuses[0].status != config.StatusSuccess {
		t.Errorf("expected StatusSuccess, got %v", provider.statuses[0].status)
	}
	if !strings.Contains(provider.statuses[0].message, "SECUREBOOT_REENABLE=true") {
		t.Errorf("expected Secure Boot re-enable signal, got %q", provider.statuses[0].message)
	}
}

func TestReportSuccessStatusFailureRetries(t *testing.T) {
	origPolicy := DefaultPolicies["report-success"]
	defer func() { DefaultPolicies["report-success"] = origPolicy }()
	DefaultPolicies["report-success"] = RetryPolicy{MaxRetries: 2, InitialDelay: 0, MaxDelay: 0, Transient: true}

	cfg := &config.MachineConfig{}
	provider := &mockProvider{reportStatusErr: errors.New("status endpoint unavailable")}
	o := newTestOrchestrator(t, cfg, provider)
	cp := &Checkpoint{}

	err := o.executeStep(context.Background(), Step{"report-success", o.reportSuccess}, cp)
	if err == nil {
		t.Fatal("expected report-success to fail after retries exhausted")
	}
	if cp.IsCompleted("report-success") {
		t.Fatal("report-success step should not be marked complete on failure")
	}
	// Initial attempt + 2 retries = 3 success attempts.
	// Step failure triggers 1 final StatusError report. Total = 4.
	if len(provider.statuses) != 4 {
		t.Fatalf("expected 4 status reports (3 success retries + 1 error report), got %d", len(provider.statuses))
	}
	for i := 0; i < 3; i++ {
		if provider.statuses[i].status != config.StatusSuccess {
			t.Fatalf("attempt %d: status = %s, want %s", i+1, provider.statuses[i].status, config.StatusSuccess)
		}
	}
	if provider.statuses[3].status != config.StatusError {
		t.Fatalf("final error report: status = %s, want %s", provider.statuses[3].status, config.StatusError)
	}
}

func TestReportSuccessStatusTransientFailureSucceeds(t *testing.T) {
	origPolicy := DefaultPolicies["report-success"]
	defer func() { DefaultPolicies["report-success"] = origPolicy }()
	DefaultPolicies["report-success"] = RetryPolicy{MaxRetries: 2, InitialDelay: 0, MaxDelay: 0, Transient: true}

	cfg := &config.MachineConfig{}
	provider := &mockProvider{
		reportStatusErrs: []error{
			errors.New("transient error 1"),
			errors.New("transient error 2"),
		},
		reportStatusErr: nil, // 3rd attempt succeeds
	}
	o := newTestOrchestrator(t, cfg, provider)
	cp := &Checkpoint{}

	err := o.executeStep(context.Background(), Step{"report-success", o.reportSuccess}, cp)
	if err != nil {
		t.Fatalf("expected report-success to succeed after retries, got error: %v", err)
	}
	if !cp.IsCompleted("report-success") {
		t.Fatal("report-success step should be marked complete")
	}
	// Initial + 2 retries (last one succeeds) = 3 attempts. No StatusError.
	if len(provider.statuses) != 3 {
		t.Fatalf("expected 3 success report attempts, got %d", len(provider.statuses))
	}
	for i := 0; i < 3; i++ {
		if provider.statuses[i].status != config.StatusSuccess {
			t.Fatalf("attempt %d: status = %s, want %s", i+1, provider.statuses[i].status, config.StatusSuccess)
		}
	}
}

func TestWipeOrSecureEraseDisks(t *testing.T) {
	tests := []struct {
		name        string
		secureErase bool
		wipeErr     error
		wantErr     bool
	}{
		{
			name:        "quick erase (default)",
			secureErase: false,
		},
		{
			name:        "secure erase enabled",
			secureErase: true,
			// NOTE: SecureEraseAllDisks reads /sys/block which is empty in test,
			// so this only verifies the function is called without panic.
			// Full coverage requires integration tests with real or mock disks.
		},
		{
			name:        "quick erase error",
			secureErase: false,
			wipeErr:     fmt.Errorf("wipe failed"),
			wantErr:     true, // WipeAllDisks returns error when all disk wipes fail
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{Mode: "deprovision"}
			cfg.Provision.Disk.SecureErase = tc.secureErase
			cmd := newMockCommander()
			if tc.wipeErr != nil {
				cmd.setResult("wipefs -af", nil, tc.wipeErr)
			}
			provider := &mockProvider{}
			mgr := disk.NewManager(cmd)
			cfg.Provision.Image.AllowInsecureHTTP = true
			cfg.Provision.Image.AllowInsecureHTTP = true
			o := NewOrchestrator(cfg, provider, mgr)

			err := o.wipeOrSecureEraseDisks(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v, got err=%v", tc.wantErr, err)
			}
		})
	}
}

func TestDeprovisionHardScopesWipeToProvisionDiskDevice(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "deprovision"}
	cfg.Provision.Disk.Device = "/dev/sda"
	provider := &mockProvider{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, provider)
	withMockBlockDevices(t, "/dev/sda")

	if err := o.selectDeprovisionDisk(context.Background()); err != nil {
		t.Fatalf("selectDeprovisionDisk: %v", err)
	}
	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}

	wipes := wipeCommandCalls(cmd.calls)
	if len(wipes) != 2 {
		t.Fatalf("expected exactly 2 target wipe calls, got %#v", wipes)
	}
	if !hasCommandCall(wipes, "sgdisk", "--zap-all", "/dev/sda") {
		t.Fatalf("expected sgdisk to target /dev/sda, got %#v", wipes)
	}
	if !hasCommandCall(wipes, "wipefs", "-af", "/dev/sda") {
		t.Fatalf("expected wipefs to target /dev/sda, got %#v", wipes)
	}
	if o.targetDisk != "/dev/sda" {
		t.Fatalf("targetDisk = %q, want /dev/sda", o.targetDisk)
	}
}

func TestDeprovisionHardPrefersDeprovisionDevice(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "deprovision"}
	cfg.Provision.Disk.Device = "/dev/sda"
	cfg.Deprovision.Device = "/dev/sdb"
	provider := &mockProvider{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, provider)
	withMockBlockDevices(t, "/dev/sda", "/dev/sdb")

	if err := o.selectDeprovisionDisk(context.Background()); err != nil {
		t.Fatalf("selectDeprovisionDisk: %v", err)
	}
	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}

	wipes := wipeCommandCalls(cmd.calls)
	if !hasCommandCall(wipes, "sgdisk", "--zap-all", "/dev/sdb") {
		t.Fatalf("expected sgdisk to target /dev/sdb, got %#v", wipes)
	}
	if !hasCommandCall(wipes, "wipefs", "-af", "/dev/sdb") {
		t.Fatalf("expected wipefs to target /dev/sdb, got %#v", wipes)
	}
	if hasCommandCall(wipes, "sgdisk", "--zap-all", "/dev/sda") ||
		hasCommandCall(wipes, "wipefs", "-af", "/dev/sda") {
		t.Fatalf("deprovision.device should override provision disk, got %#v", wipes)
	}
}

func TestDeprovisionHardScopesSecureEraseToConfiguredDevice(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "deprovision"}
	cfg.Deprovision.Device = "/dev/sdb"
	cfg.Deprovision.SecureErase = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	withMockBlockDevices(t, "/dev/sdb")

	if err := o.selectDeprovisionDisk(context.Background()); err != nil {
		t.Fatalf("selectDeprovisionDisk: %v", err)
	}
	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}

	if !hasCommandCall(cmd.calls, "hdparm", "-I", "/dev/sdb") {
		t.Fatalf("expected secure erase probe to target /dev/sdb, got %#v", cmd.calls)
	}
	if !hasCommandCall(cmd.calls, "wipefs", "-af", "/dev/sdb") {
		t.Fatalf("expected secure erase fallback to target /dev/sdb, got %#v", cmd.calls)
	}
	if hasCommandCall(cmd.calls, "hdparm", "-I", "/dev/sda") ||
		hasCommandCall(cmd.calls, "wipefs", "-af", "/dev/sda") {
		t.Fatalf("secure erase should not target provision disk, got %#v", cmd.calls)
	}
}

func TestDeprovisionHardRejectsInvalidConfiguredDevice(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "deprovision"}
	cfg.Provision.Disk.Device = "sda"
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.selectDeprovisionDisk(context.Background())
	if err == nil {
		t.Fatal("expected invalid device error")
	}
	if !strings.Contains(err.Error(), "must be an absolute /dev/ path") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wipeCommandCalls(cmd.calls)) != 0 {
		t.Fatalf("expected no wipe commands for invalid device, got %#v", cmd.calls)
	}
}

func TestDeprovisionHardRejectsCharacterConfiguredDevice(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "deprovision"}
	cfg.Provision.Disk.Device = "/dev/null"
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	withMockStat(t, func(path string) (os.FileInfo, error) {
		if path == "/dev/null" {
			return fakeFileInfo{name: "null", mode: os.ModeDevice | os.ModeCharDevice}, nil
		}
		return nil, os.ErrNotExist
	})

	err := o.selectDeprovisionDisk(context.Background())
	if err == nil {
		t.Fatal("expected character device error")
	}
	if !strings.Contains(err.Error(), "not a block device") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wipeCommandCalls(cmd.calls)) != 0 {
		t.Fatalf("expected no wipe commands for character device, got %#v", cmd.calls)
	}
}

func withMockBlockDevices(t *testing.T, devices ...string) {
	t.Helper()
	blocks := make(map[string]struct{}, len(devices))
	for _, device := range devices {
		blocks[device] = struct{}{}
	}
	withMockStat(t, func(path string) (os.FileInfo, error) {
		if _, ok := blocks[path]; ok {
			return fakeFileInfo{name: filepath.Base(path), mode: os.ModeDevice}, nil
		}
		return nil, os.ErrNotExist
	})
}

func TestWipeOrSecureEraseDisksRequiresTargetDiskInProvisionMode(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		secureErase bool
	}{
		{name: "provision quick erase", mode: "provision"},
		{name: "dry-run quick erase", mode: "dry-run"},
		{name: "provision secure erase", mode: "provision", secureErase: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{Mode: tc.mode}
			cfg.Provision.Disk.SecureErase = tc.secureErase
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

			err := o.wipeOrSecureEraseDisks(context.Background())
			if err == nil {
				t.Fatal("expected error when targetDisk is empty")
			}
			if !strings.Contains(err.Error(), "target disk is required") {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cmd.calls) != 0 {
				t.Fatalf("expected no wipe commands, got %#v", cmd.calls)
			}
		})
	}
}

func TestWipeOrSecureEraseDisksScopesQuickEraseToTargetDisk(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}

	if got := len(cmd.calls); got != 2 {
		t.Fatalf("expected 2 target wipe calls, got %d: %#v", got, cmd.calls)
	}
	if !hasCommandCall(cmd.calls, "sgdisk", "--zap-all", "/dev/sda") {
		t.Fatalf("expected sgdisk to target /dev/sda, got %#v", cmd.calls)
	}
	if !hasCommandCall(cmd.calls, "wipefs", "-af", "/dev/sda") {
		t.Fatalf("expected wipefs to target /dev/sda, got %#v", cmd.calls)
	}
}

func TestWipeOrSecureEraseDisksScopesSecureEraseToTargetDisk(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SecureErase = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}

	if !hasCommandCall(cmd.calls, "hdparm", "-I", "/dev/sda") {
		t.Fatalf("expected secure erase probe to target /dev/sda, got %#v", cmd.calls)
	}
	if !hasCommandCall(cmd.calls, "wipefs", "-af", "/dev/sda") {
		t.Fatalf("expected secure erase fallback to target /dev/sda, got %#v", cmd.calls)
	}
}

func TestWipeOrSecureEraseDisksWipesRAIDTargetWithoutSecureErase(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SecureErase = true
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/md0"

	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}
	if hasCommandName(cmd.calls, "hdparm") || hasCommandName(cmd.calls, "nvme") {
		t.Fatalf("raid target should not be hardware secure-erased, got %#v", cmd.calls)
	}
	if !hasCommandCall(cmd.calls, "sgdisk", "--zap-all", "/dev/md0") ||
		!hasCommandCall(cmd.calls, "wipefs", "-af", "/dev/md0") {
		t.Fatalf("expected normal wipe of raid target device, got %#v", cmd.calls)
	}
}

func TestWipeOrSecureEraseDisksKeepsSecureEraseForNonRAIDTarget(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SecureErase = true
	cfg.Provision.Disk.RAID = []config.RAIDConfig{{
		Name:    "md0",
		Level:   1,
		Devices: []string{"/dev/sda", "/dev/sdb"},
	}}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}
	if !hasCommandCall(cmd.calls, "hdparm", "-I", "/dev/sda") {
		t.Fatalf("expected secure erase probe for non-raid target, got %#v", cmd.calls)
	}
}

func TestWipeOrSecureEraseDisksAllowsPartitionLayoutWithImageURLsInDeprovisionMode(t *testing.T) {
	cfg := &config.MachineConfig{
		Mode: "deprovision",
	}
	cfg.Provision.Image.URLs = []string{"https://images.local/node.img.zst"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.wipeOrSecureEraseDisks(context.Background())
	if err != nil {
		t.Fatalf("expected no error in deprovision mode, got: %v", err)
	}
}

func TestWipeOrSecureEraseDisksAllowsUnsupportedPartitionLayoutMountpointsInDeprovisionMode(t *testing.T) {
	cfg := &config.MachineConfig{
		Mode: "hard",
	}
	cfg.Provision.Image.URLs = []string{"https://images.local/node.img.zst"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Filesystem: "ext4", Mountpoint: "/"},
			{Label: "var", Filesystem: "ext4", Mountpoint: "/var"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.wipeOrSecureEraseDisks(context.Background())
	if err != nil {
		t.Fatalf("expected deprovision wipe to ignore provisioning-only mountpoint support, got: %v", err)
	}
}

func TestWipeOrSecureEraseDisksAllowsPartitionLayoutWithImageURLsInProvisionMode(t *testing.T) {
	cfg := &config.MachineConfig{
		Mode: "provision",
	}
	cfg.Provision.Image.URLs = []string{"https://images.local/node.img.zst"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	err := o.wipeOrSecureEraseDisks(context.Background())
	if err != nil {
		t.Fatalf("expected partition layout to be allowed in provision mode, got: %v", err)
	}
	if !hasCommandCall(cmd.calls, "wipefs", "-af", "/dev/sda") {
		t.Fatalf("expected wipefs to target /dev/sda, got %#v", cmd.calls)
	}
}

func TestValidateProvisionInputsRejectsPartitionLayoutWithoutImageURLsInProvisionMode(t *testing.T) {
	cfg := &config.MachineConfig{
		Mode: "provision",
	}
	cfg.Provision.TargetOS = config.TargetOSLinux
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.validateProvisionInputs(context.Background())
	if err == nil {
		t.Fatal("expected error when partition layout is set without image urls in provision mode")
	}
	if !strings.Contains(err.Error(), "no image URLs configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWipeOrSecureEraseDisksRejectsLVMLayoutBeforeWipeWhenToolMissing(t *testing.T) {
	cfg := &config.MachineConfig{
		Mode: "provision",
	}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "BOOTY-PV", SizeMB: 1024},
		},
		LVM: &config.LVMConfig{
			VolumeGroup: "sysvg",
			PVPartition: 1,
			Volumes: []config.LVVolume{
				{Name: "root", Filesystem: "ext4", Mountpoint: "/", Extents: "100%FREE"},
			},
		},
	}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("lvm version", nil, fmt.Errorf("exec lvm: %w", exec.ErrNotFound))

	err := o.wipeOrSecureEraseDisks(context.Background())
	if err == nil {
		t.Fatal("expected missing lvm error")
	}
	if !strings.Contains(err.Error(), "lvm tooling required by partition layout") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wipeCommandCalls(cmd.calls)) != 0 {
		t.Fatalf("expected no wipe commands before lvm preflight succeeds, got %#v", cmd.calls)
	}
}

func TestWipeOrSecureEraseDisksRejectsConflictingDeviceOverrides(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/dev/sda"
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Device: "/dev/sdb",
		Table:  "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.wipeOrSecureEraseDisks(context.Background())
	if err == nil {
		t.Fatal("expected error for conflicting disk device overrides")
	}
	if !strings.Contains(err.Error(), "disk device conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectInventoryDisabled(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.collectInventory(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectFirmwareDisabled(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.collectFirmware(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectFirmwareMinimumFailureAbortsAfterReport(t *testing.T) {
	previousCollect := collectFirmwareFn
	collectFirmwareFn = func() (*firmware.Report, error) {
		return &firmware.Report{
			BIOS: firmware.Version{Component: "BIOS", Version: "U30"},
		}, nil
	}
	t.Cleanup(func() {
		collectFirmwareFn = previousCollect
	})

	cfg := &config.MachineConfig{}
	cfg.Provision.Firmware.Enabled = true
	cfg.Provision.Firmware.MinBIOS = "U46"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.collectFirmware(context.Background())
	if err == nil {
		t.Fatal("expected firmware validation failure")
	}
	if !strings.Contains(err.Error(), "firmware validation failed") ||
		!strings.Contains(err.Error(), "firmware-bios") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(provider.firmwareReports) != 1 {
		t.Fatalf("firmware reports = %d, want 1", len(provider.firmwareReports))
	}
}

func TestOrchestratorSetHostnameEmpty(t *testing.T) {
	cfg := &config.MachineConfig{Hostname: ""}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.setHostname(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthChecksDisabled(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.runHealthChecks(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPostProvisionCmdsEmpty(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.runPostProvisionCmds(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFirmwareChanged(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if o.FirmwareChanged() {
		t.Error("expected no firmware change initially")
	}
	o.firmwareChanged = true
	if !o.FirmwareChanged() {
		t.Error("expected firmware change after setting flag")
	}
}

func TestCheckpointResume_SkipsCompleted(t *testing.T) {
	// Steps: first two are marked done in checkpoint; only the third should run.
	dir := t.TempDir()
	cpPath := dir + "/checkpoint.json"

	// Pre-create a checkpoint with the first two steps completed.
	cp := &Checkpoint{
		CompletedSteps: []string{"step-one", "step-two"},
		persist:        true,
		path:           cpPath,
	}
	if err := cp.Save(); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	loadedCP, err := LoadCheckpointFrom(cpPath)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}

	var ran []string
	steps := []Step{
		{"step-one", func(_ context.Context) error { ran = append(ran, "step-one"); return nil }},
		{"step-two", func(_ context.Context) error { ran = append(ran, "step-two"); return nil }},
		{"step-three", func(_ context.Context) error { ran = append(ran, "step-three"); return nil }},
	}

	stateSteps := map[string]struct{}{}
	for _, step := range steps {
		_, mustRun := stateSteps[step.Name]
		if loadedCP.IsCompleted(step.Name) && !mustRun {
			continue
		}
		if err := step.Fn(context.Background()); err != nil {
			t.Fatalf("step %s failed: %v", step.Name, err)
		}
	}

	if len(ran) != 1 || ran[0] != "step-three" {
		t.Errorf("expected only step-three to run on resume, got %v", ran)
	}
}

func TestCheckpointResume_StateStepsAlwaysRun(t *testing.T) {
	// stateSteps must re-run even if marked complete because they rebuild
	// runtime in-memory state and mount state after a restart.
	dir := t.TempDir()
	cpPath := dir + "/checkpoint.json"

	cp := &Checkpoint{
		CompletedSteps: []string{
			"validate-provision-inputs",
			"verify-image",
			"mount-efivarfs",
			"setup-mellanox",
			"setup-raid",
			"detect-disk",
			"parse-partitions",
			"enable-lvm",
			"mount-root",
			"mount-boot",
			"mount-shared-data",
			"setup-chroot-binds",
			"teardown-chroot",
			"stream-image",
			"configure-ssh",
		},
		persist: true,
		path:    cpPath,
	}
	if err := cp.Save(); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	loadedCP, err := LoadCheckpointFrom(cpPath)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}

	stateSteps := resumeStateSteps()

	var ran []string
	steps := []Step{
		{"validate-provision-inputs", func(_ context.Context) error {
			ran = append(ran, "validate-provision-inputs")
			return nil
		}},
		{"verify-image", func(_ context.Context) error { ran = append(ran, "verify-image"); return nil }},
		{"mount-efivarfs", func(_ context.Context) error { ran = append(ran, "mount-efivarfs"); return nil }},
		{"setup-mellanox", func(_ context.Context) error { ran = append(ran, "setup-mellanox"); return nil }},
		{"setup-raid", func(_ context.Context) error { ran = append(ran, "setup-raid"); return nil }},
		{"detect-disk", func(_ context.Context) error { ran = append(ran, "detect-disk"); return nil }},
		{"parse-partitions", func(_ context.Context) error { ran = append(ran, "parse-partitions"); return nil }},
		{"enable-lvm", func(_ context.Context) error { ran = append(ran, "enable-lvm"); return nil }},
		{"mount-root", func(_ context.Context) error { ran = append(ran, "mount-root"); return nil }},
		{"mount-boot", func(_ context.Context) error { ran = append(ran, "mount-boot"); return nil }},
		{"mount-shared-data", func(_ context.Context) error { ran = append(ran, "mount-shared-data"); return nil }},
		{"setup-chroot-binds", func(_ context.Context) error { ran = append(ran, "setup-chroot-binds"); return nil }},
		{"teardown-chroot", func(_ context.Context) error { ran = append(ran, "teardown-chroot"); return nil }},
		{"stream-image", func(_ context.Context) error { ran = append(ran, "stream-image"); return nil }},
		{"configure-ssh", func(_ context.Context) error { ran = append(ran, "configure-ssh"); return nil }},
	}

	for _, step := range steps {
		_, mustRun := stateSteps[step.Name]
		if loadedCP.IsCompleted(step.Name) && !mustRun {
			continue
		}
		if err := step.Fn(context.Background()); err != nil {
			t.Fatalf("step %s failed: %v", step.Name, err)
		}
	}

	// stateSteps re-run; stream-image and configure-ssh skip because they are
	// completed non-state steps.
	if len(ran) != 12 {
		t.Errorf("expected 12 state step runs, got %v", ran)
	}
	for _, name := range []string{
		"validate-provision-inputs",
		"verify-image",
		"mount-efivarfs",
		"setup-mellanox",
		"detect-disk",
		"parse-partitions",
		"enable-lvm",
		"mount-root",
		"mount-boot",
		"mount-shared-data",
		"setup-chroot-binds",
		"teardown-chroot",
	} {
		found := false
		for _, r := range ran {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s to re-run on resume", name)
		}
	}
}

func TestCheckpoint_FailureCountIncrements(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	cp := &Checkpoint{}
	testErr := fmt.Errorf("simulated transient failure")
	step := Step{"failing-step", func(_ context.Context) error { return testErr }}

	_ = o.executeStep(context.Background(), step, cp)

	if cp.FailureCount != 1 {
		t.Errorf("expected FailureCount=1, got %d", cp.FailureCount)
	}
	if len(cp.Errors) != 1 {
		t.Errorf("expected 1 error recorded, got %d", len(cp.Errors))
	}
}

func TestLoadOrCreateCheckpoint(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		wantPersist bool
	}{
		{name: "unset env returns non-persistent", envValue: "", wantPersist: false},
		{name: "true enables persistence", envValue: "true", wantPersist: true},
		{name: "1 enables persistence", envValue: "1", wantPersist: true},
		{name: "false disables persistence", envValue: "false", wantPersist: false},
		{name: "0 disables persistence", envValue: "0", wantPersist: false},
		{name: "random string disables persistence", envValue: "notabool", wantPersist: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			provider := &mockProvider{}
			o := newTestOrchestrator(t, cfg, provider)

			if tc.envValue != "" {
				t.Setenv("BOOTY_RESUME", tc.envValue)
			} else {
				t.Setenv("BOOTY_RESUME", "")
			}
			previous := loadCheckpointFn
			loadCheckpointFn = func() (*Checkpoint, error) {
				return nil, ErrNoCheckpoint
			}
			t.Cleanup(func() {
				loadCheckpointFn = previous
			})

			cp, err := o.loadOrCreateCheckpoint()
			if err != nil {
				t.Fatalf("loadOrCreateCheckpoint: %v", err)
			}
			if cp == nil {
				t.Fatal("expected non-nil checkpoint")
				return
			}
			if cp.persist != tc.wantPersist {
				t.Errorf("persist = %v, want %v", cp.persist, tc.wantPersist)
			}
		})
	}
}

func TestLoadOrCreateCheckpointResumeFailsClosedOnLoadError(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	t.Setenv("BOOTY_RESUME", "true")

	loadErr := errors.New("unmarshal checkpoint: invalid character")
	previous := loadCheckpointFn
	loadCheckpointFn = func() (*Checkpoint, error) {
		return nil, loadErr
	}
	t.Cleanup(func() {
		loadCheckpointFn = previous
	})

	cp, err := o.loadOrCreateCheckpoint()
	if err == nil {
		t.Fatal("expected checkpoint load error")
	}
	if cp != nil {
		t.Fatal("expected nil checkpoint on load error")
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("expected wrapped load error, got %v", err)
	}
}

func TestLoadOrCreateCheckpointResumeStartsFreshWhenMissing(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	t.Setenv("BOOTY_RESUME", "true")

	previous := loadCheckpointFn
	loadCheckpointFn = func() (*Checkpoint, error) {
		return nil, ErrNoCheckpoint
	}
	t.Cleanup(func() {
		loadCheckpointFn = previous
	})

	cp, err := o.loadOrCreateCheckpoint()
	if err != nil {
		t.Fatalf("loadOrCreateCheckpoint: %v", err)
	}
	if cp == nil {
		t.Fatal("expected fresh checkpoint")
	}
	if !cp.persist {
		t.Fatal("expected fresh resume checkpoint to persist")
	}
}

func TestExecuteStepReturnsCheckpointSaveErrorAfterSuccess(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	dir := t.TempDir()
	cpPath := filepath.Join(dir, "checkpoint-dir")
	if err := os.Mkdir(cpPath, 0o755); err != nil {
		t.Fatalf("mkdir checkpoint path: %v", err)
	}

	cp := &Checkpoint{path: cpPath, persist: true}
	step := Step{"successful-step", func(_ context.Context) error { return nil }}

	err := o.executeStep(context.Background(), step, cp)
	if err == nil {
		t.Fatal("expected checkpoint save error")
	}
	if !strings.Contains(err.Error(), "save checkpoint after step successful-step") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRescueConfig_WiresAllFields(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Rescue.Mode = "shell"
	cfg.Rescue.SSHPubKey = "ssh-ed25519 AAAA..."
	cfg.Rescue.PasswordHash = "$6$rounds=4096$salt$hash"
	cfg.Rescue.Timeout = 120
	cfg.Rescue.AutoMountDisks = true
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	rc := o.RescueConfig()

	if rc.Mode != "shell" {
		t.Errorf("Mode = %q, want shell", rc.Mode)
	}
	if len(rc.SSHKeys) != 1 || rc.SSHKeys[0] != "ssh-ed25519 AAAA..." {
		t.Errorf("SSHKeys = %v, want [ssh-ed25519 AAAA...]", rc.SSHKeys)
	}
	if rc.PasswordHash != "$6$rounds=4096$salt$hash" {
		t.Errorf("PasswordHash = %q", rc.PasswordHash)
	}
	if rc.ShellTimeout.Seconds() != 120 {
		t.Errorf("ShellTimeout = %v, want 2m", rc.ShellTimeout)
	}
	if !rc.AutoMountDisks {
		t.Error("AutoMountDisks should be true")
	}
}

func TestRescueConfig_DefaultsApplied(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	rc := o.RescueConfig()

	if rc.Mode != "reboot" {
		t.Errorf("Mode = %q, want reboot", rc.Mode)
	}
	if rc.RetryDelay == 0 {
		t.Error("RetryDelay should have a default")
	}
	if rc.ShellTimeout == 0 {
		t.Error("ShellTimeout should have a default")
	}
}

func TestVerifyImageSignature_Skipped(t *testing.T) {
	cfg := &config.MachineConfig{}
	// Give it a valid URL so SelectBestSource doesn't fail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg.Provision.Image.URLs = []string{srv.URL}
	cfg.Provision.Image.AllowInsecureHTTP = true
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	// No signature URL → should skip without error.
	if err := o.verifyImageSignature(context.Background()); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestVerifyImageSignature_FailsOnUnreachableSource(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.SignatureURL = ""
	cfg.Provision.Image.URLs = []string{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.verifyImageSignature(context.Background())
	if err == nil {
		t.Fatal("expected error from source resolution failure, but got nil")
	}
	if !strings.Contains(err.Error(), "selecting image source") {
		t.Fatalf("expected 'selecting image source' error, got: %v", err)
	}
}

func TestVerifyImageSignature_MissingPubKey(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.SignatureURL = "https://example.com/image.sig"
	cfg.Provision.Image.GPGPubKey = ""
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.verifyImageSignature(context.Background())
	if err == nil {
		t.Error("expected error for missing pub key")
	}
}

func TestVerifyImageRejectsMultiLayerOCIBeforeWipe(t *testing.T) {
	srv := startDryRunOCIRegistry(t)
	defer srv.Close()
	ref := pushDryRunMultiLayerOCIImage(t, srv, "test/multi-layer-os:v1", "layer-1", "layer-2")

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"oci://" + ref}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.verifyImageSignature(context.Background())
	if err == nil {
		t.Fatal("expected multi-layer OCI image rejection")
	}
	if !strings.Contains(err.Error(), "expected exactly one payload layer") {
		t.Fatalf("error = %q, want exactly one payload layer", err.Error())
	}
	if o.bestImageURL != "oci://"+ref {
		t.Fatalf("bestImageURL = %q, want selected OCI ref", o.bestImageURL)
	}
}

func TestVerifyImageSignatureOCIProbeUsesTimeout(t *testing.T) {
	previousProbe := probeOCIReference
	t.Cleanup(func() {
		probeOCIReference = previousProbe
	})

	const ref = "registry.example.invalid/team/node:latest"
	var called bool
	probeOCIReference = func(ctx context.Context, gotRef string) error {
		called = true
		if gotRef != ref {
			t.Fatalf("probe ref = %q, want %q", gotRef, ref)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("probe context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > ociPreflightProbeTimeout {
			t.Fatalf("probe deadline remaining = %v, want within %v", remaining, ociPreflightProbeTimeout)
		}
		return errors.New("stop probe")
	}

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"oci://" + ref}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err := o.verifyImageSignature(context.Background())
	if err == nil {
		t.Fatal("expected probe error")
	}
	if !strings.Contains(err.Error(), "probing OCI image source") {
		t.Fatalf("error = %q, want OCI probe context", err.Error())
	}
	if !called {
		t.Fatal("probe was not called")
	}
}

func TestVerifyImageSignatureUsesCachedBestImageURL(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"https://127.0.0.1:1/unreachable.raw"}
	cfg.Provision.Image.SignatureURL = "https://example.com/image.sig"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.bestImageURL = "https://images.example.invalid/node.raw"

	err := o.verifyImageSignature(context.Background())
	if err == nil {
		t.Fatal("expected missing pub key error")
	}
	if !strings.Contains(err.Error(), "no GPG public key") {
		t.Fatalf("error = %q, want cached URL path to reach pubkey validation", err.Error())
	}
	if o.bestImageURL != "https://images.example.invalid/node.raw" {
		t.Fatalf("bestImageURL = %q, want cached URL preserved", o.bestImageURL)
	}
}

func TestDryRunImageMode(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		status DryRunStatus
	}{
		{"default empty", "", DryRunPass},
		{"whole-disk", "whole-disk", DryRunPass},
		{"partition", "partition", DryRunPass},
		{"PARTITION caps", "PARTITION", DryRunPass},
		{"ab", "ab", DryRunPass},
		{"invalid", "invalid-mode", DryRunFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.Image.Mode = tt.mode
			provider := &mockProvider{}
			o := newTestOrchestrator(t, cfg, provider)
			result := o.dryRunImageMode(context.Background())
			if result.Status != tt.status {
				t.Errorf("dryRunImageMode(%q) status = %s, want %s", tt.mode, result.Status, tt.status)
			}
		})
	}
}

func TestEnsureABPartitionLayoutTargetsInactiveSlot(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.RootSizeMB = 8192
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	if err := o.ensureABPartitionLayout(); err != nil {
		t.Fatalf("ensureABPartitionLayout: %v", err)
	}
	layout := cfg.Provision.Disk.PartitionLayout
	if layout == nil {
		t.Fatal("expected partition layout")
		return
	}
	if layout.Partitions[1].Mountpoint != "" {
		t.Fatalf("slot A mountpoint = %q, want empty", layout.Partitions[1].Mountpoint)
	}
	if layout.Partitions[2].Mountpoint != "/" {
		t.Fatalf("slot B mountpoint = %q, want /", layout.Partitions[2].Mountpoint)
	}
}

func TestParsePartitionsEnsuresABLayoutOnResume(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.RootSizeMB = 8192
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	if err := o.parsePartitions(context.Background()); err != nil {
		t.Fatalf("parsePartitions: %v", err)
	}
	if cfg.Provision.Disk.PartitionLayout == nil {
		t.Fatal("expected generated A/B partition layout")
	}
	if o.bootPartition != "/dev/sda1" {
		t.Fatalf("bootPartition = %q, want /dev/sda1", o.bootPartition)
	}
	if o.rootPartition != "/dev/sda3" {
		t.Fatalf("rootPartition = %q, want inactive slot /dev/sda3", o.rootPartition)
	}
}

func TestParsePartitionsUsesConfiguredRootPartitionNumber(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RootPartitionNumber = 2
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "EFI"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "ubuntu-root"},
		{Node: "/dev/sda3", Type: disk.LinuxFilesystemGUID, Name: "state"},
	}), nil)

	if err := o.parsePartitions(context.Background()); err != nil {
		t.Fatalf("parsePartitions: %v", err)
	}
	if o.rootPartition != "/dev/sda2" {
		t.Fatalf("rootPartition = %q, want selected root /dev/sda2", o.rootPartition)
	}
}

func TestParsePartitionsUsesConfiguredRootPartitionLabel(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RootPartitionLabel = "ubuntu-root"
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "EFI"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "ubuntu-root"},
		{Node: "/dev/sda3", Type: disk.LinuxFilesystemGUID, Name: "state"},
	}), nil)

	if err := o.parsePartitions(context.Background()); err != nil {
		t.Fatalf("parsePartitions: %v", err)
	}
	if o.rootPartition != "/dev/sda2" {
		t.Fatalf("rootPartition = %q, want selected root /dev/sda2", o.rootPartition)
	}
}

func TestParsePartitionsRejectsAmbiguousConfiguredRootPartitionLabel(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RootPartitionLabel = "ubuntu-root"
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "EFI"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "ubuntu-root"},
		{Node: "/dev/sda3", Type: disk.LinuxFilesystemGUID, Name: "ubuntu-root"},
	}), nil)

	err := o.parsePartitions(context.Background())
	if err == nil {
		t.Fatal("expected ambiguous root label error")
	}
	if !strings.Contains(err.Error(), `root partition label "ubuntu-root" matched 2 Linux partitions`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParsePartitionsRejectsMissingConfiguredRootPartitionLabel(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RootPartitionLabel = "missing-root"
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "EFI"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "ubuntu-root"},
	}), nil)

	err := o.parsePartitions(context.Background())
	if err == nil {
		t.Fatal("expected missing root label error")
	}
	if !strings.Contains(err.Error(), `root partition label "missing-root" not found`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParsePartitionsRejectsNonLinuxConfiguredRootPartitionNumber(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.RootPartitionNumber = 1
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "EFI"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "ubuntu-root"},
	}), nil)

	err := o.parsePartitions(context.Background())
	if err == nil {
		t.Fatal("expected non-Linux root selector error")
	}
	if !strings.Contains(err.Error(), "non-Linux partition type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestABStreamTargetsPreserveExistingSkipsSharedBoot(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.SourceRootLabel = "rootfs"
	cfg.Provision.AB.SourceRootPartition = 0
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.bootPartition = "/dev/sda1"
	o.rootPartition = "/dev/sda3"

	targets := o.abStreamTargets()
	if targets.Disk != "/dev/sda" {
		t.Fatalf("Disk = %q, want /dev/sda", targets.Disk)
	}
	if targets.RootPartition != "/dev/sda3" {
		t.Fatalf("RootPartition = %q, want /dev/sda3", targets.RootPartition)
	}
	if targets.BootPartition != "" {
		t.Fatalf("BootPartition = %q, want empty when preserving existing A/B boot assets", targets.BootPartition)
	}
	if targets.SourceRootLabel != "rootfs" {
		t.Fatalf("SourceRootLabel = %q, want rootfs", targets.SourceRootLabel)
	}
}

func TestShouldPreserveABBootEntries(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		preserve bool
		want     bool
	}{
		{name: "A/B preserve existing", mode: config.ImageModeAB, preserve: true, want: true},
		{name: "A/B fresh install", mode: config.ImageModeAB, preserve: false, want: false},
		{name: "whole disk preserve flag ignored", mode: config.ImageModeWholeDisk, preserve: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.Image.Mode = tt.mode
			cfg.Provision.AB.PreserveExisting = tt.preserve
			o := newTestOrchestrator(t, cfg, &mockProvider{})

			if got := o.shouldPreserveABBootEntries(); got != tt.want {
				t.Fatalf("shouldPreserveABBootEntries() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestWriteABSlotState(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotB
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.PreserveExisting = true
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.rootPartition = "/dev/sda2"

	if err := o.writeABSlotState(); err != nil {
		t.Fatalf("writeABSlotState: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(o.config.rootDir, "etc", "booty", "ab-slot.env"))
	if err != nil {
		t.Fatalf("read ab-slot.env: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"BOOTY_AB_TARGET_SLOT=a",
		"BOOTY_AB_BOOTED_SLOT=a",
		"BOOTY_AB_ACTIVE_SLOT=b",
		"BOOTY_AB_PRESERVE_EXISTING=true",
		"BOOTY_AB_ROOT_PARTITION=/dev/sda2",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("ab-slot.env missing %q in:\n%s", want, content)
		}
	}
}

func TestReadABSlotStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ab-slot.env")
	if err := os.WriteFile(path, []byte("BOOTY_AB_BOOTED_SLOT='b'\nBOOTY_AB_TARGET_SLOT=a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := readABSlotStateFile(path)
	if err != nil {
		t.Fatalf("readABSlotStateFile: %v", err)
	}
	if state["BOOTY_AB_BOOTED_SLOT"] != "b" {
		t.Fatalf("BOOTY_AB_BOOTED_SLOT = %q, want b", state["BOOTY_AB_BOOTED_SLOT"])
	}
}

func TestDetectBootedABSlotFromCmdline(t *testing.T) {
	oldEval := evalRootSymlinks
	oldSysBlockRoot := sysBlockRoot
	sysRoot := t.TempDir()
	sysBlockRoot = sysRoot
	evalRootSymlinks = func(path string) (string, error) {
		switch path {
		case "/dev/disk/by-uuid/root-a-uuid":
			return "/dev/sda2", nil
		case "/dev/disk/by-partuuid/root-b-partuuid":
			return "/dev/sda3", nil
		case "/dev/mapper/crypt-root-b":
			return "/dev/dm-0", nil
		default:
			return "", os.ErrNotExist
		}
	}
	t.Cleanup(func() {
		evalRootSymlinks = oldEval
		sysBlockRoot = oldSysBlockRoot
	})
	slavesDir := filepath.Join(sysRoot, "dm-0", "slaves")
	if err := os.MkdirAll(slavesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slavesDir, "sda3"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		cmdline  string
		disk     string
		wantSlot string
	}{
		{name: "partlabel slot a", cmdline: "quiet root=PARTLABEL=BOOTY-ROOT-A", disk: "/dev/sda", wantSlot: config.ABSlotA},
		{name: "by partlabel slot b", cmdline: "root=/dev/disk/by-partlabel/BOOTY-ROOT-B ro", disk: "/dev/sda", wantSlot: config.ABSlotB},
		{name: "scsi partition slot a", cmdline: "root=/dev/sda2", disk: "/dev/sda", wantSlot: config.ABSlotA},
		{name: "nvme partition slot b", cmdline: "root=/dev/nvme0n1p3", disk: "/dev/nvme0n1", wantSlot: config.ABSlotB},
		{name: "uuid slot a", cmdline: "root=UUID=root-a-uuid", disk: "/dev/sda", wantSlot: config.ABSlotA},
		{name: "partuuid slot b", cmdline: "root=PARTUUID=root-b-partuuid", disk: "/dev/sda", wantSlot: config.ABSlotB},
		{name: "mapper slave slot b", cmdline: "root=/dev/mapper/crypt-root-b", disk: "/dev/sda", wantSlot: config.ABSlotB},
		{name: "stale root before slot b", cmdline: "root=UUID=old-root root=PARTLABEL=BOOTY-ROOT-B", disk: "/dev/sda", wantSlot: config.ABSlotB},
		{name: "last booty root wins", cmdline: "root=PARTLABEL=BOOTY-ROOT-A root=PARTLABEL=BOOTY-ROOT-B", disk: "/dev/sda", wantSlot: config.ABSlotB},
		{name: "unknown root", cmdline: "root=UUID=abcd", disk: "/dev/sda", wantSlot: ""},
		{name: "no root", cmdline: "console=ttyS0", disk: "/dev/sda", wantSlot: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProcCmdline(t, tt.cmdline)
			got, err := detectBootedABSlotFromCmdline(tt.disk)
			if err != nil {
				t.Fatalf("detectBootedABSlotFromCmdline: %v", err)
			}
			if got != tt.wantSlot {
				t.Fatalf("detectBootedABSlotFromCmdline() = %q, want %q", got, tt.wantSlot)
			}
		})
	}
}

func TestValidateABBootedSlotSignalAllowsProvisionerBootWithoutABRoot(t *testing.T) {
	withProcCmdline(t, "console=ttyS0 root=LABEL=caas-deploy-image")

	if err := validateABBootedSlotSignal("/dev/sda", config.ABSlotA); err != nil {
		t.Fatalf("validateABBootedSlotSignal: %v", err)
	}
}

func TestValidateABBootedSlotSignalRejectsMismatchedABRoot(t *testing.T) {
	withProcCmdline(t, "console=ttyS0 root=PARTLABEL=BOOTY-ROOT-B")

	err := validateABBootedSlotSignal("/dev/sda", config.ABSlotA)
	if err == nil {
		t.Fatal("expected stale active slot rejection")
	}
	if !strings.Contains(err.Error(), `kernel cmdline reports booted A/B slot "b", config declares active slot "a"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestABSlotPartitionDevice(t *testing.T) {
	tests := []struct {
		slot string
		want string
	}{
		{slot: config.ABSlotA, want: "/dev/sda2"},
		{slot: config.ABSlotB, want: "/dev/sda3"},
	}
	for _, tt := range tests {
		got, err := abSlotPartitionDevice("/dev/sda", tt.slot)
		if err != nil {
			t.Fatalf("abSlotPartitionDevice(%q): %v", tt.slot, err)
		}
		if got != tt.want {
			t.Fatalf("abSlotPartitionDevice(%q) = %q, want %q", tt.slot, got, tt.want)
		}
	}
}

func TestWriteABSlotStateRejectsSymlinkEscape(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(o.config.rootDir, "etc")); err != nil {
		t.Fatal(err)
	}

	err := o.writeABSlotState()
	if err == nil {
		t.Fatal("expected symlink escape error")
	}
	if !strings.Contains(err.Error(), "target escapes provisioned root") {
		t.Fatalf("error = %v, want target escape", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "booty", "ab-slot.env")); !os.IsNotExist(err) {
		t.Fatalf("A/B slot state followed symlink into %s", outside)
	}
}

func TestWipeOrSecureEraseDisksSkipsWholeDiskWipeForABPreserveExisting(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no wipe commands, got %#v", cmd.calls)
	}
}

func TestABPreserveExistingValidatesExistingLayoutBeforeWipe(t *testing.T) {
	withProcCmdline(t, "console=ttyS0 root=PARTLABEL=BOOTY-ROOT-A")
	withABSlotStateMount(t, config.ABSlotA)

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.PreserveExisting = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = filepath.Join(t.TempDir(), "disk")
	activePartition := disk.PartitionDevicePath(o.targetDisk, 2)
	if err := os.WriteFile(activePartition, nil, 0o644); err != nil {
		t.Fatalf("create fake active partition: %v", err)
	}
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: disk.PartitionDevicePath(o.targetDisk, 1), Type: disk.EFISystemPartitionGUID, Name: "BOOTY-EFI"},
		{Node: activePartition, Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-A"},
		{Node: disk.PartitionDevicePath(o.targetDisk, 3), Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-B"},
		{Node: disk.PartitionDevicePath(o.targetDisk, 4), Type: disk.LinuxFilesystemGUID, Name: "BOOTY-STATE"},
	}), nil)

	if err := o.ensureABPartitionLayout(); err != nil {
		t.Fatalf("ensureABPartitionLayout: %v", err)
	}
	if err := o.parsePartitionsFromLayout(context.Background()); err != nil {
		t.Fatalf("parsePartitionsFromLayout: %v", err)
	}
	if err := o.validateABPreserveExistingLayout(context.Background()); err != nil {
		t.Fatalf("validateABPreserveExistingLayout: %v", err)
	}
	if err := o.prepareABTargetSlot(context.Background()); err != nil {
		t.Fatalf("prepareABTargetSlot: %v", err)
	}
	targetPartition := disk.PartitionDevicePath(o.targetDisk, 3)
	if !hasCommandCall(cmd.calls, "wipefs", "-af", targetPartition) {
		t.Fatalf("expected wipefs only on inactive root %s, calls: %#v", targetPartition, cmd.calls)
	}
}

func TestABPreserveExistingRejectsMissingActiveSlotDeviceBeforeWipe(t *testing.T) {
	withProcCmdline(t, "console=ttyS0 root=LABEL=caas-deploy-image")

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.PreserveExisting = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = filepath.Join(t.TempDir(), "disk")
	activePartition := disk.PartitionDevicePath(o.targetDisk, 2)
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: disk.PartitionDevicePath(o.targetDisk, 1), Type: disk.EFISystemPartitionGUID, Name: "BOOTY-EFI"},
		{Node: activePartition, Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-A"},
		{Node: disk.PartitionDevicePath(o.targetDisk, 3), Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-B"},
		{Node: disk.PartitionDevicePath(o.targetDisk, 4), Type: disk.LinuxFilesystemGUID, Name: "BOOTY-STATE"},
	}), nil)

	if err := o.ensureABPartitionLayout(); err != nil {
		t.Fatalf("ensureABPartitionLayout: %v", err)
	}
	if err := o.parsePartitionsFromLayout(context.Background()); err != nil {
		t.Fatalf("parsePartitionsFromLayout: %v", err)
	}
	err := o.validateABPreserveExistingLayout(context.Background())
	if err == nil {
		t.Fatal("expected missing active slot device rejection")
	}
	if !strings.Contains(err.Error(), "active A/B partition device "+activePartition+" is not present") {
		t.Fatalf("error = %v", err)
	}
	if hasCommandName(cmd.calls, "wipefs") {
		t.Fatalf("missing active slot validation must fail before wipefs, calls: %#v", cmd.calls)
	}
}

func TestABPreserveExistingRejectsStaleActiveSlotFromCmdline(t *testing.T) {
	withProcCmdline(t, "console=ttyS0 root=PARTLABEL=BOOTY-ROOT-B")

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.PreserveExisting = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "BOOTY-EFI"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-A"},
		{Node: "/dev/sda3", Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-B"},
		{Node: "/dev/sda4", Type: disk.LinuxFilesystemGUID, Name: "BOOTY-STATE"},
	}), nil)

	if err := o.ensureABPartitionLayout(); err != nil {
		t.Fatalf("ensureABPartitionLayout: %v", err)
	}
	if err := o.parsePartitionsFromLayout(context.Background()); err != nil {
		t.Fatalf("parsePartitionsFromLayout: %v", err)
	}
	err := o.validateABPreserveExistingLayout(context.Background())
	if err == nil {
		t.Fatal("expected stale active slot rejection")
	}
	if !strings.Contains(err.Error(), `kernel cmdline reports booted A/B slot "b", config declares active slot "a"`) {
		t.Fatalf("error = %v", err)
	}
	if hasCommandName(cmd.calls, "wipefs") {
		t.Fatalf("stale active slot validation must fail before wipefs, calls: %#v", cmd.calls)
	}
}

func TestABPreserveExistingRejectsUnexpectedLayoutBeforeWipe(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.AB.PreserveExisting = true
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{
		{Node: "/dev/sda1", Type: disk.EFISystemPartitionGUID, Name: "SYSTEM"},
		{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-B"},
		{Node: "/dev/sda3", Type: disk.LinuxFilesystemGUID, Name: "BOOTY-ROOT-A"},
		{Node: "/dev/sda4", Type: disk.LinuxFilesystemGUID, Name: "BOOTY-STATE"},
	}), nil)

	err := o.streamABImage(context.Background(), "https://image.example/os.raw", nil)
	if err == nil {
		t.Fatal("expected unexpected preserved A/B layout to fail")
	}
	if !strings.Contains(err.Error(), `existing A/B partition 1 label = "SYSTEM", want "BOOTY-EFI"`) {
		t.Fatalf("error = %v", err)
	}
	if hasCommandName(cmd.calls, "wipefs") {
		t.Fatalf("preserve layout validation must fail before wipefs, calls: %#v", cmd.calls)
	}
}

func TestResolveRootFromLayoutPrefersLVMRoot(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.targetDisk = "/dev/sda"

	layout := &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "pv", Mountpoint: "/var"},
		},
		LVM: &config.LVMConfig{
			VolumeGroup: "sysvg",
			Volumes: []config.LVVolume{
				{Name: "root", Mountpoint: "/"},
			},
		},
	}

	if err := o.resolveRootFromLayout(layout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.rootPartition != "/dev/sysvg/root" {
		t.Errorf("rootPartition = %q, want /dev/sysvg/root", o.rootPartition)
	}
}

func TestDetectDiskUsesPartitionLayoutDeviceOverride(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Device: "/dev/disk/by-id/test-disk",
		Table:  "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.detectDisk(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.targetDisk != "/dev/disk/by-id/test-disk" {
		t.Fatalf("targetDisk = %q, want /dev/disk/by-id/test-disk", o.targetDisk)
	}
}

func TestDetectDiskTrimsPartitionLayoutDeviceOverride(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Device: "  /dev/disk/by-id/test-disk  ",
		Table:  "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.detectDisk(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.targetDisk != "/dev/disk/by-id/test-disk" {
		t.Fatalf("targetDisk = %q, want /dev/disk/by-id/test-disk", o.targetDisk)
	}
}

func TestResolveRootFromLayoutFallsBackToPartitionRoot(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.targetDisk = "/dev/sda"

	layout := &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "data", Mountpoint: "/var"},
			{Label: "root", Mountpoint: "/"},
		},
		LVM: &config.LVMConfig{
			VolumeGroup: "sysvg",
			Volumes: []config.LVVolume{
				{Name: "var", Mountpoint: "/var/lib"},
			},
		},
	}

	if err := o.resolveRootFromLayout(layout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.rootPartition != "/dev/sda2" {
		t.Errorf("rootPartition = %q, want /dev/sda2", o.rootPartition)
	}
}

func TestResolveRootFromLayoutMissingRoot(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.targetDisk = "/dev/sda"

	layout := &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "data", Mountpoint: "/var"},
		},
		LVM: &config.LVMConfig{
			VolumeGroup: "sysvg",
			Volumes: []config.LVVolume{
				{Name: "data", Mountpoint: "/data"},
			},
		},
	}

	err := o.resolveRootFromLayout(layout)
	if err == nil {
		t.Fatal("expected error when no root mountpoint is defined")
	}
	if !strings.Contains(err.Error(), "mountpoint \"/\"") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStreamImagePartitionLayoutFailsWithoutImages(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.targetDisk = "/dev/sda"

	err := o.streamImage(context.Background())
	if err == nil {
		t.Fatal("expected error when partition layout is used without image urls")
	}
	if !strings.Contains(err.Error(), "no image URLs provided") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStreamImagePartitionLayoutRejectsPartitionImageMode(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModePartition
	cfg.Provision.Image.URLs = []string{"https://images.local/node.img.zst"}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"

	err := o.streamImage(context.Background())
	if err == nil {
		t.Fatal("expected partition image mode to reject declarative partition layout")
	}
	if !strings.Contains(err.Error(), "cannot be combined with partition layout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStreamImagePartitionLayoutStreamsDeclaredRoot(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"https://images.local/node.img.zst"}
	cfg.Provision.Image.SourceRootLabel = "root-a"
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "efi", Filesystem: "vfat", Mountpoint: "/boot/efi"},
			{Label: "root", Filesystem: "ext4", Mountpoint: "/"},
		},
	}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.targetDisk = "/dev/sda"

	previous := streamRootImageFn
	called := false
	streamRootImageFn = func(_ context.Context, gotURL string, target image.RootTarget, opts ...image.StreamOpts) error {
		called = true
		if gotURL != "https://images.local/node.img.zst" {
			t.Fatalf("stream URL = %q", gotURL)
		}
		if target.RootPartition != "/dev/sda2" {
			t.Fatalf("root target = %q, want /dev/sda2", target.RootPartition)
		}
		if target.SourceRootLabel != "root-a" {
			t.Fatalf("source root label = %q, want root-a", target.SourceRootLabel)
		}
		if target.SourceRootPartition != 0 {
			t.Fatalf("source root partition = %d, want 0", target.SourceRootPartition)
		}
		if len(opts) != 0 {
			t.Fatalf("unexpected stream opts: %#v", opts)
		}
		return nil
	}
	t.Cleanup(func() {
		streamRootImageFn = previous
	})

	err := o.streamImage(context.Background())
	if err != nil {
		t.Fatalf("streamImage: %v", err)
	}
	if !called {
		t.Fatal("streamRootImageFn was not called")
	}
}

func TestWriteFstabUsesDetectedPartitionLayoutRootFilesystem(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "efi", Filesystem: "vfat", Mountpoint: "/boot/efi"},
			{Label: "root", Filesystem: "xfs", Mountpoint: "/"},
		},
	}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.rootPartition = "/dev/sda2"
	cmd.setResult("blkid -o", []byte("ext4\n"), nil)

	if err := o.writeFstab(context.Background()); err != nil {
		t.Fatalf("writeFstab: %v", err)
	}

	fstabBytes, err := os.ReadFile(filepath.Join(o.config.rootDir, "etc", "fstab"))
	if err != nil {
		t.Fatalf("read fstab: %v", err)
	}
	fstab := string(fstabBytes)
	if !strings.Contains(fstab, "PARTLABEL=root\t/\text4\tdefaults\t0\t1") {
		t.Fatalf("fstab root entry =\n%s\nwant detected ext4", fstab)
	}
	if cfg.Provision.Disk.PartitionLayout.Partitions[1].Filesystem != "xfs" {
		t.Fatalf("layout root filesystem mutated to %q", cfg.Provision.Disk.PartitionLayout.Partitions[1].Filesystem)
	}
}

func TestWriteFstabFailsWhenPartitionLayoutRootFilesystemIsUnknown(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "root", Filesystem: "xfs", Mountpoint: "/"},
		},
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	o.rootPartition = "/dev/sda1"

	err := o.writeFstab(context.Background())
	if err == nil {
		t.Fatal("expected empty blkid result to fail")
	}
	if !strings.Contains(err.Error(), "empty filesystem type") {
		t.Fatalf("writeFstab error = %v", err)
	}
}

func TestParsePartitionsFromLayoutNoBootEFIMountpoint(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table: "gpt",
		Partitions: []config.Partition{
			{Label: "data", Filesystem: "vfat", Mountpoint: "/boot"},
			{Label: "root", Filesystem: "ext4", Mountpoint: "/"},
		},
	}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	o.targetDisk = "/dev/sda"

	err := o.parsePartitionsFromLayout(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.rootPartition != "/dev/sda2" {
		t.Errorf("rootPartition = %q, want /dev/sda2", o.rootPartition)
	}
	if o.bootPartition != "" {
		t.Errorf("bootPartition = %q, want empty when /boot/efi is not declared", o.bootPartition)
	}
}

func TestGrowPartitionSkippedForPartitionLayout(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{Table: "gpt", Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}}}
	o := NewOrchestrator(
		cfg,
		&mockProvider{},
		disk.NewManager(cmd),
	)
	o.targetDisk = "/dev/sda"
	o.rootPartition = "/dev/sda1"

	if err := o.growPartition(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("expected no commands when grow-partition is skipped, got %d", len(cmd.calls))
	}
}

func TestResizeFilesystemRunsForPartitionLayout(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{Table: "gpt", Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}}}
	o := NewOrchestrator(
		cfg,
		&mockProvider{},
		disk.NewManager(cmd),
	)
	o.rootPartition = "/dev/sda1"

	if err := o.resizeFilesystem(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCommandCall(cmd.calls, "resize2fs", "/dev/sda1") {
		t.Fatalf("expected resize2fs /dev/sda1 when resizing partition layout root, got %#v", cmd.calls)
	}
}

func TestResizeFilesystemRunsForABPartitionLayout(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{Table: "gpt", Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}}}
	o := NewOrchestrator(
		cfg,
		&mockProvider{},
		disk.NewManager(cmd),
	)
	o.rootPartition = "/dev/sda3"

	if err := o.resizeFilesystem(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCommandCall(cmd.calls, "resize2fs", "/dev/sda3") {
		t.Fatalf("expected resize2fs /dev/sda3 when resizing A/B root slot, got %#v", cmd.calls)
	}
}

func TestResizeFilesystemUsesMountedRootForXFS(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{}
	o := NewOrchestrator(
		cfg,
		&mockProvider{},
		disk.NewManager(cmd),
	)
	o.rootPartition = "/dev/sda2"
	cmd.setResult("resize2fs /dev/sda2", nil, fmt.Errorf("not ext"))

	if err := o.resizeFilesystem(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCommandCall(cmd.calls, "xfs_growfs", newroot) {
		t.Fatalf("expected xfs_growfs to target mounted root %s, got %#v", newroot, cmd.calls)
	}
}

func TestInjectCloudInit_Disabled(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("expected no error when CloudInit disabled, got %v", err)
	}
}

func TestInjectCloudInit_UnsupportedDatasource(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "openstack"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.injectCloudInit(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported datasource")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("expected 'unsupported' in error, got %v", err)
	}
}

func TestInjectCloudInit_NoCloudInject(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "nocloud"
	cfg.Network.Static.Iface = "eth0"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the seed files were created under rootDir.
	seedDir := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "nocloud")
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		if _, err := os.Stat(filepath.Join(seedDir, name)); err != nil {
			t.Errorf("expected seed file %s to exist: %v", name, err)
		}
	}
}

func TestInjectCloudInit_RequiresStaticInterfaceWithoutBond(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "nocloud"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.injectCloudInit(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "STATIC_IFACE") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInjectCloudInit_NoCloudUsesStaticInterface(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "nocloud"
	cfg.Network.Static.Iface = "eth0"
	cfg.Network.Static.IP = "10.0.0.10/24"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	netPath := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "nocloud", "network-config")
	data, err := os.ReadFile(netPath)
	if err != nil {
		t.Fatalf("read network-config: %v", err)
	}

	var nc cloudinit.NetworkConfig
	if err := yaml.Unmarshal(data, &nc); err != nil {
		t.Fatalf("unmarshal network-config: %v", err)
	}
	if _, ok := nc.Ethernets["id0"]; ok {
		t.Fatalf("unexpected id0 ethernet in network-config: %+v", nc.Ethernets)
	}
	eth, ok := nc.Ethernets["eth0"]
	if !ok {
		t.Fatalf("network-config ethernets = %+v, want eth0", nc.Ethernets)
	}
	if len(eth.Addresses) != 1 || eth.Addresses[0] != "10.0.0.10/24" {
		t.Fatalf("eth0 addresses = %v", eth.Addresses)
	}
}

func TestConfigureDNSPersistsUbuntuStaticInterface(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.PersistNetwork = true
	cfg.OSFamily = "ubuntu"
	cfg.Network.Static.IP = "10.1.0.5/24"
	cfg.Network.Static.Gateway = "10.1.0.1"
	cfg.Network.Static.Iface = "eth0"
	cfg.Network.DNSResolvers = "8.8.8.8, 1.1.1.1"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.configureDNS(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(o.config.rootDir, "etc", "netplan", "01-booty-provisioned.yaml"))
	if err != nil {
		t.Fatalf("read netplan config: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"eth0:",
		"addresses: [10.1.0.5/24]",
		"via: 10.1.0.1",
		"addresses: [8.8.8.8, 1.1.1.1]",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("netplan config missing %q:\n%s", want, content)
		}
	}
}

func TestTargetNetworkConfigBondAndVLANMapping(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.MachineConfig)
		assert    func(*testing.T, *networkpersist.NetworkConfig)
		wantErr   string
	}{
		{
			name: "bond with static address",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.Bond.Interfaces = "eth0, eth1"
				cfg.Network.Bond.Mode = "802.3ad"
				cfg.Network.Static.IP = "10.1.0.5/24"
				cfg.Network.Static.Gateway = "10.1.0.1"
			},
			assert: func(t *testing.T, got *networkpersist.NetworkConfig) {
				t.Helper()
				if len(got.Bonds) != 1 {
					t.Fatalf("bonds = %#v, want one bond", got.Bonds)
				}
				bond := got.Bonds[0]
				if bond.Name != "bond0" || bond.Mode != "802.3ad" || bond.Address != "10.1.0.5/24" || bond.Gateway != "10.1.0.1" {
					t.Fatalf("unexpected bond config: %#v", bond)
				}
				if len(bond.Members) != 2 || bond.Members[0] != "eth0" || bond.Members[1] != "eth1" {
					t.Fatalf("unexpected bond members: %#v", bond.Members)
				}
			},
		},
		{
			name: "bond without address or vlan is rejected",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.Bond.Interfaces = "eth0,eth1"
			},
			wantErr: "bond network persistence requires static ip or vlan config",
		},
		{
			name: "bond lacp alias persists canonical mode",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.Bond.Interfaces = "eth0,eth1"
				cfg.Network.Bond.Mode = " LACP "
				cfg.Network.Static.IP = "10.1.0.5/24"
			},
			assert: func(t *testing.T, got *networkpersist.NetworkConfig) {
				t.Helper()
				if len(got.Bonds) != 1 {
					t.Fatalf("bonds = %#v, want one bond", got.Bonds)
				}
				if got.Bonds[0].Mode != "802.3ad" {
					t.Fatalf("bond mode = %q, want 802.3ad", got.Bonds[0].Mode)
				}
			},
		},
		{
			name: "bond with vlan drops unaddressed gateway",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.Bond.Interfaces = "eth0,eth1"
				cfg.Network.Static.Gateway = "10.1.0.1"
				cfg.Network.VLAN.Config = "200:bond0:10.200.0.42/24"
			},
			assert: func(t *testing.T, got *networkpersist.NetworkConfig) {
				t.Helper()
				if len(got.Bonds) != 1 {
					t.Fatalf("bonds = %#v, want one bond", got.Bonds)
				}
				bond := got.Bonds[0]
				if bond.Address != "" || bond.Gateway != "" {
					t.Fatalf("unexpected unaddressed bond config: %#v", bond)
				}
			},
		},
		{
			name: "vlans map dhcp and static addresses",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.VLAN.Config = "200:eno1:10.200.0.42/24,300:eno2"
			},
			assert: func(t *testing.T, got *networkpersist.NetworkConfig) {
				t.Helper()
				if len(got.VLANs) != 2 {
					t.Fatalf("vlans = %#v, want two vlans", got.VLANs)
				}
				if got.VLANs[0].Parent != "eno1" || got.VLANs[0].ID != 200 || got.VLANs[0].Address != "10.200.0.42/24" || got.VLANs[0].DHCP {
					t.Fatalf("unexpected static vlan: %#v", got.VLANs[0])
				}
				if got.VLANs[1].Parent != "eno2" || got.VLANs[1].ID != 300 || !got.VLANs[1].DHCP || got.VLANs[1].Address != "" {
					t.Fatalf("unexpected dhcp vlan: %#v", got.VLANs[1])
				}
			},
		},
		{
			name: "dhcp interface drops gateway",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.Static.Iface = "eno1"
				cfg.Network.Static.Gateway = "10.1.0.1"
			},
			assert: func(t *testing.T, got *networkpersist.NetworkConfig) {
				t.Helper()
				if len(got.Interfaces) != 1 {
					t.Fatalf("interfaces = %#v, want one interface", got.Interfaces)
				}
				iface := got.Interfaces[0]
				if iface.Name != "eno1" || !iface.DHCP || iface.Address != "" || iface.Gateway != "" {
					t.Fatalf("unexpected dhcp interface config: %#v", iface)
				}
			},
		},
		{
			name: "vlan gateway is rejected",
			configure: func(cfg *config.MachineConfig) {
				cfg.Network.VLAN.Config = "200:eno1:10.200.0.42/24:10.200.0.1"
			},
			wantErr: "cannot render vlan gateways yet",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			tc.configure(cfg)
			o := newTestOrchestrator(t, cfg, &mockProvider{})

			got, err := o.targetNetworkConfig()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("targetNetworkConfig() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("targetNetworkConfig() unexpected error: %v", err)
			}
			tc.assert(t, got)
		})
	}
}

func TestPersistNetworkConfigRequiresOSFamily(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.PersistNetwork = true
	cfg.Network.Static.IP = "10.1.0.5/24"
	cfg.Network.Static.Iface = "eth0"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.persistNetworkConfig()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unsupported OS family") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPersistNetworkConfigRequiresInterfaceForStaticIP(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.PersistNetwork = true
	cfg.OSFamily = "ubuntu"
	cfg.Network.Static.IP = "10.1.0.5/24"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.persistNetworkConfig()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "static iface is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInjectCloudInit_ConfigDriveInject(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "configdrive"
	cfg.Provision.ProviderID = "redfish://bmc.example/Systems/1"
	cfg.Network.Static.Iface = "eth0"
	cfg.Network.Static.IP = "10.0.0.10/24"
	cfg.Network.Static.Gateway = "10.0.0.1"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	seedDir := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "config_drive", "openstack", "latest")
	for _, name := range []string{"user_data", "meta_data.json", "network_data.json"} {
		if _, err := os.Stat(filepath.Join(seedDir, name)); err != nil {
			t.Errorf("expected seed file %s to exist: %v", name, err)
		}
	}

	metaData, err := os.ReadFile(filepath.Join(seedDir, "meta_data.json"))
	if err != nil {
		t.Fatalf("read meta_data.json: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metaData, &metadata); err != nil {
		t.Fatalf("unmarshal meta_data.json: %v", err)
	}
	if metadata["uuid"] != "redfish://bmc.example/Systems/1" {
		t.Fatalf("metadata uuid = %v", metadata["uuid"])
	}
}

func TestInjectCloudInit_DefaultDatasourceAndStableInstanceID(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.ProviderID = "redfish://bmc.example/Systems/1"
	cfg.Network.Static.Iface = "eth0"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	metaPath := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "nocloud", "meta-data")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta-data: %v", err)
	}
	if !strings.Contains(string(data), "instance-id: redfish://bmc.example/Systems/1") {
		t.Fatalf("meta-data missing stable instance-id, got: %s", string(data))
	}
}

func TestInjectCloudInit_ConfigDriveDatasourceCaseInsensitiveAndTrimmed(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = " ConfigDrive "
	cfg.Network.Static.Iface = "eth0"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	seedDir := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "config_drive", "openstack", "latest")
	if _, err := os.Stat(filepath.Join(seedDir, "meta_data.json")); err != nil {
		t.Fatalf("expected configdrive meta_data.json: %v", err)
	}
}

func TestInjectCloudInit_TrimmedBondAndDNSValues(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "nocloud"
	cfg.Network.Static.IP = "10.0.0.10/24"
	cfg.Network.Static.Gateway = "10.0.0.1"
	cfg.Network.Bond.Interfaces = "eth0, eth1, ,"
	cfg.Network.Bond.Mode = "802.3ad"
	cfg.Network.DNSResolvers = "8.8.8.8, 1.1.1.1, ,"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	netPath := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "nocloud", "network-config")
	data, err := os.ReadFile(netPath)
	if err != nil {
		t.Fatalf("read network-config: %v", err)
	}

	var nc cloudinit.NetworkConfig
	if err := yaml.Unmarshal(data, &nc); err != nil {
		t.Fatalf("unmarshal network-config: %v", err)
	}

	bond, ok := nc.Bonds["bond0"]
	if !ok {
		t.Fatal("expected bond0 in network-config")
	}
	if len(bond.Interfaces) != 2 || bond.Interfaces[0] != "eth0" || bond.Interfaces[1] != "eth1" {
		t.Fatalf("unexpected bond interfaces: %v", bond.Interfaces)
	}
	if bond.Nameservers == nil || len(bond.Nameservers.Addresses) != 2 {
		t.Fatalf("unexpected nameservers: %+v", bond.Nameservers)
	}
	for _, addr := range bond.Nameservers.Addresses {
		if strings.TrimSpace(addr) != addr {
			t.Fatalf("nameserver has whitespace: %q", addr)
		}
	}
}

func TestInjectCloudInit_NoCloudVLANConfig(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "nocloud"
	cfg.Network.VLAN.Config = "200:eno1:10.200.0.42/24:10.200.0.1,300:eno2"
	cfg.Network.DNSResolvers = "1.1.1.1"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	netPath := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "nocloud", "network-config")
	data, err := os.ReadFile(netPath)
	if err != nil {
		t.Fatalf("read network-config: %v", err)
	}

	var nc cloudinit.NetworkConfig
	if err := yaml.Unmarshal(data, &nc); err != nil {
		t.Fatalf("unmarshal network-config: %v", err)
	}

	if _, ok := nc.Ethernets["id0"]; ok {
		t.Fatalf("unexpected fallback id0 ethernet: %#v", nc.Ethernets)
	}
	if _, ok := nc.Ethernets["eno1"]; !ok {
		t.Fatalf("missing VLAN parent ethernet: %#v", nc.Ethernets)
	}
	staticVLAN, ok := nc.VLANs["eno1.200"]
	if !ok {
		t.Fatalf("missing static VLAN: %#v", nc.VLANs)
	}
	if staticVLAN.Gateway4 != "10.200.0.1" || staticVLAN.DHCP4 {
		t.Fatalf("unexpected static VLAN config: %#v", staticVLAN)
	}
	if staticVLAN.Nameservers == nil || len(staticVLAN.Nameservers.Addresses) != 1 {
		t.Fatalf("unexpected VLAN nameservers: %#v", staticVLAN.Nameservers)
	}
	dhcpVLAN, ok := nc.VLANs["eno2.300"]
	if !ok {
		t.Fatalf("missing DHCP VLAN: %#v", nc.VLANs)
	}
	if !dhcpVLAN.DHCP4 || len(dhcpVLAN.Addresses) != 0 {
		t.Fatalf("unexpected DHCP VLAN config: %#v", dhcpVLAN)
	}
}

func TestInjectCloudInit_InvalidVLANConfigFails(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "test-host",
	}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Network.VLAN.Config = "bad-vlan"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	err := o.injectCloudInit(context.Background())
	if err == nil {
		t.Fatal("expected invalid VLAN config to fail")
	}
	if !strings.Contains(err.Error(), "invalid cloud-init VLAN config") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDetectDisk_CharDeviceRejected verifies that detectDisk rejects a character
// device when DiskDevice is explicitly configured. Both the validatePartitionLayoutConfig
// and detectDisk code paths must reject character devices consistently.
func TestDetectDisk_CharDeviceRejected(t *testing.T) {
	// /dev/null is always a character device on Linux; use it as a stand-in for
	// a misconfigured char device path.
	charDevice := "/dev/null"
	info, err := os.Stat(charDevice)
	if err != nil {
		t.Fatalf("cannot stat %s: %v", charDevice, err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("%s is not a character device on this host", charDevice)
	}

	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = charDevice
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	err = o.detectDisk(context.Background())
	if err == nil {
		t.Fatal("expected error when DiskDevice is a character device, got nil")
	}
	if !strings.Contains(err.Error(), "not a block device") {
		t.Fatalf("expected 'not a block device' in error, got: %v", err)
	}
}

func TestIsSensitiveEnvKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"AUTH_TOKEN", true},
		{"SECRET_VALUE", true},
		{"DB_PASSWORD", true},
		{"API_KEY", true},
		{"MY_CREDENTIAL", true},
		{"HOME", false},
		{"PATH", false},
		{"NETWORK_MODE", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := isSensitiveEnvKey(tt.key); got != tt.want {
				t.Errorf("isSensitiveEnvKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestStreamImagePartitionModeWipesDiskFirst(t *testing.T) {
	// Verify that WipeDisk is called before StreamPartitions in partition mode.
	// This ensures a clean slate on any retry and prevents data corruption.
	cmd := newMockCommander()
	wipeErr := fmt.Errorf("wipe failed")
	cmd.setResult("wipefs -af", nil, wipeErr)

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = "partition"
	o := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk = "/dev/sda"
	o.bestImageURL = "https://images.local/node.img.zst"

	err := o.streamImage(context.Background())
	if err == nil {
		t.Fatal("expected error when wipefs fails, got nil")
	}
	if !strings.Contains(err.Error(), "wiping disk before partition stream") {
		t.Fatalf("expected 'wiping disk before partition stream' in error, got: %v", err)
	}

	// sgdisk is called first by WipeDisk (before wipefs), so it must appear
	// as the first recorded command.
	if len(cmd.calls) < 1 {
		t.Fatal("expected at least one command to be recorded")
	}
	if cmd.calls[0].name != "sgdisk" {
		t.Fatalf("expected first command to be 'sgdisk' (WipeDisk), got %q", cmd.calls[0].name)
	}
}

func TestStopRAID(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	cmd.setResult("mdadm --stop", nil, nil)
	if err := o.stopRAID(context.Background()); err != nil {
		t.Fatalf("stopRAID: %v", err)
	}
}

func TestStopRAIDError(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	cmd.setResult("mdadm --stop", nil, fmt.Errorf("stop failed"))
	if err := o.stopRAID(context.Background()); err == nil {
		t.Fatal("expected error from stopRAID")
	}
}

func TestDisableLVMStep(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	if err := o.disableLVM(context.Background()); err != nil {
		t.Fatalf("disableLVM: %v", err)
	}
}

func TestEnableLVMStep(t *testing.T) {
	cfg := &config.MachineConfig{}
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	if err := o.enableLVM(context.Background()); err != nil {
		t.Fatalf("enableLVM: %v", err)
	}
}

func TestPartprobe(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("partprobe /dev/sda", nil, nil)
	if err := o.partprobe(context.Background()); err != nil {
		t.Fatalf("partprobe: %v", err)
	}
}

func TestPartprobeError(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.targetDisk = "/dev/sda"
	cmd.setResult("partprobe /dev/sda", nil, fmt.Errorf("partprobe: device busy"))
	cmd.setResult("blockdev --rereadpt", nil, fmt.Errorf("blockdev: device busy"))
	if err := o.partprobe(context.Background()); err == nil {
		t.Fatal("expected error from partprobe when both partprobe and blockdev fail")
	}
}

func TestCheckFilesystem(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.rootPartition = "/dev/sda2"
	cmd.setResult("blkid -o", nil, nil)
	cmd.setResult("e2fsck -fy", nil, nil)
	if err := o.checkFilesystem(context.Background()); err != nil {
		t.Fatalf("checkFilesystem: %v", err)
	}
}

func TestTeardownChrootReturnsJoinedErrors(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.DisableKexec = true
	o, _ := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	err := o.teardownChroot(context.Background())
	if err == nil {
		t.Fatal("expected error from teardownChroot when unmount fails on non-root host")
	}
}

func TestTeardownChrootKeepsNewrootMountedForKexec(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	if err := o.teardownChroot(context.Background()); err != nil {
		t.Fatalf("teardownChroot should keep /newroot mounted for kexec: %v", err)
	}
}

func TestTeardownChrootKeepsABPreserveExistingMountedForKexec(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	o, _ := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	if err := o.teardownChroot(context.Background()); err != nil {
		t.Fatalf("teardownChroot should keep /newroot mounted for preserve-existing kexec: %v", err)
	}
}

func TestTeardownChrootUnmountsWhenFirmwareChangedRequiresReboot(t *testing.T) {
	cfg := &config.MachineConfig{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	o.firmwareChanged = true

	err := o.teardownChroot(context.Background())
	if err == nil {
		t.Fatal("expected teardown error when firmware change disables kexec and unmounts on non-root host")
	}
}

func TestTeardownChrootUnmountsWhenSecureBootReEnableRequiresReboot(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.SecureBoot.ReEnable = true
	o, _ := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})

	err := o.teardownChroot(context.Background())
	if err == nil {
		t.Fatal("expected teardown error when secure boot re-enable disables kexec and unmounts on non-root host")
	}
}

func TestShouldKeepTargetRootMountedForKexecMatchesKexecGates(t *testing.T) {
	if !ShouldKeepTargetRootMountedForKexec(&config.MachineConfig{}, false) {
		t.Fatal("expected kexec-capable config to keep target root mounted")
	}
	cfg := &config.MachineConfig{}
	cfg.Provision.DisableKexec = true
	if ShouldKeepTargetRootMountedForKexec(cfg, false) {
		t.Fatal("disabled kexec must not keep target root mounted")
	}
	cfg = &config.MachineConfig{}
	cfg.Provision.SecureBoot.ReEnable = true
	if ShouldKeepTargetRootMountedForKexec(cfg, false) {
		t.Fatal("secure boot re-enable must not keep target root mounted")
	}
	if ShouldKeepTargetRootMountedForKexec(&config.MachineConfig{}, true) {
		t.Fatal("firmware changes requiring reboot must not keep target root mounted")
	}
}

type telemetryProvider struct {
	mockProvider
	metricsReports int
	eventReports   int
}

func (p *telemetryProvider) ReportMetrics(_ context.Context, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty metrics payload")
	}
	p.metricsReports++
	return nil
}

func (p *telemetryProvider) SendEvent(_ context.Context, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty event payload")
	}
	p.eventReports++
	return nil
}

func TestNewOrchestratorInitializesTelemetry(t *testing.T) {
	provider := &telemetryProvider{}
	o := NewOrchestrator(&config.MachineConfig{}, provider, disk.NewManager(newMockCommander()))

	if o.collector == nil {
		t.Fatal("collector is nil")
	}
	if o.emitter == nil {
		t.Fatal("emitter is nil")
	}
	o.emitter.Emit("provision.completed", "", "done", 1)
	o.flushTelemetry(context.Background())

	if provider.metricsReports != 1 {
		t.Fatalf("metrics reports = %d, want 1", provider.metricsReports)
	}
	if provider.eventReports != 1 {
		t.Fatalf("event reports = %d, want 1", provider.eventReports)
	}
}

func TestFlushTelemetryHandlesNilCollectorAndEmitter(t *testing.T) {
	provider := &telemetryProvider{}
	o := NewOrchestrator(&config.MachineConfig{}, provider, disk.NewManager(newMockCommander()))
	o.collector = nil
	o.emitter = nil

	o.flushTelemetry(context.Background())

	if provider.metricsReports != 0 {
		t.Fatalf("metrics reports = %d, want 0", provider.metricsReports)
	}
	if provider.eventReports != 0 {
		t.Fatalf("event reports = %d, want 0", provider.eventReports)
	}
}

func TestVerifySecureBootChainEnforcesPinnedDigests(t *testing.T) {
	root := t.TempDir()
	withSecureBootRoot(t, root)
	writeSecureBootArtifact(t, root, "/boot/efi/EFI/BOOT/BOOTX64.EFI", minimalProvisionPE())
	writeSecureBootArtifact(t, root, "/boot/efi/EFI/ubuntu/grubx64.efi", minimalProvisionPE())
	writeSecureBootArtifact(t, root, "/boot/vmlinuz", []byte("trusted kernel"))

	shimSum := sha256.Sum256(minimalProvisionPE())
	grubSum := sha256.Sum256(minimalProvisionPE())
	kernelSum := sha256.Sum256([]byte("trusted kernel"))
	cfg := &config.MachineConfig{}
	cfg.Provision.SecureBoot.PinnedDigests = map[string]string{
		"shim":   fmt.Sprintf("%x", shimSum[:]),
		"grub":   "sha256:" + fmt.Sprintf("%x", grubSum[:]),
		"kernel": fmt.Sprintf("%x", kernelSum[:]),
	}
	o := newTestOrchestrator(t, cfg, &mockProvider{})

	if err := o.verifySecureBootChain(context.Background()); err != nil {
		t.Fatalf("verifySecureBootChain() = %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "boot", "vmlinuz"), []byte("tampered kernel"), 0o600); err != nil {
		t.Fatalf("tamper kernel: %v", err)
	}
	err := o.verifySecureBootChain(context.Background())
	if err == nil {
		t.Fatal("verifySecureBootChain() error = nil, want digest mismatch")
	}
	if !strings.Contains(err.Error(), "kernel: validation failed") {
		t.Fatalf("verifySecureBootChain() error = %q, want kernel validation failure", err.Error())
	}
}

func withSecureBootRoot(t *testing.T, root string) {
	t.Helper()
	old := secureBootRoot
	secureBootRoot = root
	t.Cleanup(func() { secureBootRoot = old })
}

func writeSecureBootArtifact(t *testing.T, root, path string, data []byte) {
	t.Helper()
	fullPath := filepath.Join(root, strings.TrimPrefix(path, "/"))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdir artifact parent: %v", err)
	}
	if err := os.WriteFile(fullPath, data, 0o600); err != nil {
		t.Fatalf("write artifact %s: %v", path, err)
	}
}

func minimalProvisionPE() []byte {
	const (
		dosStubSize   = 64
		peSignature   = 4
		coffHdrSize   = 20
		optHdrOffset  = dosStubSize + peSignature + coffHdrSize
		optHeaderSize = 112
		magicPE32Plus = uint16(0x020b)
	)
	machine := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
	if runtime.GOARCH == "arm64" {
		machine = pe.IMAGE_FILE_MACHINE_ARM64
	}
	buf := make([]byte, optHdrOffset+optHeaderSize)
	buf[0] = 'M'
	buf[1] = 'Z'
	binary.LittleEndian.PutUint32(buf[0x3c:], dosStubSize)
	copy(buf[dosStubSize:], []byte("PE\x00\x00"))
	coffBase := dosStubSize + peSignature
	binary.LittleEndian.PutUint16(buf[coffBase:], machine)
	binary.LittleEndian.PutUint16(buf[coffBase+16:], optHeaderSize)
	binary.LittleEndian.PutUint16(buf[coffBase+18:], 0x0002)
	binary.LittleEndian.PutUint16(buf[optHdrOffset:], magicPE32Plus)
	return buf
}

func TestValidateProvisionInputsHTTPInsecure(t *testing.T) {
	t.Run("rejects plain http when allowInsecureHTTP is false", func(t *testing.T) {
		cfg := &config.MachineConfig{Provision: config.ProvisionConfig{TargetOS: "linux", Image: config.ImageConfig{URLs: []string{"http://images.example.invalid/node.raw"}}}}
		o := newTestOrchestrator(t, cfg, &mockProvider{})
		o.cfg.Provision.Image.AllowInsecureHTTP = false
		err := o.validateProvisionInputs(context.Background())
		if err == nil || !strings.Contains(err.Error(), "plain HTTP sources require") {
			t.Errorf("expected error about insecure HTTP, got %v", err)
		}
	})
	t.Run("accepts plain http when allowInsecureHTTP is true", func(t *testing.T) {
		cfg := &config.MachineConfig{Provision: config.ProvisionConfig{TargetOS: "linux", Image: config.ImageConfig{URLs: []string{"http://images.example.invalid/node.raw"}}}}
		o := newTestOrchestrator(t, cfg, &mockProvider{})
		o.cfg.Provision.Image.AllowInsecureHTTP = true
		err := o.validateProvisionInputs(context.Background())
		if err != nil && strings.Contains(err.Error(), "plain HTTP sources require") {
			t.Errorf("did not expect error about insecure HTTP, got %v", err)
		}
	})
}
