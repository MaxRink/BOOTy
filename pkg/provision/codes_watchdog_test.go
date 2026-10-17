//go:build linux

package provision

import "testing"

func TestWatchdogStallCodeIsInTheCatalog(t *testing.T) {
	for _, c := range FailureCodes() {
		if c.Code == CodeStalled {
			return
		}
	}
	t.Fatalf("%s is not in the catalog", CodeStalled)
}
