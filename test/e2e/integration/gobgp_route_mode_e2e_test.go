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
// The route distinguisher BOOTy uses is <ASN>:<VNI>.

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

	rd := rdLine("65020:100")
	for _, routeType := range []string{"prefix", "macip", "multicast"} {
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

	rd := rdLine("65040:100")
	for _, routeType := range []string{"prefix", "macip", "multicast"} {
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
		name, container, rd, log string
	}{
		{name: "L3VNI in FRR config", container: gobgpLabNumbered, rd: "65030:100", log: "provision VNI is an FRR L3VNI"},
		{name: "no FRR config", container: gobgpLabDual, rd: "65010:100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.log != "" {
				waitForBootyLog(t, tt.container, tt.log)
			}
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
					if strings.Contains(out, rd) {
						t.Fatalf("%s originated %s routes in Type-5 only mode:\n%s", tt.container, routeType, out)
					}
				}
				time.Sleep(bgpConvergeInterval)
			}
		})
	}
}
