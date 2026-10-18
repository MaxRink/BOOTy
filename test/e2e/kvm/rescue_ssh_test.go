//go:build e2e

package kvm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRescueSSHLoginQEMU boots the default flavor (its dropbear and its
// /etc/passwd), lets the provision fail on an unreachable image, and logs
// in to the rescue shell over SSH with RESCUE_SSH_PUBKEY. Without a root
// entry in /etc/passwd, dropbear refused every login ("nonexistent user").
//
// BOOTY_FLAVOR_INITRAMFS: initramfs.cpio.zst of the default flavor.
// BOOTY_FLAVOR_KERNEL: vmlinuz of `--target kernel-image`.
func TestRescueSSHLoginQEMU(t *testing.T) {
	qemuAvailable(t)
	flavor := os.Getenv("BOOTY_FLAVOR_INITRAMFS")
	kernel := os.Getenv("BOOTY_FLAVOR_KERNEL")
	if flavor == "" || kernel == "" {
		failOrSkipUnsupportedHost(t, "BOOTY_FLAVOR_INITRAMFS and BOOTY_FLAVOR_KERNEL are required")
	}
	requireKVMAssets(t, flavor, kernel)
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			failOrSkipUnsupportedHost(t, "%s not available", tool)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if out, err := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	image := filepath.Join(dir, "unused.img")
	writeFile(t, image, "")
	caprf := startFakeCAPRF(t, image)
	vars := caprf.nativeVars("provision")
	vars["PROVISION_TARGET_OS"] = "linux"
	vars["IMAGE"] = "http://10.0.2.2:1/missing.raw" // nothing listens: the provision fails
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["RESCUE_MODE"] = "shell"
	vars["RESCUE_SSH_PUBKEY"] = strings.TrimSpace(string(pub))
	initramfs := flavorInitramfsWithVars(t, flavor, vars, nil)

	port := freeTCPPort(ctx, t)
	args := append([]string{
		"-m", "2048", "-nographic", "-no-reboot",
		"-kernel", kernel, "-initrd", initramfs,
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", fmt.Sprintf("user,hostfwd=tcp:127.0.0.1:%d-:22", port),
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	qemu := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	pipe, err := qemu.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	qemu.Stderr = qemu.Stdout
	if err := qemu.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = qemu.Wait() })

	var serial strings.Builder
	started := false
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		serial.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), "SSH daemon started on port 22") {
			started = true
			break
		}
	}
	if !started {
		t.Fatalf("no rescue SSH daemon; tail:\n%s", tail([]byte(serial.String()), 3000))
	}
	go func() { // keep draining the console
		for sc.Scan() {
		}
	}()

	var out []byte
	for range 30 {
		out, err = exec.CommandContext(ctx, "ssh", "-i", key, "-p", fmt.Sprint(port),
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
			"-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
			"root@127.0.0.1", "echo rescue-ok; id -un").CombinedOutput()
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil || !strings.Contains(string(out), "rescue-ok") || !strings.Contains(string(out), "root") {
		t.Fatalf("rescue SSH login failed: %v\n%s\nconsole tail:\n%s", err, out, tail([]byte(serial.String()), 2000))
	}
}

// freeTCPPort returns a free TCP port on 127.0.0.1.
func freeTCPPort(ctx context.Context, t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}
