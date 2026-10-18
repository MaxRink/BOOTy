//go:build linux

package provision

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/image"
)

// A probe that keeps failing with a transient error stops the run before
// the wipe with the format probe code; one that recovers lets it go on.
func TestImageProbeFailsClosedBeforeTheWipe(t *testing.T) {
	origProbe, origDelay := validateStreamingPrerequisites, probeRetryDelay
	t.Cleanup(func() { validateStreamingPrerequisites, probeRetryDelay = origProbe, origDelay })
	probeRetryDelay = time.Millisecond
	transient := fmt.Errorf("%w: %w", image.ErrFormatProbe, context.DeadlineExceeded)

	for _, tt := range []struct {
		name     string
		failures int
		wantErr  bool
	}{
		{name: "transient error every time", failures: probeAttempts, wantErr: true},
		{name: "transient error, then the probe works", failures: probeAttempts - 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			validateStreamingPrerequisites = func(context.Context, string) (image.Format, error) {
				calls++
				if calls <= tt.failures {
					return image.FormatRaw, transient
				}
				return image.FormatRaw, nil
			}
			o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
			o.bestImageURL = "https://img.example/os.img"
			err := o.validateImageStreamingPrerequisites(context.Background(), []string{o.bestImageURL})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if code := CodeOf(WithCode("validate-provision-inputs", err)); code != codeFormatProbe {
					t.Fatalf("code = %q, want %s", code, codeFormatProbe)
				}
				if calls != probeAttempts {
					t.Fatalf("probe calls = %d, want %d", calls, probeAttempts)
				}
			}
		})
	}
}
