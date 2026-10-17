//go:build linux && linux_e2e

package sriov

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// netdevsim is a netdevsim device: a software NIC with devlink, an eSwitch
// (legacy and switchdev), SR-IOV VFs and the rtnetlink VF attributes. The
// kernel module is in linux-modules-extra on the Ubuntu runners.
type netdevsim struct {
	id  int
	dir string // /sys/bus/netdevsim/devices/netdevsim<id>
}

const netdevsimBus = "/sys/bus/netdevsim"

// physPortRe matches the phys_port_name of a netdevsim PF port. The port
// numbers are not always 0 and 1.
var physPortRe = regexp.MustCompile(`^p\d+$`)

// newNetdevsim creates a netdevsim device with ports ports in the current
// network namespace and removes it at the end of the test.
func newNetdevsim(t *testing.T, id, ports int) *netdevsim {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	if _, err := os.Stat(netdevsimBus); err != nil {
		t.Fatalf("netdevsim not loaded (modprobe netdevsim): %v", err)
	}
	writeSys(t, filepath.Join(netdevsimBus, "new_device"), fmt.Sprintf("%d %d", id, ports))
	n := &netdevsim{id: id, dir: filepath.Join(netdevsimBus, "devices", "netdevsim"+strconv.Itoa(id))}
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(netdevsimBus, "del_device"), []byte(strconv.Itoa(id)), 0o200); err != nil {
			t.Errorf("delete netdevsim%d: %v", id, err)
		}
	})
	n.waitPorts(t, ports)
	settleUdev(t)
	return n
}

// settleUdev waits until udev has handled the new netdevs, so it does not
// rename a port while a test reads it. Hosts without udev skip this.
func settleUdev(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("udevadm"); err != nil {
		return
	}
	if out, err := exec.CommandContext(t.Context(), "udevadm", "settle", "--timeout=10").CombinedOutput(); err != nil {
		t.Logf("udevadm settle: %v: %s", err, out)
	}
}

// waitPorts waits until the device has want port netdevs (udev may rename
// them while the test starts).
func (n *netdevsim) waitPorts(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		names := n.ports()
		if len(names) == want {
			return names
		}
		select {
		case <-deadline:
			t.Fatalf("netdevsim%d has ports %v, want %d", n.id, names, want)
		case <-tick.C:
		}
	}
}

// ports lists the netdevs of the device that are PF ports (p0, p1, ...),
// not representors.
func (n *netdevsim) ports() []string {
	entries, err := os.ReadDir(filepath.Join(n.dir, "net"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !representorName.MatchString(readTrim(filepath.Join(n.dir, "net", e.Name(), "phys_port_name"))) {
			names = append(names, e.Name())
		}
	}
	return names
}

func (n *netdevsim) name() string { return "netdevsim" + strconv.Itoa(n.id) }

// setMaxVFs raises the VF limit through debugfs, where netdevsim keeps it.
func (n *netdevsim) setMaxVFs(t *testing.T, max int) {
	t.Helper()
	path := filepath.Join("/sys/kernel/debug/netdevsim", n.name(), "max_vfs")
	if _, err := os.Stat(path); err != nil {
		t.Logf("no %s, keeping the default VF limit: %v", path, err)
		return
	}
	writeSys(t, path, strconv.Itoa(max))
}

func writeSys(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o200); err != nil {
		t.Fatalf("write %q to %s: %v", value, path, err)
	}
}

func netdevsimPFs(t *testing.T, n *netdevsim) []PF {
	t.Helper()
	all, err := NewInspector().Discover()
	if err != nil {
		t.Fatal(err)
	}
	var pfs []PF
	for _, pf := range all {
		if pf.Bus == "netdevsim" && pf.PCIAddress == n.name() {
			pfs = append(pfs, pf)
		}
	}
	return pfs
}

// waitPFs waits until Discover reports want PF ports of the device: udev
// can rename a port while Discover reads it.
func waitPFs(t *testing.T, n *netdevsim, want int) []PF {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		pfs := netdevsimPFs(t, n)
		if len(pfs) == want {
			return pfs
		}
		select {
		case <-deadline:
			t.Fatalf("netdevsim PF ports = %+v, want %d", pfs, want)
		case <-tick.C:
		}
	}
}

// TestNetdevsimDiscover reads a two-port netdevsim device with two VFs
// through sysfs, devlink and rtnetlink of the running kernel.
func TestNetdevsimDiscover(t *testing.T) {
	n := newNetdevsim(t, 4710, 2)
	n.setMaxVFs(t, 8)
	writeSys(t, filepath.Join(n.dir, "sriov_numvfs"), "2")

	pfs := waitPFs(t, n, 2)
	for _, pf := range pfs {
		if pf.Driver != "netdevsim" || pf.PortCount != 2 || pf.NumVFs != 2 || pf.LinkType != LinkTypeEthernet {
			t.Errorf("%s: driver %q ports %d numVFs %d linkType %q", pf.Interface, pf.Driver, pf.PortCount, pf.NumVFs, pf.LinkType)
		}
		if pf.Eswitch.Capability != CapSupported || pf.Eswitch.Mode != EswitchLegacy {
			t.Errorf("%s: eswitch %+v, want supported legacy", pf.Interface, pf.Eswitch)
		}
		if !physPortRe.MatchString(pf.PhysPortName) {
			t.Errorf("%s: phys_port_name %q", pf.Interface, pf.PhysPortName)
		}
		if pf.PhysSwitchID == "" {
			t.Errorf("%s: no phys_switch_id", pf.Interface)
		}
		if pf.Devlink == nil || pf.Devlink.Handle != "netdevsim/"+n.name() || pf.Devlink.Driver != "netdevsim" {
			t.Errorf("%s: devlink info %+v", pf.Interface, pf.Devlink)
		}
		if pf.NUMANode != -1 {
			t.Errorf("%s: NUMA node %d, want -1", pf.Interface, pf.NUMANode)
		}
	}
	// rtnetlink lists the VFs of the device on the port netdevs.
	var vfs int
	for _, pf := range pfs {
		vfs += len(pf.VFs)
	}
	if vfs < 2 {
		t.Fatalf("rtnetlink VFs: %+v", pfs)
	}
}
