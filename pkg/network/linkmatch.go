package network

import (
	"path"
	"strings"
)

// LinkMatch selects physical NICs like a netplan ethernets match block.
type LinkMatch struct {
	Name    string // interface name or shell glob such as "en*"; empty matches all
	MAC     string // MAC address; empty matches all
	SetName string // new name for the single matching NIC (netplan set-name)
}

// LinkCandidate is a physical NIC that a LinkMatch can select.
type LinkCandidate struct {
	Name string
	MAC  string
}

// Matches returns the names of the candidates that m selects.
func (m LinkMatch) Matches(links []LinkCandidate) []string {
	var names []string
	for _, l := range links {
		if m.Name != "" {
			if ok, err := path.Match(m.Name, l.Name); err != nil || !ok {
				continue
			}
		}
		if m.MAC != "" && !strings.EqualFold(m.MAC, l.MAC) {
			continue
		}
		names = append(names, l.Name)
	}
	return names
}
