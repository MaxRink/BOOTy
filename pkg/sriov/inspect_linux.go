//go:build linux

package sriov

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// representorName matches the phys_port_name of VF and SF representors
// (pf0vf3, c1pf0sf2). Representors hang off the PF device too, but they are
// not PF ports.
var representorName = regexp.MustCompile(`^(c\d+)?pf\d+(vf|sf)\d+$`)

// Inspector reads the SR-IOV state of the PF ports of a host.
type Inspector struct {
	// SysNet is the /sys/class/net directory.
	SysNet string
	// Devlink answers the devlink queries.
	Devlink Devlink
	// VFs returns the VF list of a PF netdev from rtnetlink.
	VFs func(iface string) ([]netlink.VfInfo, error)
	// Drvinfo returns the driver and the firmware version from ethtool.
	Drvinfo func(iface string) (driver, firmware string, err error)
}

// NewInspector returns an Inspector for the running kernel.
func NewInspector() *Inspector {
	return &Inspector{
		SysNet:  "/sys/class/net",
		Devlink: NetlinkDevlink{},
		VFs:     linkVFs,
		Drvinfo: ethtoolDrvinfo,
	}
}

// Discover reports every PF port, sorted by interface name. A read that
// fails leaves its fields empty and adds a warning to the PF; Discover
// fails only when SysNet cannot be listed.
func (in *Inspector) Discover() ([]PF, error) {
	entries, err := os.ReadDir(in.SysNet)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", in.SysNet, err)
	}
	var pfs []PF
	for _, e := range entries {
		if in.isPFPort(e.Name()) {
			pfs = append(pfs, in.inspect(e.Name()))
		}
	}
	sort.Slice(pfs, func(i, j int) bool { return pfs[i].Interface < pfs[j].Interface })
	groupPorts(pfs)
	return pfs, nil
}

// isPFPort reports whether a netdev is a port of a physical function: it
// has a parent device, the device is not a VF, and it is not a representor.
func (in *Inspector) isPFPort(name string) bool {
	dev := filepath.Join(in.SysNet, name, "device")
	if _, err := os.Stat(dev); err != nil {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dev, "physfn")); err == nil {
		return false
	}
	return !representorName.MatchString(readTrim(filepath.Join(in.SysNet, name, "phys_port_name")))
}

func (in *Inspector) inspect(name string) PF {
	netDir := filepath.Join(in.SysNet, name)
	dev := filepath.Join(netDir, "device")
	pf := PF{
		Interface:    name,
		MAC:          readTrim(filepath.Join(netDir, "address")),
		Bus:          linkBase(filepath.Join(dev, "subsystem")),
		PCIAddress:   linkBase(dev),
		VendorID:     pciID(readTrim(filepath.Join(dev, "vendor"))),
		DeviceID:     pciID(readTrim(filepath.Join(dev, "device"))),
		Driver:       linkBase(filepath.Join(dev, "driver")),
		NUMANode:     readInt(filepath.Join(dev, "numa_node"), -1),
		LinkType:     linkType(readTrim(filepath.Join(netDir, "type"))),
		TotalVFs:     readInt(filepath.Join(dev, "sriov_totalvfs"), 0),
		NumVFs:       readInt(filepath.Join(dev, "sriov_numvfs"), 0),
		PhysSwitchID: readTrim(filepath.Join(netDir, "phys_switch_id")),
		PhysPortName: readTrim(filepath.Join(netDir, "phys_port_name")),
		Eswitch:      Eswitch{Capability: CapUnknown},
		VFLAG:        CapUnknown,
	}
	pf.SRIOVCapable = pf.TotalVFs > 0
	in.readFirmware(&pf)
	in.readDevlink(&pf)
	in.readVFs(&pf, dev)
	return pf
}

func (in *Inspector) readFirmware(pf *PF) {
	if in.Drvinfo == nil {
		return
	}
	_, fw, err := in.Drvinfo(pf.Interface)
	if err != nil {
		pf.Warnings = append(pf.Warnings, "ethtool drvinfo: "+err.Error())
		return
	}
	pf.Firmware = fw
}

