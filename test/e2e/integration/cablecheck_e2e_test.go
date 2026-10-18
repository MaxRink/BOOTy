//go:build e2e_cablecheck

// Cable check lab (topology-cablecheck.clab.yml): BOOTy sends LLDP with
// the bm4x-cablecheck identity, and the lldpd leaf shows what a switch
// reports to NSO, which the bm4x connection check reads.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
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
	// The checks start after LLDP_NEIGHBOR_WAIT (90 s): the management
	// NIC eth0 has carrier but no LLDP neighbor.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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
	// LLDP_NEIGHBOR_WAIT: eth1 and eth2 have the leaf, the management NIC
	// has none.
	out, _ := exec.CommandContext(ctx, "docker", "logs", cablecheckBooty).CombinedOutput()
	if !strings.Contains(string(out), `msg="no LLDP neighbor on NICs with carrier" nics=[eth0]`) {
		t.Errorf("neighbor wait did not report exactly eth0 without a neighbor")
	}
}

// BOOTy logs the lldpd leaf as neighbor of both NICs, with the port ID
// subtype, the port description and the management addresses, and never
// itself: it ignores the frames that it sends.
func TestCableCheckReceive(t *testing.T) {
	cablecheckStartBooty(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var log string
	for {
		out, err := exec.CommandContext(ctx, "docker", "logs", cablecheckBooty).CombinedOutput()
		log = string(out)
		found := 0
		for _, nic := range cablecheckNICs {
			for _, line := range strings.Split(log, "\n") {
				if strings.Contains(line, `msg="LLDP neighbor"`) && strings.Contains(line, "interface="+nic+" ") &&
					strings.Contains(line, "systemName=leaf01") && strings.Contains(line, "portDescription="+nic) &&
					strings.Contains(line, "portIDSubtype=") && !strings.Contains(line, "mgmtAddresses=[]") {
					found++
					break
				}
			}
		}
		if err == nil && found == len(cablecheckNICs) {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("BOOTy did not log the leaf on %v with port details:\n%s", cablecheckNICs, log)
		}
		time.Sleep(time.Second)
	}
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, `msg="LLDP neighbor"`) && strings.Contains(line, "systemName=bare-minimal") {
			t.Errorf("BOOTy reports its own frame as neighbor: %s", line)
		}
	}
}

// The lab vars are the vars of the cable check ISO plus the expected
// cabling and the sinks.
func TestCableCheckLabVars(t *testing.T) {
	iso, err := os.ReadFile("../../../hack/check-iso/vars")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.ReadFile("../clab/data/vars-cablecheck")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(iso), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && !strings.Contains(string(lab), line+"\n") {
			t.Errorf("lab vars miss %q of the check ISO", line)
		}
	}
}

// cablecheckFile waits for a file that BOOTy writes and returns it.
func cablecheckFile(t *testing.T, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for {
		out, err := exec.CommandContext(ctx, "docker", "exec", cablecheckBooty, "cat", path).Output()
		if err == nil {
			return out
		}
		if ctx.Err() != nil {
			t.Fatalf("%s not written: %v", path, err)
		}
		time.Sleep(time.Second)
	}
}

// The host side of the cable check: the bm4x report has one entry, eth2
// on leaf01 eth2 instead of eth3, and the result document fails on the
// cabling match (block) with the leaf as neighbor of eth1 and eth2.
func TestCableCheckDiff(t *testing.T) {
	cablecheckStartBooty(t)
	host, err := cablecheckExec(context.Background(), cablecheckBooty, "hostname")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Diff []map[string]map[string]string `json:"diff"`
	}
	b := cablecheckFile(t, "/run/booty/bm4x-connection-report.json")
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatalf("bm4x report %s: %v", b, err)
	}
	want := []map[string]map[string]string{{
		"a":         {"hostname": strings.TrimSpace(host), "interfaceName": "eth2"},
		"bactual":   {"hostname": "leaf01", "interfaceName": "eth2"},
		"bexpected": {"hostname": "leaf01", "interfaceName": "eth3"},
	}}
	if !reflect.DeepEqual(report.Diff, want) {
		t.Errorf("bm4x report = %s, want the eth2 entry only", b)
	}

	var doc struct {
		Status string `json:"status"`
		Checks []struct {
			Name, Status, Severity string
		} `json:"checks"`
		Links []struct {
			Ifname string `json:"ifname"`
			LLDP   *struct {
				SystemName string `json:"systemName"`
			} `json:"lldp"`
			State *struct {
				SpeedMbps int    `json:"speedMbps"`
				Duplex    string `json:"duplex"`
			} `json:"link"`
		} `json:"links"`
	}
	b = cablecheckFile(t, "/run/booty/check-result.json")
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result %s: %v", b, err)
	}
	failed := false
	for _, c := range doc.Checks {
		failed = failed || c.Name == "cabling-match" && c.Status == "fail" && c.Severity == "critical"
	}
	if doc.Status != "fail" || !failed {
		t.Errorf("result status %s, cabling-match failed %v: %s", doc.Status, failed, b)
	}
	leaf := 0
	for _, l := range doc.Links {
		if (l.Ifname == "eth1" || l.Ifname == "eth2") && l.LLDP != nil && l.LLDP.SystemName == "leaf01" &&
			l.State != nil && l.State.SpeedMbps == 10000 && l.State.Duplex == "full" { // veth over ethtool netlink
			leaf++
		}
	}
	if leaf != 2 {
		t.Errorf("leaf01 at 10000 Mb/s full duplex on %d of eth1, eth2: %s", leaf, b)
	}
}
