package loopdev

import "testing"

func TestSplitPartition(t *testing.T) {
	for path, want := range map[string]struct {
		dev  string
		part int
		ok   bool
	}{
		"/dev/loop3p2":  {dev: "/dev/loop3", part: 2, ok: true},
		"/dev/loop12p1": {dev: "/dev/loop12", part: 1, ok: true},
		"/dev/loop3":    {},
		"/dev/nbd0p1":   {},
		"/dev/sda2":     {},
	} {
		dev, part, ok := SplitPartition(path)
		if dev != want.dev || part != want.part || ok != want.ok {
			t.Errorf("SplitPartition(%q) = %q, %d, %v; want %q, %d, %v", path, dev, part, ok, want.dev, want.part, want.ok)
		}
	}
}
