//go:build e2e_gobgp_type5

// Package integration contains E2E tests for the pure Type-5 per-machine leaf fabric.
// These tests validate that BOOTy's GoBGP stack works in the exact CAPRF production layout:
//   - Per-machine leaf (pure L3, no VXLAN on leaf)
//   - eBGP unnumbered peering between BOOTy and its dedicated leaf
//   - L3VNI 1000 with nolearning (EVPN control-plane driven)
//   - Pure Type-5 IP prefix routes with VXLAN encapsulation and router MAC ECs
//   - NO Type-2/Type-3 routes
//   - Jumbo MTU 9100 on underlay
//
// Prerequisites:
//
//	make clab-type5-up   # deploys topology-type5.clab.yml
package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	type5LabSpine = "clab-booty-type5-lab-spine01"
	type5LabLeaf1 = "clab-booty-type5-lab-leaf01"
	type5LabLeaf2 = "clab-booty-type5-lab-leaf02"
	type5LabVM0   = "clab-booty-type5-lab-booty-vm0"
	type5LabVM1   = "clab-booty-type5-lab-booty-vm1"

	type5ConvergeTimeout  = 90 * time.Second
	type5ConvergeInterval = 2 * time.Second
)

func requireType5Lab(t *testing.T) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker not available: %v", err)
	}
	if !strings.Contains(string(out), type5LabSpine) {
		t.Fatal("Type-5 topology not deployed (run: make clab-type5-up)")
	}
}

func type5DockerExec(t *testing.T, container string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), type5ConvergeTimeout)
	defer cancel()
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.CommandContext(ctx, "docker", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %s failed: %v\n%s",
			container, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func type5DockerExecRaw(t *testing.T, container string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), type5ConvergeTimeout)
	defer cancel()
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.CommandContext(ctx, "docker", cmdArgs...).CombinedOutput()
	return string(out), err
}

func type5HTTPGetRaw(t *testing.T, container, url string) (string, error) {
	t.Helper()
	return type5DockerExecRaw(t, container, "sh", "-c",
		`wget -qO- -T 2 "$1" 2>/dev/null || busybox wget -qO- -T 2 "$1" 2>/dev/null || curl -fsS --max-time 2 "$1"`,
		"type5-http", url)
}

func type5Vtysh(t *testing.T, container string, cmd string) string {
	t.Helper()
	return type5DockerExec(t, container, "vtysh", "-c", cmd)
}

func type5VtyshRaw(t *testing.T, container string, cmd string) (string, error) {
	t.Helper()
	return type5DockerExecRaw(t, container, "vtysh", "-c", cmd)
}

func type5DumpDebugState(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}

	t.Log("=== TYPE-5 DEBUG STATE DUMP (test failed) ===")

	for _, cmd := range []string{
		"show bgp summary json",
		"show bgp ipv4 unicast",
		"show bgp l2vpn evpn",
		"show bgp l2vpn evpn route type prefix",
		"show bgp l2vpn evpn route type macip",
		"show bgp l2vpn evpn route type multicast",
		"show evpn vni",
		"show ip route",
	} {
		out, _ := type5VtyshRaw(t, type5LabSpine, cmd)
		t.Logf("[spine01] %s:\n%s", cmd, out)
	}

	for _, cmd := range [][]string{
		{"ip", "-d", "link", "show", "type", "vxlan"},
		{"ip", "addr", "show"},
		{"ip", "neigh", "show"},
		{"bridge", "fdb", "show"},
	} {
		out, _ := type5DockerExecRaw(t, type5LabSpine, cmd...)
		t.Logf("[spine01] %s:\n%s", strings.Join(cmd, " "), out)
	}

	for _, leaf := range []struct {
		name, container string
	}{
		{"leaf01", type5LabLeaf1},
		{"leaf02", type5LabLeaf2},
	} {
		for _, cmd := range []string{
			"show bgp summary json",
			"show bgp l2vpn evpn",
			"show ip route",
		} {
			out, _ := type5VtyshRaw(t, leaf.container, cmd)
			t.Logf("[%s] %s:\n%s", leaf.name, cmd, out)
		}
	}

	for _, vm := range []struct {
		name, container, ip string
	}{
		{"booty-vm0", type5LabVM0, "10.200.0.10"},
		{"booty-vm1", type5LabVM1, "10.200.0.11"},
	} {
		for _, cmd := range [][]string{
			{"ip", "-d", "link", "show", "type", "vxlan"},
			{"ip", "addr", "show"},
			{"ip", "neigh", "show"},
			{"ip", "route", "show"},
			{"ip", "route", "get", "10.200.0.10", "from", vm.ip},
			{"ip", "route", "get", "10.200.0.11", "from", vm.ip},
			{"bridge", "fdb", "show"},
		} {
			out, _ := type5DockerExecRaw(t, vm.container, cmd...)
			t.Logf("[%s] %s:\n%s", vm.name, strings.Join(cmd, " "), out)
		}
	}

}

