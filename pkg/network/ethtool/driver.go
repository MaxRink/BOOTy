//go:build linux

package ethtool

import (
	"os"
	"path/filepath"
)

// sysClassNet is /sys/class/net; a test seam.
var sysClassNet = "/sys/class/net"

// Driver returns the kernel driver of a NIC, "" for a virtual one.
func Driver(ifname string) string {
	link, err := os.Readlink(filepath.Join(sysClassNet, ifname, "device", "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}
