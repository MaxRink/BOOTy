//go:build e2e

package kvm

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Legacy evidence: provision.sh pick_best_source probes every IMAGE source
// with `curl --range 0-0` (a GET) and streams from the fastest one. A
// presigned source that rejects HEAD must still win when it is fastest.
func TestLegacyCompatPresignedImageSourceQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	imagePath := compressGzip(t, createChrootCapableTestDiskImage(t, 512))
	caprf := startFakeCAPRF(t, imagePath)

	var presignedDownloads, slowDownloads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/presigned/image.gz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden) // bound to GET, like a presigned S3 URL
			return
		}
		if r.Header.Get("Range") == "" {
			presignedDownloads.Add(1)
		}
		http.ServeFile(w, r, imagePath)
	})
	mux.HandleFunc("/slow/image.gz", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
			slowDownloads.Add(1)
		}
		http.ServeFile(w, r, imagePath)
	})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	base := fmt.Sprintf("http://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)

	targetDisk := filepath.Join(t.TempDir(), "legacy-image-source.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = base + "/slow/image.gz " + base + "/presigned/image.gz?X-Amz-Signature=test"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("image source output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	if presignedDownloads.Load() == 0 || slowDownloads.Load() != 0 {
		t.Fatalf("downloads presigned=%d slow=%d, want the image from the faster presigned source",
			presignedDownloads.Load(), slowDownloads.Load())
	}
}
