//go:build linux

package network

import (
	"errors"
	"testing"

	"github.com/vishvananda/netlink"
)

// An interrupted dump is repeated; other errors and a clean dump return
// at once; the interruption is returned when it does not stop.
func TestRetryDump(t *testing.T) {
	for _, tt := range []struct {
		name  string
		errs  []error
		calls int
		want  error
	}{
		{"interrupted twice", []error{netlink.ErrDumpInterrupted, netlink.ErrDumpInterrupted, nil}, 3, nil},
		{"other error", []error{errors.New("boom")}, 1, errors.New("boom")},
		{"always interrupted", []error{netlink.ErrDumpInterrupted}, dumpRetries, netlink.ErrDumpInterrupted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			out, err := RetryDump(func() ([]int, error) {
				e := tt.errs[min(calls, len(tt.errs)-1)]
				calls++
				return []int{calls}, e
			})
			if calls != tt.calls || (err == nil) != (tt.want == nil) || len(out) != 1 {
				t.Fatalf("calls %d err %v out %v; want %d calls, err %v", calls, err, out, tt.calls, tt.want)
			}
		})
	}
}
