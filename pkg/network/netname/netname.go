// Package netname computes the predictable network interface names that
// systemd-udevd's net_id builtin gives Ethernet links (naming scheme v255,
// Ubuntu 24.04), from sysfs. The ramdisk has no udev, so its links keep the
// kernel names (eth0), while the netplan and frr.conf of the installed OS
// use the predictable names (enp1s0, ens1f0np0).
package netname

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// maxOnboardIndex is the largest firmware index net_id accepts
// (NAMING_16BIT_INDEX).
const maxOnboardIndex = 65535

// Names returns the predictable names of the Ethernet link ifname in the
// default NamePolicy order: onboard (eno), slot (ens), path (enp). The first
// one is the name systemd gives the link; the others are its alternative
// names. Links that are not PCI Ethernet devices have no names.
func Names(sysfs, ifname string) []string {
	netDir := filepath.Join(sysfs, "class", "net", ifname)
	if readTrim(filepath.Join(netDir, "type")) != "1" {
		return nil // not ARPHRD_ETHER
	}
	dev, err := filepath.EvalSymlinks(filepath.Join(netDir, "device"))
	if err != nil {
		return nil // virtual link
	}
	if strings.HasPrefix(filepath.Base(dev), "virtio") {
		dev = filepath.Dir(dev) // virtio_net sits below its PCI device
	}
	addr, ok := parsePCIAddress(filepath.Base(dev))
	if !ok {
		return nil
	}
	if fileExists(filepath.Join(dev, "physfn")) {
		return nil // SR-IOV VF: systemd names it after its PF, not needed here
	}

	suffix := portSuffix(netDir)
	domain := ""
	if addr.domain > 0 {
		domain = fmt.Sprintf("P%d", addr.domain)
	}
	function := addr.function
	if readTrim(filepath.Join(dev, "ari_enabled")) == "1" {
		// ARI (the NIC's own ari_enabled, like is_pci_ari_enabled): the
		// device number is part of the function number; net_id v255 keeps
		// the slot number in the path name.
		function += addr.device * 8
	}
	multifunction := isMultifunction(sysfs, addr)
	fn := ""
	if function > 0 || multifunction {
		fn = fmt.Sprintf("f%d", function)
	}

	var names []string
	if index, ok := onboardIndex(dev); ok {
		names = append(names, fmt.Sprintf("eno%d%s", index, suffix))
	}
	if slot := hotplugSlot(sysfs, dev, multifunction); slot > 0 {
		names = append(names, fmt.Sprintf("en%ss%d%s%s", domain, slot, fn, suffix))
	}
	names = append(names, fmt.Sprintf("en%sp%ds%d%s%s", domain, addr.bus, addr.device, fn, suffix))
	return names
}

type pciAddress struct {
	domain, bus, device, function int
}

// parsePCIAddress parses a PCI device name such as 0000:01:00.0.
func parsePCIAddress(s string) (pciAddress, bool) {
	var a pciAddress
	if n, err := fmt.Sscanf(s, "%x:%x:%x.%x", &a.domain, &a.bus, &a.device, &a.function); err != nil || n != 4 {
		return a, false
	}
	return a, true
}

func (a pciAddress) slotPrefix() string {
	return fmt.Sprintf("%04x:%02x:%02x", a.domain, a.bus, a.device)
}

// switchdevVF matches a switchdev VF representor port name (pf0vf3).
var switchdevVF = regexp.MustCompile(`^pf\d+vf(\d+)$`)

// portSuffix is r<vf> for a switchdev VF representor, n<phys_port_name>,
// or d<dev_port> for a non-zero dev_port.
func portSuffix(netDir string) string {
	if name := readTrim(filepath.Join(netDir, "phys_port_name")); name != "" {
		if m := switchdevVF.FindStringSubmatch(name); m != nil {
			return "r" + m[1]
		}
		return "n" + name
	}
	if port, err := strconv.Atoi(readTrim(filepath.Join(netDir, "dev_port"))); err == nil && port > 0 {
		return fmt.Sprintf("d%d", port)
	}
	return ""
}

