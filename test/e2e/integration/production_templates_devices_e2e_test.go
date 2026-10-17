//go:build e2e_prodtpl

// Kernel device state of the production template lab (#630 N1, N3,
// section 4.3): BOOTy with the templates and the vars-only server must
// build the same devices as the shadow node, which applies the template
// netplan the way systemd-networkd does.
package integration

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// prodtplDevices are the devices of the template that every server has.
var prodtplDevices = []string{"Vrf_underlay", "dum.underlay", "vx.3000", "br.provision", "ens1f0np0", "ens1f1np1"}

// prodtplDeviceView returns one line per device setting, with the values
// of the server replaced by "vtep" and "rmac".
func prodtplDeviceView(ctx context.Context, node prodtplNode) ([]string, error) {
	c := prodtplContainer(node)
	norm := strings.NewReplacer(node.underlay, "vtep", prodtplV6Of[node.name].rmac, "rmac", prodtplV6Of[node.name].lo, "lo")
	var links []struct {
		Name     string   `json:"ifname"`
		MTU      int      `json:"mtu"`
		Master   string   `json:"master"`
		Flags    []string `json:"flags"`
		Address  string   `json:"address"`
		GenMode  string   `json:"inet6_addr_gen_mode"`
		LinkInfo struct {
			Kind      string         `json:"info_kind"`
			Data      map[string]any `json:"info_data"`
			SlaveKind string         `json:"info_slave_kind"`
			SlaveData map[string]any `json:"info_slave_data"`
		} `json:"linkinfo"`
	}
	if err := prodtplJSON(ctx, c, &links, "ip", "-d", "-j", "link", "show"); err != nil {
		return nil, err
	}
	var view []string
	add := func(format string, args ...any) { view = append(view, norm.Replace(fmt.Sprintf(format, args...))) }
	for _, l := range links {
		if !slices.Contains(prodtplDevices, l.Name) {
			continue
		}
		add("%s kind=%s mtu=%d master=%s up=%v addrgenmode=%s", l.Name, l.LinkInfo.Kind, l.MTU, l.Master, slices.Contains(l.Flags, "UP"), l.GenMode)
		switch l.LinkInfo.Kind {
		case "vxlan":
			d := l.LinkInfo.Data
			add("%s vxlan id=%v local=%v link=%v port=%v learning=%v proxy=%v mac=%s", l.Name, d["id"], d["local"], d["link"], d["port"], d["learning"], d["proxy"], l.Address)
		case "bridge":
			add("%s bridge mac=%s stp=%v", l.Name, l.Address, l.LinkInfo.Data["stp_state"])
		case "vrf":
			add("%s vrf table=%v", l.Name, l.LinkInfo.Data["table"])
		}
		if l.LinkInfo.SlaveKind == "bridge" {
			d := l.LinkInfo.SlaveData
			add("%s port hairpin=%v learning=%v", l.Name, d["hairpin"], d["learning"])
		}
		ra, err := prodtplExec(ctx, c, "cat", "/proc/sys/net/ipv6/conf/"+l.Name+"/accept_ra")
		if err != nil {
			return nil, err
		}
		add("%s accept_ra=%s", l.Name, strings.TrimSpace(ra))
	}
	addrs, err := prodtplAddrView(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		add("%s", a)
	}
	// Both forwarding sysctls: the template has "no ip forwarding" and no
	// "ipv6 forwarding" line, so IPv6 keeps the kernel default.
	for _, p := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
		fwd, err := prodtplExec(ctx, c, "cat", p)
		if err != nil {
			return nil, err
		}
		add("%s=%s", p, strings.TrimSpace(fwd))
	}
	// The Router flag of the server's Neighbor Advertisements (set with
	// IPv6 forwarding on) as the leaf keeps it for the server's link-local
	// address on each uplink.
	for i, port := range node.leafPorts {
		var neigh []struct {
			Dst   string   `json:"dst"`
			State []string `json:"state"`
			Flags []string `json:"flags"`
		}
		if err := prodtplJSON(ctx, prodtplLeaf, &neigh, "ip", "-6", "-j", "neigh", "show", "dev", port); err != nil {
			return nil, err
		}
		for _, n := range neigh {
			if strings.HasPrefix(n.Dst, "fe80:") && !slices.Contains(n.State, "FAILED") {
				add("uplink%d leaf neigh router=%v", i, slices.Contains(n.Flags, "router"))
			}
		}
	}
	slices.Sort(view)
	return view, nil
}

