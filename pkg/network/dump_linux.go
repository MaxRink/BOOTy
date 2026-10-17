//go:build linux

package network

import (
	"errors"

	"github.com/vishvananda/netlink"
)

// dumpRetries bounds the repeats of an interrupted netlink dump.
const dumpRetries = 5

// RetryDump repeats a netlink dump that the kernel interrupted because the
// table changed meanwhile (NLM_F_DUMP_INTR, netlink.ErrDumpInterrupted).
// That happens while links come and go, for example while containerlab
// moves the veths into the namespace or a driver brings up its ports; the
// result of an interrupted dump may be incomplete.
func RetryDump[T any](dump func() ([]T, error)) ([]T, error) {
	var out []T
	var err error
	for range dumpRetries {
		out, err = dump()
		if !errors.Is(err, netlink.ErrDumpInterrupted) {
			return out, err
		}
	}
	return out, err
}