// onboardIndex is the firmware index of an onboard device: ACPI _DSM
// (acpi_index, 0 is valid) if the file exists, else SMBIOS type 41
// (index). Indexes above maxOnboardIndex are ignored.
func onboardIndex(dev string) (int, bool) {
	for _, f := range []string{"acpi_index", "index"} {
		v := readTrim(filepath.Join(dev, f))
		if v == "" {
			continue
		}
		i, err := strconv.Atoi(v)
		if err != nil || i < 0 || i > maxOnboardIndex {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// hotplugSlot returns the number of the PCI hotplug slot of the device,
// like net_id v255. Slot directories that are not a positive number ("0",
// "0-1") are ignored, so QEMU q35 root ports give path names (enp1s0).
// When no slot matches the device, net_id looks at the bridges above it;
// a bridge slot names the device only when it is multifunction
// (NAMING_BRIDGE_MULTIFUNCTION_SLOT).
func hotplugSlot(sysfs, dev string, multifunction bool) int {
	slotsDir := filepath.Join(sysfs, "bus", "pci", "slots")
	entries, err := os.ReadDir(slotsDir)
	if err != nil {
		return 0
	}
	for d := dev; ; d = filepath.Dir(d) {
		if _, ok := parsePCIAddress(filepath.Base(d)); !ok {
			return 0
		}
		for _, e := range entries {
			slot, err := strconv.Atoi(e.Name())
			if err != nil || slot <= 0 {
				continue
			}
			// startswith(sysname, address): a slot with number 0xff has the
			// address dddd:bb.
			address := readTrim(filepath.Join(slotsDir, e.Name(), "address"))
			if address == "" || !strings.HasPrefix(filepath.Base(d), address) {
				continue
			}
			if d != dev && !multifunction {
				return 0
			}
			return slot
		}
	}
}

// isMultifunction reports whether the PCI device has more than one
// function, from the multifunction bit of the configuration header type.
func isMultifunction(sysfs string, a pciAddress) bool {
	cfg, err := os.ReadFile(filepath.Join(sysfs, "bus", "pci", "devices",
		fmt.Sprintf("%s.%x", a.slotPrefix(), a.function), "config"))
	if err == nil && len(cfg) > 0x0e {
		return cfg[0x0e]&0x80 != 0
	}
	// Without a readable header, look for sibling functions.
	siblings, _ := filepath.Glob(filepath.Join(sysfs, "bus", "pci", "devices", a.slotPrefix()+".*"))
	return len(siblings) > 1
}

// Rename is one planned rename.
type Rename struct {
	From, To string
}

// Plan maps kernel link names to the wanted names that are one of their
// predictable names. A wanted name that two links compute is ambiguous and
// skipped with a warning. A link that computes two wanted names gets the
// first one; the other is logged.
func Plan(sysfs string, links, wanted []string) []Rename {
	var renames []Rename
	for _, want := range wanted {
		if want == "" || slices.Contains(links, want) {
			continue
		}
		var owners []string
		for _, link := range links {
			if slices.Contains(Names(sysfs, link), want) {
				owners = append(owners, link)
			}
		}
		switch {
		case len(owners) == 0:
			continue
		case len(owners) > 1:
			slog.Warn("several NICs have this predictable name, not renaming", "name", want, "nics", owners)
			continue
		}
		if i := slices.IndexFunc(renames, func(r Rename) bool { return r.From == owners[0] }); i >= 0 {
			if renames[i].To != want { // a duplicate wanted name is not a conflict
				slog.Warn("NIC already gets another configured name", "nic", owners[0], "name", renames[i].To, "also", want)
			}
			continue
		}
		renames = append(renames, Rename{From: owners[0], To: want})
	}
	return renames
}

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
