package netplan

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The production network files (bm4x-operator templates rendered with
// documentation addresses, see pkg/network/frrconf/testdata) must parse
// without errors, and the derived network.Config must stay as reviewed.
func TestToNetworkConfigGolden(t *testing.T) {
	dirs, err := filepath.Glob("../frrconf/testdata/*/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "netplan.yaml")); err != nil {
			continue
		}
		name := filepath.Base(filepath.Dir(dir)) + "-" + filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			np, err := ParseFile(filepath.Join(dir, "netplan.yaml"))
			if err != nil {
				t.Fatalf("ParseFile() = %v", err)
			}
			golden := filepath.Join("testdata", name+".golden.json")
			var got []byte
			frr, err := ParseFRRConfig(filepath.Join(dir, "frr.conf"))
			if err != nil {
				// A config that BOOTy can not run yet: the error is the
				// golden result.
				golden = filepath.Join("testdata", name+".error.golden")
				got = []byte(err.Error())
			} else if got, err = json.MarshalIndent(ToNetworkConfig(np, frr), "", "  "); err != nil {
				t.Fatal(err)
			}
			if *update {
				if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden file (run with -update): %v", err)
			}
			if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
				t.Fatalf("network config differs from %s (run with -update and review the diff):\n%s", golden, got)
			}
		})
	}
}

// netplan rejects unknown keys, so BOOTy does too.
func TestParseFileRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(path, []byte("network:\n  version: 2\n  ethernets:\n    eth0:\n      wakeonlan: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path); err == nil || !strings.Contains(err.Error(), "wakeonlan") {
		t.Fatalf("ParseFile() = %v, want an unknown key error", err)
	}
}