// prodtplAddrView lists the addresses of the template devices and lo, with
// "ll" for any IPv6 link-local address.
func prodtplAddrView(ctx context.Context, c string) ([]string, error) {
	var ifs []struct {
		Name  string `json:"ifname"`
		Addrs []struct {
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
			Scope  string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := prodtplJSON(ctx, c, &ifs, "ip", "-j", "addr", "show"); err != nil {
		return nil, err
	}
	var out []string
	for _, i := range ifs {
		if !slices.Contains(prodtplDevices, i.Name) && i.Name != "lo" {
			continue
		}
		for _, a := range i.Addrs {
			switch {
			case a.Scope == "host":
			case a.Scope == "link":
				out = append(out, i.Name+" addr ll")
			default:
				out = append(out, fmt.Sprintf("%s addr %s/%d", i.Name, a.Local, a.Prefix))
			}
		}
	}
	return out, nil
}

// Like a differential `ip -d link`, `ip addr`, `bridge link` and sysctl
// comparison: the VXLAN device and the bridge follow the netplan keys
// (mtu, macaddress, mac-learning, arp-proxy, hairpin, port-mac-learning,
// link-local: [], accept-ra: false), "no ip forwarding" holds, IPv6
// forwarding stays off, and the leaf sees the same Router flag.
func TestProdTplKernelDevices(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	// Polled until BOOTy and the vars-only server look like the shadow
	// node: the RA and NA state settles after the sessions are up.
	var views map[string][]string
	var lastErr error
	for {
		views, lastErr = map[string][]string{}, nil
		for _, node := range prodtplServers {
			v, err := prodtplDeviceView(ctx, node)
			if err != nil {
				lastErr = err
			}
			views[node.name] = v
		}
		if lastErr == nil && len(views["shadow"]) > 0 &&
			reflect.DeepEqual(views["booty"], views["shadow"]) && reflect.DeepEqual(views["native"], views["shadow"]) {
			break
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				t.Fatalf("devices: %v", lastErr)
			}
			for _, name := range []string{"booty", "native"} {
				if !reflect.DeepEqual(views[name], views["shadow"]) {
					t.Errorf("%s differs from the shadow node:\n%s", name, diffLines(views[name], views["shadow"]))
				}
			}
			return
		case <-time.After(2 * time.Second):
		}
	}
	for _, want := range []string{
		"vx.3000 kind=vxlan mtu=9000 master=br.provision up=true addrgenmode=none",
		"vx.3000 port hairpin=true learning=false",
		"vx.3000 vxlan id=3000 local=vtep link=dum.underlay port=4789 learning=false proxy=false mac=rmac",
		"br.provision kind=bridge mtu=9000 master= up=true addrgenmode=none",
		"ens1f0np0 accept_ra=0",
		"/proc/sys/net/ipv4/ip_forward=0",
		"/proc/sys/net/ipv6/conf/all/forwarding=0",
	} {
		if !slices.Contains(views["shadow"], want) {
			t.Fatalf("shadow (the reference) lacks %q:\n%s", want, strings.Join(views["shadow"], "\n"))
		}
	}
}

func diffLines(got, want []string) string {
	var b strings.Builder
	for _, l := range got {
		if !slices.Contains(want, l) {
			b.WriteString("  + " + l + "\n")
		}
	}
	for _, l := range want {
		if !slices.Contains(got, l) {
			b.WriteString("  - " + l + "\n")
		}
	}
	return b.String()
}