func type5WaitForBGPInterface(t *testing.T, container, iface string) {
	t.Helper()
	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5VtyshRaw(t, container, "show bgp neighbors "+iface+" json")
		if strings.Contains(out, "Established") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("BGP peer on %s (%s) did not reach ESTABLISHED within %s:\n%s",
				container, iface, type5ConvergeTimeout, out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

// --- Spine-Leaf Fabric BGP ---------------------------------------------------

func TestType5SpineLeaf01BGPEstablished(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth1")
	t.Log("spine01 ↔ leaf01 BGP ESTABLISHED")
}

func TestType5SpineLeaf02BGPEstablished(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth2")
	t.Log("spine01 ↔ leaf02 BGP ESTABLISHED")
}

func TestType5Leaf01VM0BGPEstablished(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf1, "eth2")
	t.Log("leaf01 ↔ booty-vm0 BGP ESTABLISHED (proves GoBGP unnumbered)")
}

func TestType5Leaf02VM1BGPEstablished(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf2, "eth2")
	t.Log("leaf02 ↔ booty-vm1 BGP ESTABLISHED (proves GoBGP unnumbered)")
}

// --- EVPN Address Family Active ----------------------------------------------

func TestType5Leaf01EVPNActive(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf1, "eth2")

	out := type5Vtysh(t, type5LabLeaf1, "show bgp neighbors eth2 json")
	if !strings.Contains(strings.ToLower(out), "l2vpnevpn") {
		t.Errorf("L2VPN-EVPN not active on leaf01:eth2 (toward VM0):\n%s", out)
	}
}

func TestType5Leaf02EVPNActive(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf2, "eth2")

	out := type5Vtysh(t, type5LabLeaf2, "show bgp neighbors eth2 json")
	if !strings.Contains(strings.ToLower(out), "l2vpnevpn") {
		t.Errorf("L2VPN-EVPN not active on leaf02:eth2 (toward VM1):\n%s", out)
	}
}

// --- Underlay VTEP Reachability ----------------------------------------------

