//go:build e2e

package kvm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/crash"
)

func TestCrashArtifactsStartupUploadBeforeImageDownload(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	existingRaw := createTestDiskImage(t, 512)
	seedRawDiskCrashArtifacts(t, existingRaw)
	targetDisk := filepath.Join(t.TempDir(), "target.qcow2")
	run(t, "convert existing raw to qcow2", "qemu-img", "convert", "-f", "raw", "-O", "qcow2", existingRaw, targetDisk)

	replacementRaw := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, replacementRaw)
	server := newKVMCrashArtifactServer(t, imageGz)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                             "crash-kvm-node",
		"INSECURE_TRANSPORT":                   "true",
		"IMAGE_ALLOW_INSECURE_HTTP":            "true",
		"IMAGE":                                server.guestURL + "/image.gz",
		"MODE":                                 "provision",
		"DISK_DEVICE":                          "/dev/vda",
		"INIT_URL":                             server.guestURL + "/status/init",
		"STATIC_IP":                            "10.0.2.15/24",
		"STATIC_GATEWAY":                       "10.0.2.2",
		"STATIC_IFACE":                         "eth0",
		"CRASH_ARTIFACTS_ENABLED":              "true",
		"CRASH_ARTIFACTS_PREPARE_URL":          server.guestURL + "/crash/prepare",
		"CRASH_ARTIFACTS_INCLUDE_MEMORY_DUMPS": "true",
	})

	kernel := findKernel(t)
	output := runQEMUCrashProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Provision output tail:\n%s", tail(output, 3000))

	events := server.eventsSnapshot()
	prepareIdx := indexOf(events, "prepare")
	uploadIdx := indexOf(events, "upload")
	imageIdx := indexOf(events, "image")
	if prepareIdx == -1 || uploadIdx == -1 || imageIdx == -1 {
		t.Fatalf("expected prepare, upload, and image events, got %v", events)
	}
	if !(prepareIdx < imageIdx && uploadIdx < imageIdx) {
		t.Fatalf("crash upload must happen before image download, got events %v", events)
	}

	entries := readKVMCrashArchive(t, server.uploadedBody())
	if _, ok := entries["target-root/var/crash/vmcore"]; !ok {
		t.Fatalf("uploaded archive missing old OS vmcore; entries=%v", kvmArchiveKeys(entries))
	}
	if _, ok := entries["metadata.json"]; !ok {
		t.Fatalf("uploaded archive missing metadata.json; entries=%v", kvmArchiveKeys(entries))
	}
	if logs := string(output); strings.Contains(logs, "X-Amz-Signature=test-secret") {
		t.Fatal("serial logs leaked presigned query string")
	}
}

func runQEMUCrashProvision(t *testing.T, kernel, initramfs, disk string, timeoutDur time.Duration) []byte {
	t.Helper()
	args := []string{
		"-m", "1024",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", runnerKernelCmdline + " net.ifnames=0",
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDur)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Logf("QEMU crash provision timed out after %v. tail:\n%s", timeoutDur, tail(out, 2000))
	} else if err != nil {
		t.Logf("QEMU crash provision exited: %v (expected on reboot)", err)
	}
	return out
}

type kvmCrashArtifactServer struct {
	guestURL string

	mu         sync.Mutex
	events     []string
	uploadBody []byte
}

func newKVMCrashArtifactServer(t *testing.T, imagePath string) *kvmCrashArtifactServer {
	t.Helper()
	state := &kvmCrashArtifactServer{}
	mux := http.NewServeMux()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	state.guestURL = fmt.Sprintf("http://10.0.2.2:%d", port)

	mux.HandleFunc("/status/init", func(w http.ResponseWriter, _ *http.Request) {
		state.record("init")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/image.gz", func(w http.ResponseWriter, r *http.Request) {
		state.record("image")
		http.ServeFile(w, r, imagePath)
	})
	mux.HandleFunc("/crash/prepare", func(w http.ResponseWriter, r *http.Request) {
		state.record("prepare")
		var req crash.PrepareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode prepare: %v", err)
		}
		if req.Manifest.Metadata.Machine.Hostname != "crash-kvm-node" {
			t.Fatalf("prepare hostname = %q", req.Manifest.Metadata.Machine.Hostname)
		}
		_ = json.NewEncoder(w).Encode(crash.PrepareResponse{
			UploadMode: crash.UploadModePresignedPUT,
			AuthMode:   crash.AuthModeNone,
			Method:     http.MethodPut,
			UploadURL:  state.guestURL + "/crash/upload?X-Amz-Signature=test-secret",
		})
	})
	mux.HandleFunc("/crash/upload", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		state.mu.Lock()
		state.events = append(state.events, "upload")
		state.uploadBody = body
		state.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return state
}

func (s *kvmCrashArtifactServer) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *kvmCrashArtifactServer) eventsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *kvmCrashArtifactServer) uploadedBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.uploadBody...)
}

func seedRawDiskCrashArtifacts(t *testing.T, rawDisk string) {
	t.Helper()
	out := runOutput(t, "attach existing raw loop", "losetup", "--find", "--show", "--partscan", rawDisk)
	loopDev := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loopDev).Run() })
	rootDev := loopDev + "p2"
	waitForDevice(t, rootDev, 5*time.Second)
	mountDir := filepath.Join(t.TempDir(), "existing-root")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatalf("mkdir mount dir: %v", err)
	}
	mountWithRetry(t, "mount existing root", rootDev, mountDir)
	defer run(t, "unmount existing root", "umount", mountDir)
	writeKVMFile(t, filepath.Join(mountDir, "var", "crash", "vmcore"), "old-os-vmcore")
	writeKVMFile(t, filepath.Join(mountDir, "var", "log", "kern.log"), "kernel panic - not syncing: kvm")
}

func writeKVMFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readKVMCrashArchive(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := make(map[string][]byte)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		entries[header.Name] = body
	}
	return entries
}

func kvmArchiveKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}
