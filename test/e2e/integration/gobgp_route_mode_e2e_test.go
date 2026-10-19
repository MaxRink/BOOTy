//go:build e2e_gobgp

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// EVPN route mode e2e tests. BOOTy selects the EVPN route types it originates
// from the provisioner FRR config (see README "Netplan Configuration"):
//   - booty-unnumbered: advertise-all-vni with an L2VNI, so Type-2 and Type-3
//     routes are originated next to Type-5.
//   - booty-numbered: advertise-all-vni with the provisioning VNI declared as
//     the default VRF L3VNI (the CAPRF frr.conf shape), so Type-5 only.
//   - booty-dual: no FRR config (vars only), so Type-5 only.
//   - booty-l2: no FRR config, but EVPN_L2_ENABLED=true in vars, so Type-2
//     and Type-3 routes are originated next to Type-5.
// BOOTy uses FRR's automatic route distinguisher <router-id>:<n>: n is the
// id of the "router bgp" instance (1 with one instance), and an L2VNI gets
// the next id, so its Type-2 and Type-3 routes have <router-id>:2.

const routeModeObserveWindow = 15 * time.Second

func spineEVPNRoutes(t *testing.T, routeType string) (string, error) {
	t.Helper()
	return gobgpDockerExecRaw(t, gobgpLabSpine, "vtysh", "-c", "show bgp l2vpn evpn route type "+routeType)
}

func rdLine(rd string) string { return "Route Distinguisher: " + rd }

// waitForBootyLog polls the container log until it contains want.
func waitForBootyLog(t *testing.T, container, want string) {
	t.Helper()
	waitForGobgpCondition(t, bgpConvergeTimeout, container+" log does not contain "+want, func(ctx context.Context) (bool, string) {
		out, err := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
		if err != nil {
			return false, err.Error()
		}
		return strings.Contains(string(out), want), ""
	})
}

func TestGoBGPRouteModeL2FromFRRConfig(t *testing.T) {
	requireGoBGPLab(t)
	t.Cleanup(func() { dumpDebugState(t) })

	waitForBGPInterface(t, "eth3")
	waitForBootyLog(t, gobgpLabUnnumbered, "detected netplan configuration files")

	for routeType, rd := range map[string]string{"prefix": rdLine("10.0.0.20:1"), "macip": rdLine("10.0.0.20:2"), "multicast": rdLine("10.0.0.20:2")} {
		waitForGobgpCondition(t, bgpConvergeTimeout, "booty-unnumbered "+routeType+" route not on spine", func(context.Context) (bool, string) {
			out, err := spineEVPNRoutes(t, routeType)
			return err == nil && strings.Contains(out, rd), out
		})
	}
}

// TestGoBGPRouteModeL2FromVars checks the explicit opt-in. booty-l2 has no
// FRR config with advertise-all-vni, only EVPN_L2_ENABLED=true in vars.
func TestGoBGPRouteModeL2FromVars(t *testing.T) {
	requireGoBGPLab(t)
	t.Cleanup(func() { dumpDebugState(t) })

	waitForBGPInterface(t, "eth7")

	for routeType, rd := range map[string]string{"prefix": rdLine("10.0.0.23:1"), "macip": rdLine("10.0.0.23:2"), "multicast": rdLine("10.0.0.23:2")} {
		waitForGobgpCondition(t, bgpConvergeTimeout, "booty-l2 "+routeType+" route not on spine", func(context.Context) (bool, string) {
			out, err := spineEVPNRoutes(t, routeType)
			return err == nil && strings.Contains(out, rd), out
		})
	}
}

func TestGoBGPRouteModeType5Only(t *testing.T) {
	requireGoBGPLab(t)
	t.Cleanup(func() { dumpDebugState(t) })

	tests := []struct {
		name, container, rd string
	}{
		{name: "L3VNI in FRR config", container: gobgpLabNumbered, rd: "10.0.0.22:1"},
		{name: "no FRR config", container: gobgpLabDual, rd: "10.0.0.21:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd := rdLine(tt.rd)
			waitForGobgpCondition(t, 2*bgpConvergeTimeout, tt.container+" Type-5 route not on spine", func(context.Context) (bool, string) {
				out, err := spineEVPNRoutes(t, "prefix")
				return err == nil && strings.Contains(out, rd), out
			})
			// Type-2/Type-3 would follow Type-5 within the same setup, so watch
			// for a while before concluding that they are absent.
			deadline := time.Now().Add(routeModeObserveWindow)
			for time.Now().Before(deadline) {
				for _, routeType := range []string{"macip", "multicast"} {
					out, err := spineEVPNRoutes(t, routeType)
					if err != nil {
						t.Fatalf("query %s routes on spine01: %v\n%s", routeType, err, out)
					}
					// Any RD of the server: an L2VNI would use the next id.
					if strings.Contains(out, rdLine(strings.Split(tt.rd, ":")[0]+":")) {
						t.Fatalf("%s originated %s routes in Type-5 only mode:\n%s", tt.container, routeType, out)
					}
				}
				time.Sleep(bgpConvergeInterval)
			}
		})
	}
}
