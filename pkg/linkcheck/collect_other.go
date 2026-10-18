//go:build !linux

package linkcheck

// collect only names the NICs: BOOTy reads them on Linux only.
func collect(nics []string) []Link {
	links := make([]Link, 0, len(nics))
	for _, nic := range nics {
		links = append(links, Link{Ifname: nic})
	}
	return links
}
