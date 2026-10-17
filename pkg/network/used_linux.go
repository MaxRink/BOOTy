//go:build linux

package network

import (
	"net"
	"path"
	"slices"
	"strings"
	"sync/atomic"
)

// UsedInterfaces are the interfaces that the ramdisk network uses, as its
// stacks pick them: the configured names (static interface, bond members,
// VLAN parents, BGP interfaces, netplan ethernets, frr.conf unnumbered
// peers, set-name targets) with globs and auto-detect resolved against the
// links of the host, plus the interfaces of the unnumbered BGP sessions.
// AllPhysical is true when the BGP stack peers on every physical NIC.
type UsedInterfaces struct {
	Names       []string
	AllPhysical bool
}

// Seams of UsedInterfacesOf; tests replace them.
var (
	linkNames = func() []string {
		ifs, err := net.Interfaces()
		if err != nil {
			return nil
		}
		names := make([]string, 0, len(ifs))
		for i := range ifs {
			names = append(names, ifs[i].Name)
		}
		return names
	}
	physicalNICs = func() []string {
		nics, err := DetectPhysicalNICs()
		if err != nil {
			return nil
		}
		return nics
	}
)

// UsedInterfacesOf returns the interfaces that the network of c uses.
//
//   - GoBGP peers on the interfaces of its unnumbered sessions, else on
//     BGP_INTERFACES, else on every physical NIC (UnderlayTier.waitForNICs).
//   - The FRR stack peers on every physical NIC, also with BGP_INTERFACES.
//   - The static stack without an interface takes the first physical NIC.
func UsedInterfacesOf(c *Config) UsedInterfaces {
	var sessions []string
	for i := range c.BGPSessions {
		if s := &c.BGPSessions[i]; s.Interface && s.Name != "" {
			sessions = append(sessions, s.Name)
		}
	}
	raw := append(rawInterfaceNames(c), sessions...)
	if c.IsStaticMode() && (c.StaticIface == "" || c.StaticIface == "auto-detect") {
		if nics := physicalNICs(); len(nics) > 0 {
			raw = append(raw, nics[0])
		}
	}
	u := UsedInterfaces{Names: resolveInterfaceNames(raw)}
	switch {
	case c.IsGoBGPMode():
		u.AllPhysical = len(sessions) == 0 && strings.Trim(c.BGPInterfaces, ", ") == ""
	case c.IsFRRMode():
		u.AllPhysical = true
	}
	return u
}

// rawInterfaceNames is ConfiguredInterfaceNames with the globs and the
// auto-detect markers kept.
func rawInterfaceNames(c *Config) []string {
	names := append([]string{c.StaticIface, c.VXLANLink}, c.Interfaces...)
	names = append(names, c.InterfaceNames...)
	names = append(names, strings.Split(c.BondInterfaces, ",")...)
	names = append(names, strings.Split(c.BGPInterfaces, ",")...)
	for _, v := range c.VLANs {
		names = append(names, v.Parent)
	}
	for _, m := range c.LinkRenames {
		names = append(names, m.Name)
	}
	if c.StaticMatch != nil {
		names = append(names, c.StaticMatch.Name)
	}
	return names
}

// resolveInterfaceNames drops empty and auto-detect names, keeps literal
// names, and expands shell patterns to the matching links of the host.
func resolveInterfaceNames(raw []string) []string {
	var out []string
	add := func(n string) {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, n := range raw {
		n = strings.TrimSpace(n)
		switch {
		case n == "" || n == "auto-detect":
		case strings.ContainsAny(n, "*?["):
			for _, l := range linkNames() {
				if ok, err := path.Match(n, l); err == nil && ok {
					add(l)
				}
			}
		default:
			add(n)
		}
	}
	return out
}

var activeInterfaces atomic.Pointer[UsedInterfaces]

// RecordUsedInterfaces remembers the interfaces of the active ramdisk
// network, for the checks that must not disturb it (SR-IOV).
func RecordUsedInterfaces(c *Config) {
	u := UsedInterfacesOf(c)
	activeInterfaces.Store(&u)
}

// ActiveInterfaces returns the recorded interfaces; known is false before
// the network setup recorded them.
func ActiveInterfaces() (u UsedInterfaces, known bool) {
	p := activeInterfaces.Load()
	if p == nil {
		return UsedInterfaces{}, false
	}
	return *p, true
}