func (in *Inspector) readDevlink(pf *PF) {
	if in.Devlink == nil || pf.Bus == "" || pf.PCIAddress == "" {
		return
	}
	esw, err := in.Devlink.Eswitch(pf.Bus, pf.PCIAddress)
	switch {
	case err == nil:
		pf.Eswitch = esw
	case errors.Is(err, ErrNotSupported):
		pf.Eswitch = Eswitch{Capability: CapUnsupported}
	default:
		pf.Warnings = append(pf.Warnings, "devlink eswitch: "+err.Error())
	}
	info, err := in.Devlink.Info(pf.Bus, pf.PCIAddress)
	switch {
	case err == nil:
		pf.Devlink = info
		if pf.Firmware == "" {
			pf.Firmware = runningFirmware(info)
		}
	case !errors.Is(err, ErrNotSupported):
		pf.Warnings = append(pf.Warnings, "devlink info: "+err.Error())
	}
}

// runningFirmware picks the main firmware version from the devlink running
// versions: "fw" (generic) or "fw.version" (mlx5 before the generic name).
func runningFirmware(info *DevlinkInfo) string {
	for _, name := range []string{"fw", "fw.version", "fw.mgmt"} {
		for _, v := range info.Running {
			if v.Name == name {
				return v.Value
			}
		}
	}
	return ""
}

func (in *Inspector) readVFs(pf *PF, dev string) {
	if pf.NumVFs == 0 || in.VFs == nil {
		return
	}
	infos, err := in.VFs(pf.Interface)
	if err != nil {
		pf.Warnings = append(pf.Warnings, "rtnetlink VF list: "+err.Error())
		return
	}
	for i := range infos {
		v := &infos[i]
		vf := VF{
			Index:      v.ID,
			PCIAddress: linkBase(filepath.Join(dev, "virtfn"+strconv.Itoa(v.ID))),
			VLAN:       v.Vlan,
			QoS:        v.Qos,
			MinTxRate:  v.MinTxRate,
			MaxTxRate:  v.MaxTxRate,
			SpoofChk:   v.Spoofchk,
			Trust:      v.Trust != 0,
			LinkState:  vfLinkState(v.LinkState),
		}
		if len(v.Mac) > 0 {
			vf.MAC = v.Mac.String()
		}
		pf.VFs = append(pf.VFs, vf)
	}
}

func vfLinkState(s uint32) string {
	switch s {
	case netlink.VF_LINK_STATE_AUTO:
		return "auto"
	case netlink.VF_LINK_STATE_ENABLE:
		return "enable"
	case netlink.VF_LINK_STATE_DISABLE:
		return "disable"
	}
	return "state-" + strconv.FormatUint(uint64(s), 10)
}

func linkVFs(iface string) ([]netlink.VfInfo, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("link %s: %w", iface, err)
	}
	return link.Attrs().Vfs, nil
}

func ethtoolDrvinfo(iface string) (driver, firmware string, err error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return "", "", fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck // read-only socket
	info, err := unix.IoctlGetEthtoolDrvinfo(fd, iface)
	if err != nil {
		return "", "", fmt.Errorf("ETHTOOL_GDRVINFO %s: %w", iface, err)
	}
	return unix.ByteSliceToString(info.Driver[:]), unix.ByteSliceToString(info.Fw_version[:]), nil
}

func readTrim(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // sysfs path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readInt(path string, fallback int) int {
	n, err := strconv.Atoi(readTrim(path))
	if err != nil {
		return fallback
	}
	return n
}

func linkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// pciID turns a sysfs ID such as 0x15B3 into 15b3.
func pciID(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"))
}

func linkType(arphrd string) string {
	switch arphrd {
	case "":
		return ""
	case strconv.Itoa(unix.ARPHRD_ETHER):
		return LinkTypeEthernet
	case strconv.Itoa(unix.ARPHRD_INFINIBAND):
		return LinkTypeInfiniband
	}
	return "arphrd-" + arphrd
}
