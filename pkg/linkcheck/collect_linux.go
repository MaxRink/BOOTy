//go:build linux

package linkcheck

import (
	"net"
	"os"
	"path/filepath"

	"github.com/telekom/BOOTy/pkg/network/ethtool"
	"github.com/telekom/BOOTy/pkg/network/lldp"
)

// sysClassNet is /sys/class/net; a test seam.
var sysClassNet = "/sys/class/net"

// collect returns what BOOTy knows about each NIC.
func collect(nics []string) []Link {
	neighbors := lldp.Neighbors()
	links := make([]Link, 0, len(nics))
	for _, nic := range nics {
		l := Link{Ifname: nic, Driver: ethtool.Driver(nic), State: linkState(nic), PCIe: pcie(nic), Module: module(nic)}
		if ifi, err := net.InterfaceByName(nic); err == nil {
			l.MAC = ifi.HardwareAddr.String()
			l.Carrier = ifi.Flags&net.FlagRunning != 0
		}
		if dev, err := os.Readlink(filepath.Join(sysClassNet, nic, "device")); err == nil {
			l.PCI = filepath.Base(dev)
		}
		for i := range neighbors {
			if neighbors[i].Interface == nic {
				l.LLDP = &neighbors[i]
			}
		}
		links = append(links, l)
	}
	return links
}
