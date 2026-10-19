//go:build linux

package image

import (
	"context"
	"fmt"
)

// ValidateStreamingPrerequisites probes the image source before the
// destructive disk steps and returns its format. A qcow2 URL is not probed:
// BOOTy converts qcow2 in process and needs no tool for it.
func ValidateStreamingPrerequisites(ctx context.Context, source string) (Format, error) {
	if sourceLooksQCOW2(source) {
		return FormatQCOW2, nil
	}
	format, err := ProbeSourceFormat(ctx, source)
	if err != nil {
		return FormatRaw, fmt.Errorf("%w: %w", ErrFormatProbe, err)
	}
	return format, nil
}
