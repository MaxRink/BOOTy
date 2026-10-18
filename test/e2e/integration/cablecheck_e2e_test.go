//go:build e2e_cablecheck

// Cable check lab (topology-cablecheck.clab.yml): BOOTy sends LLDP with
// the bm4x-cablecheck identity, and the lldpd leaf shows what a switch
// reports to NSO, which the bm4x connection check reads.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	cablecheckLeaf  = "clab-booty-cablecheck-lab-leaf01"
	cablecheckBooty = "clab-booty-cablecheck-lab-booty"
)

var cablecheckNICs = []string{"eth1", "eth2"}

func cablecheckExec(ctx context.Context, container string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker exec %s %v: %w: %s", container, args, err, out)
	}
	return string(out), nil
}

// lldpdNeighbor is one neighbor in "lldpcli -f json0 show neighbors".
type lldpdNeighbor struct {
	Name    string `json:"name"`
	Chassis []struct {
		Name []struct {
			Value string `json:"value"`
		} `json:"name"`
	} `json:"chassis"`
	Port []struct {
		ID []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"id"`
		Descr []struct {
			Value string `json:"value"`
		} `json:"descr"`
	} `json:"port"`
}

// leafView is what the leaf reports for a port: the remote system name,
// the port ID with its type and the port description.
type leafView struct{ sysName, portIDType, portID, portDescr string }

func cablecheckLeafNeighbors(ctx context.Context) (map[string]leafView, error) {
	out, err := cablecheckExec(ctx, cablecheckLeaf, "lldpcli", "-f", "json0", "show", "neighbors")
	if err != nil {
		return nil, err
	}
	var doc struct {
		LLDP []struct {
			Interface []lldpdNeighbor `json:"interface"`
		} `json:"lldp"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("lldpcli json: %w: %s", err, out)
	}
	views := map[string]leafView{}
	for _, l := range doc.LLDP {
		for _, n := range l.Interface {
			var v leafView
			if len(n.Chassis) > 0 && len(n.Chassis[0].Name) > 0 {
				v.sysName = n.Chassis[0].Name[0].Value
			}
			if len(n.Port) > 0 {
				if len(n.Port[0].ID) > 0 {
					v.portIDType, v.portID = n.Port[0].ID[0].Type, n.Port[0].ID[0].Value
				}
				if len(n.Port[0].Descr) > 0 {
					v.portDescr = n.Port[0].Descr[0].Value
				}
			}
			views[n.Name] = v
		}
	}
	return views, nil
}

// cablecheckStartBooty starts BOOTy in the lab node once the leaf runs
// lldpd and both links exist, so it finds every NIC and the leaf gets the
// first frames. Its log goes to the container log.
func cablecheckStartBooty(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		_, errLeaf := cablecheckExec(ctx, cablecheckLeaf, "lldpcli", "show", "chassis")
		_, errLink := cablecheckExec(ctx, cablecheckBooty, "test", "-e", "/sys/class/net/"+cablecheckNICs[len(cablecheckNICs)-1])
		if errLeaf == nil && errLink == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("lab not ready: leaf %v, links %v", errLeaf, errLink)
		}
		time.Sleep(time.Second)
	}
	if _, err := cablecheckExec(ctx, cablecheckBooty, "pidof", "booty"); err == nil {
		return // already running
	}
	out, err := exec.CommandContext(ctx, "docker", "exec", "-d", cablecheckBooty,
		"sh", "-c", "/usr/local/bin/booty > /proc/1/fd/1 2>&1").CombinedOutput()
	if err != nil {
		t.Fatalf("start BOOTy: %v: %s", err, out)
	}
}

// bm4xMAC is Connection.MAC_ADDRESS_PATTERN of bm4x-api.
var bm4xMAC = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}([0-9A-Fa-f]{2})$`)

// The leaf sees each BOOTy NIC with its MAC as port ID (type mac), the
// interface name as port description and the system name bare-minimal:
// the neighbor that bm4x matches as a compute port
// (Connection.isToMacAddress).
func TestCableCheckLLDP(t *testing.T) {
	cablecheckStartBooty(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	want := map[string]leafView{}
	for _, nic := range cablecheckNICs {
		mac, err := cablecheckExec(ctx, cablecheckBooty, "cat", "/sys/class/net/"+nic+"/address")
		if err != nil {
			t.Fatal(err)
		}
		want[nic] = leafView{sysName: "bare-minimal", portIDType: "mac", portID: strings.TrimSpace(mac), portDescr: nic}
	}
	var got map[string]leafView
	for {
		var err error
		got, err = cablecheckLeafNeighbors(ctx)
		if err == nil && got["eth1"] == want["eth1"] && got["eth2"] == want["eth2"] {
			break
		}
		if ctx.Err() != nil {
			diag, _ := cablecheckExec(context.Background(), cablecheckLeaf, "sh", "-c",
				"lldpcli show interfaces; lldpcli show statistics; ps")
			t.Fatalf("leaf neighbors = %+v (%v), want %+v\nleaf:\n%s", got, err, want, diag)
		}
		time.Sleep(time.Second)
	}
	for port, v := range got {
		isToMac := (v.sysName == "bare-minimal" || v.sysName == "") && bm4xMAC.MatchString(v.portID)
		if !isToMac {
			t.Errorf("leaf port %s: bm4x would not match %+v as a compute port", port, v)
		}
	}
}

// With the vars of the cable check ISO, BOOTy keeps running after the
// checks (CHECK_HOLD=until-poweroff) and keeps sending.
func TestCableCheckHold(t *testing.T) {
	cablecheckStartBooty(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		out, err := exec.CommandContext(ctx, "docker", "logs", cablecheckBooty).CombinedOutput()
		if err == nil && strings.Contains(string(out), "checks done, holding until power off") {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("BOOTy does not hold after the checks: %v", err)
		}
		time.Sleep(time.Second)
	}
	if _, err := cablecheckExec(ctx, cablecheckBooty, "pidof", "booty"); err != nil {
		t.Fatalf("BOOTy is not running while it holds: %v", err)
	}
}
