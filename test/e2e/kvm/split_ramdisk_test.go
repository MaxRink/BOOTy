//go:build e2e

package kvm

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSplitRamdiskPayloadsQEMU boots the split ramdisk (initrd.Dockerfile
// --target split) the way a boot loader loads it: the base archive, the
// payload archives the run needs, and the deploy vars, concatenated. With
// FRR vars, base + frr + vars passes the payload check and starts the FRR
// daemons (the payload resolves its libraries, its manifest matches the
// build, and the frr user of #794 resolves). base + vars fails with
// BOOTY-PAY-001 and starts no FRR daemon.
//
// BOOTY_SPLIT_DIR: the output of --target split (with its vmlinuz).
func TestSplitRamdiskPayloadsQEMU(t *testing.T) {
	qemuAvailable(t)
	dir := os.Getenv("BOOTY_SPLIT_DIR")
	if dir == "" {
		failOrSkipUnsupportedHost(t, "BOOTY_SPLIT_DIR is required")
	}
	kernel := filepath.Join(dir, "vmlinuz")
	requireKVMAssets(t, filepath.Join(dir, "base.cpio.zst"), kernel)
	vars := map[string]string{
		"HOSTNAME":           "split-node",
		"MODE":               "provision",
		"underlay_ip":        "10.0.0.1",
		"asn_server":         "65001",
		"provision_vni":      "100",
		"INSECURE_TRANSPORT": "true",
		// Not reached: the test ends at the FRR start or the payload error.
		"IMAGE":                     "http://10.0.2.2:1/image.gz",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"DISK_DEVICE":               "/dev/vda",
		"INIT_URL":                  "http://10.0.2.2:1/status/init",
	}
	deploy := flavorInitramfsWithVars(t, emptyFile(t), vars, nil)

	for _, tt := range []struct {
		name     string
		payloads []string
		want     string
	}{
		// bfdd starts last (mgmtd, zebra, staticd, bgpd, bfdd); only a
		// started daemon logs daemon=<name>.
		{name: "base + frr + vars", payloads: []string{"frr"}, want: "daemon=bfdd"},
		{name: "base + vars", want: "BOOTY-PAY-001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parts := make([]string, 0, len(tt.payloads)+2)
			parts = append(parts, filepath.Join(dir, "base.cpio.zst"))
			for _, p := range tt.payloads {
				parts = append(parts, filepath.Join(dir, p+".cpio.zst"))
			}
			initrd := concatFiles(t, append(parts, deploy)...)
			out, found := runQEMUUntil(t, kernel, initrd, tt.want, 4*time.Minute)
			if !found {
				t.Fatalf("serial output lacks %q; tail:\n%s", tt.want, tail(out, 3000))
			}
			if tt.payloads != nil && !strings.Contains(string(out), `msg="started FRR daemon"`) {
				t.Fatalf("no started FRR daemon; tail:\n%s", tail(out, 3000))
			}
			if tt.payloads == nil {
				if strings.Contains(string(out), "started FRR daemon") {
					t.Fatalf("an FRR daemon started without the FRR payload; tail:\n%s", tail(out, 3000))
				}
				return
			}
			for _, bad := range []string{"BOOTY-PAY-", "daemons not found", "exit status 127", "no such file", "error while loading shared libraries"} {
				if strings.Contains(string(out), bad) {
					t.Fatalf("serial output has %q with the FRR payload loaded; tail:\n%s", bad, tail(out, 3000))
				}
			}
		})
	}
}

func emptyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty")
	writeFile(t, path, "")
	return path
}

// concatFiles writes the files one after the other into a new file. The
// kernel unpacks concatenated initramfs archives, each with its own
// compression, like the initrd list of a boot loader.
func concatFiles(t *testing.T, files ...string) string {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "initrd"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	for _, f := range files {
		in, err := os.Open(f) //nolint:gosec // test assets
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		_ = in.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return out.Name()
}

// runQEMUUntil boots kernel and initrd and stops when the serial output
// contains want or the timeout ends.
func runQEMUUntil(t *testing.T, kernel, initrd, want string, timeout time.Duration) ([]byte, bool) {
	t.Helper()
	args := append([]string{
		"-m", "2048", "-nographic", "-no-reboot",
		"-kernel", kernel, "-initrd", initrd,
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56", "-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	found := false
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), want) {
			found = true
			cancel()
			break
		}
	}
	_ = cmd.Wait()
	return []byte(out.String()), found
}
