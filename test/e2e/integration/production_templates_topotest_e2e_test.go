//go:build e2e_prodtpl

// FRR topotest scenarios that the other production template tests do not
// cover yet (#630 section 4.2).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// prodtplNexthopView lists, per BGP route of the underlay VRF, the next hop
// families and devices, for example "inet6-ll@ens1f0np0".
func prodtplNexthopView(ctx context.Context, node prodtplNode) (map[string][]string, error) {
	args := []string{"ip", "-j", "route", "show", "table", "10", "proto", "bgp"}
	if node.name == "shadow" {
		args = []string{"ip", "-j", "route", "show", "vrf", "Vrf_underlay", "proto", "bgp"}
	}
	out, err := prodtplExec(ctx, prodtplContainer(node), args...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		out = "[]"
	}
	type via struct {
		Family string `json:"family"`
		Host   string `json:"host"`
	}
	var routes []struct {
		Dst      string `json:"dst"`
		Dev      string `json:"dev"`
		Via      *via   `json:"via"`
		Gateway  string `json:"gateway"`
		Nexthops []struct {
			Dev     string `json:"dev"`
			Via     *via   `json:"via"`
			Gateway string `json:"gateway"`
		} `json:"nexthops"`
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return nil, fmt.Errorf("decode routes of %s: %w", node.name, err)
	}
	kind := func(v *via, gw string) string {
		switch {
		case v != nil && v.Family == "inet6" && strings.HasPrefix(v.Host, "fe80:"):
			return "inet6-ll"
		case v != nil:
			return v.Family
		case gw != "":
			return "inet"
		}
		return "none"
	}
	others := prodtplOthers(node)
	view := map[string][]string{}
	for _, r := range routes {
		if r.Dst == prodtplTesterPrefix {
			continue
		}
		dst := r.Dst
		if slices.Contains(others, dst) {
			dst = "other-server"
		}
		var hops []string
		if r.Dev != "" {
			hops = append(hops, kind(r.Via, r.Gateway)+"@"+r.Dev)
		}
		for _, nh := range r.Nexthops {
			hops = append(hops, kind(nh.Via, nh.Gateway)+"@"+nh.Dev)
		}
		slices.Sort(hops)
		view[dst] = hops
	}
	return view, nil
}

// Mirrors FRR bgp_ipv4_over_ipv6 test_unnumbered_loopback_ebgp_nbr_p0
// (RFC 5549, #630 C5): the IPv4 routes from the unnumbered leaf sessions
// go via the leaf's IPv6 link-local address on each uplink, for BOOTy and
// the vars-only server as for the shadow node.
func TestProdTplRFC5549Routes(t *testing.T) {
	var want map[string][]string
	for _, node := range prodtplServers {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		var got map[string][]string
		var err error
		for {
			got, err = prodtplNexthopView(ctx, node)
			if err == nil && len(got) > 0 && allLinkLocal(got) && (want == nil || reflect.DeepEqual(got, want)) {
				break
			}
			select {
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s next hops (%v):\n  got  %v\n  want %v (all inet6-ll)", node.name, err, got, want)
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
		if want == nil {
			want = got // the shadow node runs first
		}
	}
}

func allLinkLocal(view map[string][]string) bool {
	for _, hops := range view {
		for _, h := range hops {
			if !strings.HasPrefix(h, "inet6-ll@") {
				return false
			}
		}
	}
	return true
}

// Mirrors FRR bgp_ebgp_requires_policy test_ebgp_requires_policy (#630
// C9): the template has "no bgp ebgp-requires-policy", so routes from an
// eBGP peer without an inbound route-map are accepted. The tester session
// has only an outbound route-map; its 198.18.0.0/24 must be in the VRF
// table of BOOTy and of the shadow node.
func TestProdTplEBGPWithoutPolicy(t *testing.T) {
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		var got []string
		var err error
		for {
			got, err = prodtplTesterRoute(ctx, node)
			if err == nil && slices.Contains(got, "ens1f2np2") {
				break
			}
			select {
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s: %s via %v (%v), want a path over the tester uplink ens1f2np2", node.name, prodtplTesterPrefix, got, err)
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
	}
}