func TestType5VM0VTEPReachable(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth1")

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5VtyshRaw(t, type5LabSpine,
			"show bgp ipv4 unicast 192.168.4.10/32")
		if strings.Contains(out, "192.168.4.10") && !strings.Contains(out, "not in table") {
			t.Log("VM0 VTEP 192.168.4.10/32 learned on spine01")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM0 VTEP 192.168.4.10/32 not on spine01:\n%s", out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func TestType5VM1VTEPReachable(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5VtyshRaw(t, type5LabSpine,
			"show bgp ipv4 unicast 192.168.4.11/32")
		if strings.Contains(out, "192.168.4.11") && !strings.Contains(out, "not in table") {
			t.Log("VM1 VTEP 192.168.4.11/32 learned on spine01")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM1 VTEP 192.168.4.11/32 not on spine01:\n%s", out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

// --- Type-5 Routes -----------------------------------------------------------

func TestType5VM0DirectExportAsType5(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf1, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	var last string
	for {
		out, _ := type5VtyshRaw(t, type5LabLeaf1,
			"show bgp l2vpn evpn route type prefix")
		last = out
		if type5RouteHasEncapRouterMACAndNextHop(out, "10.200.0.10", "192.168.4.10") {
			t.Log("VM0 overlay IP 10.200.0.10 exported as VXLAN Type-5 on leaf01")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM0 direct Type-5 route missing ET:8/Rmac/next-hop on leaf01:\n%s", last)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func TestType5VM1DirectExportAsType5(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf2, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	var last string
	for {
		out, _ := type5VtyshRaw(t, type5LabLeaf2,
			"show bgp l2vpn evpn route type prefix")
		last = out
		if type5RouteHasEncapRouterMACAndNextHop(out, "10.200.0.11", "192.168.4.11") {
			t.Log("VM1 overlay IP 10.200.0.11 exported as VXLAN Type-5 on leaf02")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM1 direct Type-5 route missing ET:8/Rmac/next-hop on leaf02:\n%s", last)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

// TestType5RouterMACPerHost checks that each VM advertises its own derived
// router MAC. Before, hosts without an IPMI MAC all used 02:54:00:00:00:01.
func TestType5RouterMACPerHost(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	tests := []struct {
		leaf, prefix, nextHop string
	}{
		{type5LabLeaf1, "10.200.0.10", "192.168.4.10"},
		{type5LabLeaf2, "10.200.0.11", "192.168.4.11"},
	}
	var macs []string
	for _, tt := range tests {
		type5WaitForBGPInterface(t, tt.leaf, "eth2")
		deadline := time.Now().Add(type5ConvergeTimeout)
		for {
			out, _ := type5VtyshRaw(t, tt.leaf, "show bgp l2vpn evpn route type prefix")
			mac, ok := type5RouteRouterMACAndNextHop(out, tt.prefix, tt.nextHop)
			if ok {
				if !strings.HasPrefix(mac, "02:54:") || mac == "02:54:00:00:00:01" {
					t.Fatalf("router MAC for %s = %q, want a derived 02:54 MAC", tt.prefix, mac)
				}
				macs = append(macs, mac)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no router MAC for %s via %s:\n%s", tt.prefix, tt.nextHop, out)
			}
			time.Sleep(type5ConvergeInterval)
		}
	}
	if macs[0] == macs[1] {
		t.Fatalf("both VMs advertise router MAC %s", macs[0])
	}
}

func TestType5VM0OverlayAsType5(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth1")

	deadline := time.Now().Add(type5ConvergeTimeout)
	var last string
	for {
		out, _ := type5VtyshRaw(t, type5LabSpine,
			"show bgp l2vpn evpn route type prefix")
		last = out
		if type5RouteHasEncapRouterMACAndNextHop(out, "10.200.0.10", "192.168.4.10") {
			t.Log("VM0 overlay IP 10.200.0.10 present as VXLAN Type-5 on spine01")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM0 overlay Type-5 route missing ET:8/Rmac/next-hop on spine01:\n%s", last)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func TestType5VM1OverlayAsType5(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	var last string
	for {
		out, _ := type5VtyshRaw(t, type5LabSpine,
			"show bgp l2vpn evpn route type prefix")
		last = out
		if type5RouteHasEncapRouterMACAndNextHop(out, "10.200.0.11", "192.168.4.11") {
			t.Log("VM1 overlay IP 10.200.0.11 present as VXLAN Type-5 on spine01")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM1 overlay Type-5 route missing ET:8/Rmac/next-hop on spine01:\n%s", last)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

// --- NO Type-2 (MAC/IP) Routes -----------------------------------------------

func TestType5NoType2Routes(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth1")
	type5WaitForBGPInterface(t, type5LabSpine, "eth2")

	out, _ := type5VtyshRaw(t, type5LabSpine,
		"show bgp l2vpn evpn route type macip")
	if strings.Contains(out, "10.200.0") {
		t.Errorf("pure Type-5 model should have NO Type-2 routes for VM overlay IPs:\n%s", out)
	}
	t.Log("Confirmed: no Type-2 (MAC/IP) routes for VM overlay IPs on spine01")
}

// --- NO Type-3 (IMET) Routes from VMs ----------------------------------------

func TestType5NoType3RoutesFromVMs(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabSpine, "eth1")
	type5WaitForBGPInterface(t, type5LabSpine, "eth2")

	out, _ := type5VtyshRaw(t, type5LabSpine,
		"show bgp l2vpn evpn route type multicast")
	if strings.Contains(out, "192.168.4.10") || strings.Contains(out, "192.168.4.11") {
		t.Errorf("pure Type-5 model: VMs should NOT send Type-3 (IMET) routes:\n%s", out)
	}
	t.Log("Confirmed: no Type-3 (IMET) routes from VM VTEPs on spine01")
}

// --- Spine Configuration Correctness -----------------------------------------

func TestType5SpineAdvertiseIPv4Unicast(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	out := type5Vtysh(t, type5LabSpine, "show running-config")
	if !strings.Contains(out, "vni 1000") {
		t.Error("spine01 should map L3VNI 1000 for Type-5 routing")
	}
	if !strings.Contains(out, "advertise ipv4 unicast") {
		t.Error("spine01 should have 'advertise ipv4 unicast' for Type-5 generation")
	}
	if !strings.Contains(out, "advertise-all-vni") {
		t.Error("spine01 should have 'advertise-all-vni' for EVPN VNI discovery")
	}
	if !strings.Contains(out, "neighbor fabric next-hop-self force") {
		t.Error("spine01 must rewrite reflected underlay next hops for leaf VTEP reachability")
	}
	if !strings.Contains(out, "route-target import 65000:1000") {
		t.Error("spine01 should import RT 65000:1000 for the Type-5 L3VNI")
	}
	if !strings.Contains(out, "route-target export 65000:1000") {
		t.Error("spine01 should export RT 65000:1000 for the Type-5 L3VNI")
	}
	t.Log("Confirmed: spine01 uses L3VNI 1000, advertise ipv4 unicast, and RT 65000:1000")
}

// --- Leaves are Pure L3 (No VXLAN) -------------------------------------------

func TestType5LeavesPureL3(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	for _, leaf := range []struct {
		name, container string
	}{
		{"leaf01", type5LabLeaf1},
		{"leaf02", type5LabLeaf2},
	} {
		out, _ := type5DockerExecRaw(t, leaf.container, "ip", "-d", "link", "show", "type", "vxlan")
		if strings.Contains(out, "vxlan") {
			t.Errorf("%s has VXLAN interfaces — should be pure L3 router:\n%s", leaf.name, out)
		}
	}
	t.Log("Confirmed: leaves are pure L3 routers (no VXLAN)")
}

// --- VXLAN Data Plane --------------------------------------------------------

func TestType5VXLANOnVM0(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf1, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5DockerExecRaw(t, type5LabVM0, "ip", "-d", "link", "show", "type", "vxlan")
		if strings.Contains(out, "vxlan") && strings.Contains(out, "id 1000") {
			t.Log("VXLAN interface with VNI 1000 present on booty-vm0")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VXLAN VNI 1000 not found on booty-vm0:\n%s", out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func TestType5VXLANOnVM1(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf2, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5DockerExecRaw(t, type5LabVM1, "ip", "-d", "link", "show", "type", "vxlan")
		if strings.Contains(out, "vxlan") && strings.Contains(out, "id 1000") {
			t.Log("VXLAN interface with VNI 1000 present on booty-vm1")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VXLAN VNI 1000 not found on booty-vm1:\n%s", out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func type5RouteHasEncapAndRouterMAC(output, prefix string) bool {
	block := type5RouteBlock(output, prefix)
	return type5RouteBlockHasToken(block, "ET:8") && type5RouteBlockHasTokenPrefix(block, "Rmac:")
}

func type5RouteHasEncapRouterMACAndNextHop(output, prefix, nextHop string) bool {
	_, ok := type5RouteRouterMACAndNextHop(output, prefix, nextHop)
	return ok
}

func type5RouteRouterMACAndNextHop(output, prefix, nextHop string) (string, bool) {
	block := type5RouteBlock(output, prefix)
	for _, path := range type5RoutePathBlocks(block) {
		if type5RouteBlockHasToken(path, "ET:8") &&
			type5RouteBlockHasTokenPrefix(path, "Rmac:") &&
			type5RouteBlockHasNextHop(path, nextHop) {
			return type5RouteRouterMAC(path)
		}
	}
	return "", false
}

func type5RouteRouterMAC(path string) (string, bool) {
	for _, field := range strings.Fields(path) {
		field = strings.Trim(field, ",")
		if mac, ok := strings.CutPrefix(field, "Rmac:"); ok && mac != "" {
			return strings.ToLower(mac), true
		}
	}
	return "", false
}

func type5RouteBlockHasToken(block, token string) bool {
	for _, field := range strings.Fields(block) {
		if field == token {
			return true
		}
	}
	return false
}

func type5RouteBlockHasTokenPrefix(block, prefix string) bool {
	for _, field := range strings.Fields(block) {
		if strings.HasPrefix(field, prefix) && len(field) > len(prefix) {
			return true
		}
	}
	return false
}

func type5RouteBlockHasNextHop(block, nextHop string) bool {
	for _, field := range strings.Fields(block) {
		if field == nextHop || strings.HasPrefix(field, nextHop+"(") {
			return true
		}
	}
	return false
}

func type5RouteBlock(output, prefix string) string {
	lines := strings.Split(output, "\n")
	routeLine := -1
	for i, line := range lines {
		if type5RouteLineHasPrefix(line, prefix) {
			routeLine = i
			break
		}
	}
	if routeLine < 0 {
		return ""
	}

	var block []string
	for i := routeLine; i < len(lines); i++ {
		if i > routeLine && type5StartsRouteEntry(lines[i]) {
			break
		}
		block = append(block, lines[i])
	}
	return strings.Join(block, "\n")
}

func type5RoutePathBlocks(block string) []string {
	var paths []string
	var current []string
	for _, line := range strings.Split(block, "\n") {
		if type5StartsRouteEntry(line) {
			continue
		}
		if type5LineStartsNextHop(line) {
			if len(current) > 0 {
				paths = append(paths, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		paths = append(paths, strings.Join(current, "\n"))
	}
	return paths
}

func type5LineStartsNextHop(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	nextHop := strings.TrimSuffix(fields[0], ",")
	if (nextHop == "*" || nextHop == ">" || nextHop == "*>") && len(fields) > 1 {
		nextHop = strings.TrimSuffix(fields[1], ",")
	}
	if before, _, ok := strings.Cut(nextHop, "("); ok {
		nextHop = before
	}
	return net.ParseIP(nextHop) != nil
}

func type5RouteLineHasPrefix(line, prefix string) bool {
	return strings.Contains(line, "]:["+prefix+"]")
}

func type5StartsRouteEntry(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "Route Distinguisher:") {
		return true
	}
	return strings.HasPrefix(trimmed, "*") && strings.Contains(trimmed, "]:[")
}

func type5HasLineWithTokens(output string, tokens ...string) bool {
	for _, line := range strings.Split(output, "\n") {
		if type5LineHasTokens(line, tokens...) {
			return true
		}
	}
	return false
}

func type5LineHasTokens(line string, tokens ...string) bool {
	for _, token := range tokens {
		if !strings.Contains(line, token) {
			return false
		}
	}
	return true
}

func TestType5RouteHasEncapAndRouterMAC(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.10(leaf01)
                    RT:65000:1000 ET:8 Rmac:02:54:00:00:00:01
Route Distinguisher: 10.0.0.1:2
 *>  [5]:[0]:[24]:[10.100.0.0]
                    192.168.4.1(spine01)
                    RT:65000:1000 ET:8 Rmac:a6:f7:73:6d:5b:4a`

	if !type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected VM prefix block to carry ET:8 and Rmac")
	}
}

func TestType5RouteHasEncapAndRouterMACAcceptsFinalRoute(t *testing.T) {
	output := `Route Distinguisher: 10.0.0.1:2
 *>  [5]:[0]:[24]:[10.100.0.0]
                    192.168.4.1(spine01)
                    RT:65000:1000 ET:8 Rmac:a6:f7:73:6d:5b:4a
Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.10(leaf01)
                    RT:65000:1000 ET:8 Rmac:02:54:00:00:00:01`

	if !type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected final VM prefix block to carry ET:8 and Rmac")
	}
}

func TestType5RouteHasEncapAndRouterMACRejectsMissingEncap(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.10(leaf01)
                    RT:65000:1000 Rmac:02:54:00:00:00:01
Route Distinguisher: 10.0.0.1:2
 *>  [5]:[0]:[24]:[10.100.0.0]
                    192.168.4.1(spine01)
                    RT:65000:1000 ET:8 Rmac:a6:f7:73:6d:5b:4a`

	if type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected VM prefix block without ET:8 to be rejected")
	}
}

func TestType5RouteHasEncapAndRouterMACRejectsMissingRouterMAC(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.10(leaf01)
                    RT:65000:1000 ET:8
Route Distinguisher: 10.0.0.1:2
 *>  [5]:[0]:[24]:[10.100.0.0]
                    192.168.4.1(spine01)
                    RT:65000:1000 ET:8 Rmac:a6:f7:73:6d:5b:4a`

	if type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected VM prefix block without Rmac to be rejected")
	}
}

func TestType5RouteHasEncapAndRouterMACRejectsSameRDFalsePositive(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.10(leaf01)
                    RT:65000:1000
 *>  [5]:[0]:[32]:[10.200.0.11]
                    192.168.4.11(leaf02)
                    RT:65000:1000 ET:8 Rmac:02:54:00:00:00:02`

	if type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected target VM prefix without ET:8/Rmac to be rejected")
	}
}

func TestType5RouteHasEncapAndRouterMACRejectsEncapPrefixFalsePositive(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.10(leaf01)
                    RT:65000:1000 ET:80 Rmac:02:54:00:00:00:01`

	if type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected ET:80 not to satisfy exact ET:8 requirement")
	}
}

func TestType5RouteHasEncapAndRouterMACRejectsPrefixSubstringFalsePositive(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.100]
                    192.168.4.100(leaf01)
                    RT:65000:1000 ET:8 Rmac:02:54:00:00:00:01`

	if type5RouteHasEncapAndRouterMAC(output, "10.200.0.10") {
		t.Fatal("expected 10.200.0.100 not to satisfy exact 10.200.0.10 prefix lookup")
	}
}

func TestType5RouteHasEncapRouterMACAndNextHopRejectsWrongNextHop(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.11(leaf01)
                    RT:65000:1000 ET:8 Rmac:02:54:00:00:00:01`

	if type5RouteHasEncapRouterMACAndNextHop(output, "10.200.0.10", "192.168.4.10") {
		t.Fatal("expected direct Type-5 assertion to reject wrong next-hop")
	}
}

func TestType5RouteHasEncapRouterMACAndNextHopRejectsSplitPathFalsePositive(t *testing.T) {
	output := `Route Distinguisher: 65100:1000
 *>  [5]:[0]:[32]:[10.200.0.10]
                    192.168.4.11(leaf01)
                    RT:65000:1000 ET:8 Rmac:02:54:00:00:00:01
 *                  192.168.4.10(leaf02)
                    RT:65000:1000`

	if type5RouteHasEncapRouterMACAndNextHop(output, "10.200.0.10", "192.168.4.10") {
		t.Fatal("expected direct Type-5 assertion to reject next-hop from a different path")
	}
}

func TestType5RouteHasEncapAndRouterMACRejectsMissingRoute(t *testing.T) {
	if type5RouteHasEncapAndRouterMAC("Route Distinguisher: 65100:1000", "10.200.0.10") {
		t.Fatal("expected missing VM prefix to be rejected")
	}
}

func TestType5TopologyHasNoStaticSpineBUMVTEPs(t *testing.T) {
	data, err := os.ReadFile("../clab/topology-type5.clab.yml")
	if err != nil {
		t.Fatalf("read type-5 topology: %v", err)
	}
	topology := string(data)
	for _, vtep := range []string{"192.168.4.10", "192.168.4.11"} {
		forbidden := "bridge fdb append 00:00:00:00:00:00 dev vxlan1000 dst " + vtep
		if strings.Contains(topology, forbidden) {
			t.Fatalf("type-5 topology must not mask dynamic FDB programming with %q", forbidden)
		}
	}
}

func TestType5TopologyUsesFRRWithType5EncapRelayFix(t *testing.T) {
	data, err := os.ReadFile("../clab/topology-type5.clab.yml")
	if err != nil {
		t.Fatalf("read type-5 topology: %v", err)
	}
	topology := string(data)
	if strings.Contains(topology, "quay.io/frrouting/frr:10.3.1") {
		t.Fatal("type-5 topology must not use FRR 10.3.1, which strips ET:8 from transited Type-5 routes")
	}
	if got := strings.Count(topology, "image: quay.io/frrouting/frr:10.3.2"); got != 3 {
		t.Fatalf("type-5 topology should pin all three FRR nodes to 10.3.2, got %d", got)
	}
}

func TestType5SpineConfigRewritesUnderlayNextHop(t *testing.T) {
	data, err := os.ReadFile("../clab/spine01-type5.frr.conf")
	if err != nil {
		t.Fatalf("read Type-5 spine config: %v", err)
	}
	if !strings.Contains(string(data), "neighbor fabric next-hop-self force") {
		t.Fatal("Type-5 spine must rewrite underlay next hops so leaves can proxy ARP remote VTEPs")
	}
}

func TestType5LeavesAcceptOnlyTheirAttachedVTEP(t *testing.T) {
	tests := []struct {
		path, prefixList, vtep string
	}{
		{"../clab/leaf01-type5.frr.conf", "vm0-vtep", "192.168.4.10"},
		{"../clab/leaf02-type5.frr.conf", "vm1-vtep", "192.168.4.11"},
	}
	for _, tt := range tests {
		data, err := os.ReadFile(tt.path)
		if err != nil {
			t.Fatalf("read Type-5 leaf config %s: %v", tt.path, err)
		}
		config := string(data)
		if !strings.Contains(config, "neighbor eth2 prefix-list "+tt.prefixList+" in") ||
			!strings.Contains(config, "ip prefix-list "+tt.prefixList+" seq 5 permit "+tt.vtep+"/32") {
			t.Fatalf("Type-5 leaf %s must only import its local VTEP %s from eth2", tt.path, tt.vtep)
		}
	}
}

// --- Overlay Connectivity ----------------------------------------------------

func TestType5VM0ProvisionIP(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf1, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5DockerExecRaw(t, type5LabVM0, "ip", "addr", "show", "dev", "br.provision")
		if strings.Contains(out, "10.200.0.10") {
			t.Log("Provision IP 10.200.0.10 assigned on booty-vm0 br.provision")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provision IP 10.200.0.10 not on br.provision:\n%s", out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func TestType5VM1ProvisionIP(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	type5WaitForBGPInterface(t, type5LabLeaf2, "eth2")

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		out, _ := type5DockerExecRaw(t, type5LabVM1, "ip", "addr", "show", "dev", "br.provision")
		if strings.Contains(out, "10.200.0.11") {
			t.Log("Provision IP 10.200.0.11 assigned on booty-vm1 br.provision")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provision IP 10.200.0.11 not on br.provision:\n%s", out)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func TestType5DCGWHasNoL2GatewayState(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	data, err := os.ReadFile("../clab/topology-type5.clab.yml")
	if err != nil {
		t.Fatalf("read type-5 topology: %v", err)
	}
	topology := string(data)
	for _, forbidden := range []string{
		"ip link add vxlan1000", "ip link add br1000", "\n    caprf-mock:",
		"bridge fdb append", "ip addr add 10.100.0.1",
	} {
		if strings.Contains(topology, forbidden) {
			t.Fatalf("Type-5 DCGW must not own L2 gateway state %q", forbidden)
		}
	}
	for _, varsFile := range []string{"../clab/data/vars-gobgp-type5-vm0", "../clab/data/vars-gobgp-type5-vm1"} {
		data, err := os.ReadFile(varsFile)
		if err != nil {
			t.Fatalf("read %s: %v", varsFile, err)
		}
		if strings.Contains(string(data), "provision_gateway=") {
			t.Fatalf("Type-5-only config %s must not set provision_gateway", varsFile)
		}
		if !strings.Contains(string(data), `EVPN_TYPE5_ONLY="true"`) {
			t.Fatalf("Type-5-only config %s must select EVPN_TYPE5_ONLY", varsFile)
		}
	}
	for _, cmd := range [][]string{
		{"ip", "-d", "link", "show", "type", "vxlan"},
		{"bridge", "fdb", "show"},
		{"ip", "addr", "show"},
	} {
		out := type5DockerExec(t, type5LabSpine, cmd...)
		if strings.Contains(out, "vxlan") || strings.Contains(out, "10.100.0.1") {
			t.Fatalf("Type-5 DCGW must not own L2 gateway state (%s):\n%s", strings.Join(cmd, " "), out)
		}
	}
}

func TestType5VM0ImportsVM1(t *testing.T) {
	testType5RemoteVMReachability(t, type5LabVM0, "10.200.0.10", "10.200.0.11", "192.168.4.11")
}

func TestType5VM1ImportsVM0(t *testing.T) {
	testType5RemoteVMReachability(t, type5LabVM1, "10.200.0.11", "10.200.0.10", "192.168.4.10")
}

func testType5RemoteVMReachability(t *testing.T, container, source, destination, vtep string) {
	t.Helper()
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	deadline := time.Now().Add(type5ConvergeTimeout)
	for {
		route, _ := type5DockerExecRaw(t, container, "ip", "route", "show", destination+"/32")
		fdb, _ := type5DockerExecRaw(t, container, "bridge", "fdb", "show", "dev", "vx1000")
		out, err := type5DockerExecRaw(t, container,
			"ping", "-c", "1", "-W", "2", "-I", "br.provision", destination)
		if err == nil && strings.Contains(out, "1 packets received") &&
			type5HasRemoteVMRoute(route, destination, vtep) &&
			type5HasRouterMACFDBForVTEP(fdb, vtep) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Type-5 reachability from %s to %s failed after %s:\nping: %s\nroute: %s\nfdb: %s",
				source, destination, type5ConvergeTimeout, out, route, fdb)
		}
		time.Sleep(type5ConvergeInterval)
	}
}

func type5HasRemoteVMRoute(output, destination, vtep string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != destination && fields[0] != destination+"/32") {
			continue
		}
		if type5LineHasTokens(line, "via", vtep, "dev", "br.provision") {
			return true
		}
	}
	return false
}

func type5HasRouterMACFDBForVTEP(output, vtep string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) > 0 && fields[0] != "00:00:00:00:00:00" &&
			type5LineHasTokens(line, "dst", vtep) {
			return true
		}
	}
	return false
}

func TestType5RemoteVMDataPlaneMatchers(t *testing.T) {
	routes := `10.200.0.11/32 via 192.168.4.11 dev br.provision
10.200.0.10 via 192.168.4.10 dev br.provision`
	fdb := `00:00:00:00:00:00 dst 192.168.4.11 self permanent
02:54:00:00:00:11 dst 192.168.4.11 self permanent`

	if !type5HasRemoteVMRoute(routes, "10.200.0.11", "192.168.4.11") {
		t.Fatal("expected remote VM Type-5 kernel route to match")
	}
	if type5HasRemoteVMRoute(routes, "10.200.0.11", "192.168.4.10") {
		t.Fatal("wrong VTEP must not satisfy the Type-5 route matcher")
	}
	if !type5HasRouterMACFDBForVTEP(fdb, "192.168.4.11") {
		t.Fatal("expected router-MAC FDB entry for remote VTEP")
	}
	if type5HasRouterMACFDBForVTEP("00:00:00:00:00:00 dst 192.168.4.11 self permanent", "192.168.4.11") {
		t.Fatal("BUM FDB entry must not satisfy the router-MAC FDB matcher")
	}
}

func TestType5JumboMTU(t *testing.T) {
	requireType5Lab(t)
	t.Cleanup(func() { type5DumpDebugState(t) })

	// Spine → leaf interfaces should be MTU 9100.
	out := type5DockerExec(t, type5LabSpine, "ip", "link", "show", "dev", "eth1")
	if !strings.Contains(out, "mtu 9100") {
		t.Errorf("spine01:eth1 should have MTU 9100:\n%s", out)
	}
	out = type5DockerExec(t, type5LabSpine, "ip", "link", "show", "dev", "eth2")
	if !strings.Contains(out, "mtu 9100") {
		t.Errorf("spine01:eth2 should have MTU 9100:\n%s", out)
	}

	// Leaf → VM interfaces should be MTU 9100.
	out = type5DockerExec(t, type5LabLeaf1, "ip", "link", "show", "dev", "eth2")
	if !strings.Contains(out, "mtu 9100") {
		t.Errorf("leaf01:eth2 should have MTU 9100:\n%s", out)
	}
	out = type5DockerExec(t, type5LabLeaf2, "ip", "link", "show", "dev", "eth2")
	if !strings.Contains(out, "mtu 9100") {
		t.Errorf("leaf02:eth2 should have MTU 9100:\n%s", out)
	}

	t.Log("Confirmed: jumbo MTU 9100 on all underlay interfaces")
}
