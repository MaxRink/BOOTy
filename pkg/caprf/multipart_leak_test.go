package caprf

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/crash"
)

func TestCrashFormUploadClosesReaderWhenRequestIsNotSent(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "crash.tar.gz")
	// Larger than the pipe buffer, so the writer goroutine blocks until the
	// reader is closed.
	if err := os.WriteFile(archive, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewFromConfig(&config.MachineConfig{})
	// Bearer auth is only allowed for proxy uploads, so applyCrashUploadAuth
	// fails and the request is never sent.
	instructions := &crash.PrepareResponse{
		UploadURL:  "https://example.invalid/upload",
		UploadMode: crash.UploadModePresignedPOST,
		AuthMode:   crash.AuthModeBearer,
	}

	origStart, origExit := multipartWriterStarted, multipartWriterExited
	t.Cleanup(func() { multipartWriterStarted, multipartWriterExited = origStart, origExit })
	var started atomic.Int32
	exited := make(chan struct{}, 16)
	multipartWriterStarted = func() { started.Add(1) }
	multipartWriterExited = func() { exited <- struct{}{} }

	if err := c.uploadCrashPresignedForm(context.Background(), instructions, archive); err == nil {
		t.Fatal("expected an auth mode error")
	}
	timeout := time.After(5 * time.Second)
	for i := int32(0); i < started.Load(); i++ {
		select {
		case <-exited:
		case <-timeout:
			t.Fatalf("multipart writer leaked: %d of %d writers exited", i, started.Load())
		}
	}
}
