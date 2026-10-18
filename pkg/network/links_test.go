//go:build linux

package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeLinks points the links mode at a fake /proc/sys and NIC list and
// records the NICs it sets up.
func fakeLinks(t *testing.T, nics []string, setUpErr error) (procSys string, up *[]string) {
	t.Helper()
	origProc, origDetect, origSetUp := linksProcSys, linksDetect, linksSetUp
	t.Cleanup(func() { linksProcSys, linksDetect, linksSetUp = origProc, origDetect, origSetUp })
	linksProcSys = t.TempDir()
	for _, nic := range nics {
		for _, key := range []string{"accept_ra", "autoconf"} {
			if err := os.MkdirAll(filepath.Join(linksProcSys, nic), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(linksProcSys, nic, key), []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	linksDetect = func(context.Context) ([]string, error) { return nics, nil }
	up = new([]string)
	linksSetUp = func(name string) error {
		*up = append(*up, name)
		return setUpErr
	}
	return linksProcSys, up
}

// Every physical NIC gets accept_ra and autoconf 0 (link-local only) and
// is set up; nothing else is configured.
func TestLinksModeSetup(t *testing.T) {
	nics := []string{"ens1f0np0", "ens1f1np1"}
	procSys, up := fakeLinks(t, nics, nil)
	if err := (&LinksMode{}).Setup(context.Background(), &Config{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*up, nics) {
		t.Fatalf("set up %v, want %v", *up, nics)
	}
	for _, nic := range nics {
		for _, key := range []string{"accept_ra", "autoconf"} {
			b, err := os.ReadFile(filepath.Join(procSys, nic, key))
			if err != nil || string(b) != "0" {
				t.Errorf("%s %s = %q (%v), want 0", nic, key, b, err)
			}
		}
	}
	if err := (&LinksMode{}).WaitForConnectivity(context.Background(), "http://192.0.2.1/x", 0); err != nil {
		t.Fatalf("WaitForConnectivity = %v", err)
	}
}

func TestLinksModeSetupErrors(t *testing.T) {
	fakeLinks(t, nil, nil)
	if err := (&LinksMode{}).Setup(context.Background(), &Config{}); err == nil {
		t.Fatal("Setup without NICs succeeded")
	}
	fakeLinks(t, []string{"eth1"}, errors.New("boom"))
	if err := (&LinksMode{}).Setup(context.Background(), &Config{}); err == nil {
		t.Fatal("Setup ignored a link that does not come up")
	}
}

func TestIsLinksMode(t *testing.T) {
	for _, m := range []string{"links", "LINKS"} {
		if !(&Config{NetworkMode: m}).IsLinksMode() {
			t.Errorf("%q is not links mode", m)
		}
	}
	if (&Config{NetworkMode: "dhcp"}).IsLinksMode() || (&Config{}).IsLinksMode() {
		t.Error("links mode without NETWORK_MODE=links")
	}
}

// A sysctl that cannot be set fails the setup before any NIC goes up; a
// NIC without an IPv6 configuration (IPv6 off) needs no sysctl.
func TestLinksModeSysctlFailure(t *testing.T) {
	procSys, up := fakeLinks(t, []string{"eth0"}, nil)
	if err := os.Remove(filepath.Join(procSys, "eth0", "autoconf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(procSys, "eth0", "autoconf"), 0o755); err != nil { // write fails
		t.Fatal(err)
	}
	if err := (&LinksMode{}).Setup(context.Background(), &Config{}); err == nil || len(*up) != 0 {
		t.Fatalf("Setup = %v, NICs up %v; want an error and none up", err, *up)
	}

	_, up = fakeLinks(t, []string{"eth1"}, nil)
	linksDetect = func(context.Context) ([]string, error) { return []string{"eth1", "noipv6"}, nil }
	if err := (&LinksMode{}).Setup(context.Background(), &Config{}); err != nil || !reflect.DeepEqual(*up, []string{"eth1", "noipv6"}) {
		t.Fatalf("NIC without IPv6: %v, up %v", err, *up)
	}
}
