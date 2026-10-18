package linkcheck

import (
	"errors"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
	"github.com/telekom/BOOTy/pkg/network/sff"
)

func metricLinks() []Link {
	return []Link{
		{Ifname: "ens1f0np0", Carrier: true, Expected: &config.ExpectedLink{SpeedMbps: 25000},
			State: &LinkState{SpeedMbps: 10000, Flaps: 5, Errors: &Counters{RxErrors: 5, RxCRC: 5}, FEC: &FEC{Active: "rs", Uncorrectable: 2}},
			PCIe:  &PCIe{Speed: "8.0 GT/s PCIe", Width: 8, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 8}},
		{Ifname: "ens1f1np1", Carrier: true,
			State: &LinkState{SpeedMbps: 25000, Flaps: 2, Errors: &Counters{}, FEC: &FEC{Active: "off"}},
			PCIe:  &PCIe{Speed: "16.0 GT/s PCIe", Width: 4, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 8}},
		{Ifname: "eno1", State: &LinkState{SpeedMbps: 0, FEC: &FEC{Active: "off"}},
			PCIe: &PCIe{Speed: "5.0 GT/s PCIe", Width: 1, MaxSpeed: "5.0 GT/s PCIe", MaxWidth: 1}},
		// No carrier: the last known speed does not count.
		{Ifname: "eno2", State: &LinkState{SpeedMbps: 1000}},
	}
}

func TestLinkChecks(t *testing.T) {
	results := linkChecks(metricLinks(), &config.CheckThresholds{MinSpeedMbps: 25000})
	for _, tt := range []struct {
		name    string
		details []string
	}{
		{CheckLinkSpeed, []string{"ens1f0np0: 10000 Mb/s, want at least 25000", "ens1f0np0: 10000 Mb/s, want 25000"}},
		{CheckLinkFlaps, []string{"ens1f0np0: carrier lost 5 times"}},
		{CheckLinkErrors, []string{"ens1f0np0: 5 receive errors (CRC 5, frame 0), 0 transmit errors"}},
		{CheckFEC, []string{"ens1f0np0: 2 uncorrectable FEC blocks", "ens1f1np1: no FEC at 25000 Mb/s (25000 Mb/s per lane)"}},
		{CheckPCIeLink, []string{"ens1f0np0: PCIe 8.0 GT/s PCIe x8, device and slot can 16.0 GT/s PCIe x8", "ens1f1np1: PCIe 16.0 GT/s PCIe x4, device and slot can 16.0 GT/s PCIe x8"}},
	} {
		r := findCheck(t, results, tt.name)
		if r.Status != health.StatusFail || r.Severity != health.SeverityWarning || r.Details != strings.Join(tt.details, "; ") {
			t.Errorf("%s = %+v\n want details %q", tt.name, r, strings.Join(tt.details, "; "))
		}
	}

	// Limits: five flaps and five errors are fine with these limits;
	// block makes FEC critical.
	links := metricLinks()
	results = linkChecks(links, &config.CheckThresholds{MaxFlaps: new(5), MaxErrors: 5, FEC: "block"})
	for _, name := range []string{CheckLinkFlaps, CheckLinkErrors, CheckLinkSpeed} {
		if r := findCheck(t, results, name); name != CheckLinkSpeed && r.Status != health.StatusPass {
			t.Errorf("%s with higher limits = %+v", name, r)
		}
	}
	if r := findCheck(t, results, CheckFEC); r.Severity != health.SeverityCritical {
		t.Errorf("fec with block = %+v", r)
	}
	// Without a minimum speed only the expected speed counts.
	if r := findCheck(t, results, CheckLinkSpeed); r.Details != "ens1f0np0: 10000 Mb/s, want 25000" {
		t.Errorf("link-speed without minimum = %+v", r)
	}
}

func TestGTS(t *testing.T) {
	for in, want := range map[string]float64{"16.0 GT/s PCIe": 16, "2.5 GT/s": 2.5, "": 0, "Unknown": 0} {
		if got := gts(in); got != want {
			t.Errorf("gts(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOpticsChecks(t *testing.T) {
	links := []Link{
		{Ifname: "ens1f0np0", Module: &sff.Module{PartNumber: "FTLF8536P4BCL", Alarms: []string{"rx-power low"}, Warnings: []string{"rx-power low"}}},
		{Ifname: "ens1f1np1", Module: &sff.Module{PartNumber: "QSFP-100G-LR4", Warnings: []string{"temperature high"}}},
		{Ifname: "eno1"},
	}
	results := opticsChecks(links, &config.CheckThresholds{})
	if r := findCheck(t, results, CheckOpticsAlarm); r.Status != health.StatusFail || r.Severity != health.SeverityCritical ||
		r.Details != "ens1f0np0: FTLF8536P4BCL rx-power low alarm" {
		t.Errorf("alarm = %+v", r)
	}
	if r := findCheck(t, results, CheckOpticsWarning); r.Severity != health.SeverityWarning ||
		r.Details != "ens1f0np0: FTLF8536P4BCL rx-power low warning; ens1f1np1: QSFP-100G-LR4 temperature high warning" {
		t.Errorf("warning = %+v", r)
	}
	results = opticsChecks(links, &config.CheckThresholds{OpticsAlarm: "warn", OpticsWarning: "off"})
	if r := findCheck(t, results, CheckOpticsAlarm); r.Severity != health.SeverityWarning {
		t.Errorf("alarm with warn = %+v", r)
	}
	if r := findCheck(t, results, CheckOpticsWarning); r.Status != health.StatusSkip {
		t.Errorf("warning with off = %+v", r)
	}
}

// FEC is required by the lane rate, not the total speed: 40G (4x10G) and
// 100G-LR4 run without it, 100G-SR4 and 25G DACs need it, PAM4 lanes
// always do.
func TestFECRequired(t *testing.T) {
	for _, tt := range []struct {
		name       string
		speed      int
		lanes      int
		compliance byte
		want       bool
	}{
		{"10G", 10000, 0, 0, false},
		{"40G", 40000, 0, 0, false},
		{"40G 4 lanes", 40000, 4, 0, false},
		{"25G DAC", 25000, 0, 0, true},
		{"25G CA-N DAC", 25000, 1, 0x0d, false},
		{"25G-LR", 25000, 1, 0x03, false},
		{"100G-SR4", 100000, 4, 0x02, true},
		{"100G-LR4", 100000, 0, 0x03, false},
		{"100G-ER4", 100000, 4, 0x04, false},
		{"100G CWDM4", 100000, 4, 0x06, true},
		{"100G AOC 1e-12", 100000, 4, 0x18, false},
		{"50G-R2", 50000, 0, 0, true},
		{"50G PAM4", 50000, 1, 0x03, true},
		{"200G-R4", 200000, 0, 0, true},
		{"400G-R8", 400000, 0, 0x03, true},
	} {
		l := &Link{Carrier: true, State: &LinkState{SpeedMbps: tt.speed, Lanes: tt.lanes, FEC: &FEC{Active: "off"}}}
		if tt.compliance != 0 {
			l.Module = &sff.Module{Compliance: tt.compliance}
		}
		if got := fecRequired(l); got != tt.want {
			t.Errorf("%s: fecRequired = %v, want %v", tt.name, got, tt.want)
		}
		if got := len(fecProblems(l)) > 0; got != tt.want {
			t.Errorf("%s: FEC problem = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// From the EEPROM to the verdict: a 25G SFP28 needs FEC unless byte 36
// of its A0h page names a long-reach optic or a CA-N cable.
func TestFECRequiredFromSFPEEPROM(t *testing.T) {
	for _, tt := range []struct {
		name string
		code byte
		want bool
	}{
		{"25GBASE-SR", 0x02, true},
		{"25GBASE-LR", 0x03, false},
		{"25GBASE-ER", 0x04, false},
		{"25GBASE-CR CA-L", 0x0b, true},
		{"25GBASE-CR CA-S", 0x0c, true},
		{"25GBASE-CR CA-N", 0x0d, false},
		{"no code", 0x00, true},
	} {
		a0 := make([]byte, 128) // SFF-8472 A0h, no digital diagnostics (byte 92 = 0)
		a0[0], a0[36] = 0x03, tt.code
		mod, err := sff.Decode(func(addr, page, offset, length int) ([]byte, error) {
			if addr != 0x50 || page != 0 || offset+length > len(a0) {
				return nil, errors.New("no such page")
			}
			return a0[offset : offset+length], nil
		})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		l := &Link{Carrier: true, Module: mod, State: &LinkState{SpeedMbps: 25000, Lanes: 1, FEC: &FEC{Active: "off"}}}
		if got := fecRequired(l); got != tt.want {
			t.Errorf("%s: fecRequired = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The PCIe link counts as downtrained only below what both the device and
// its slot support; one lane less is downtrained.
func TestPCIeAgainstSlot(t *testing.T) {
	g4, g3 := "16.0 GT/s PCIe", "8.0 GT/s PCIe"
	for _, tt := range []struct {
		name string
		p    PCIe
		want bool
	}{
		{"full link", PCIe{Speed: g4, Width: 16, MaxSpeed: g4, MaxWidth: 16}, false},
		{"one lane less", PCIe{Speed: g4, Width: 15, MaxSpeed: g4, MaxWidth: 16}, true},
		{"x16 device in a x8 slot", PCIe{Speed: g4, Width: 8, MaxSpeed: g4, MaxWidth: 16, SlotMaxSpeed: g4, SlotMaxWidth: 8}, false},
		{"Gen4 device in a Gen3 slot", PCIe{Speed: g3, Width: 16, MaxSpeed: g4, MaxWidth: 16, SlotMaxSpeed: g3, SlotMaxWidth: 16}, false},
		{"x8 in a x16 slot, device x16", PCIe{Speed: g4, Width: 8, MaxSpeed: g4, MaxWidth: 16, SlotMaxSpeed: g4, SlotMaxWidth: 16}, true},
		{"Gen3 below a Gen4 slot", PCIe{Speed: g3, Width: 16, MaxSpeed: g4, MaxWidth: 16, SlotMaxSpeed: g4, SlotMaxWidth: 16}, true},
	} {
		r := findCheck(t, linkChecks([]Link{{Ifname: "eth0", PCIe: &tt.p}}, &config.CheckThresholds{}), CheckPCIeLink)
		if got := r.Status == health.StatusFail; got != tt.want {
			t.Errorf("%s: pcie-link %+v, want fail %v", tt.name, r, tt.want)
		}
	}
}

// Four carrier losses are the default limit, the fifth fails.
func TestDefaultMaxFlaps(t *testing.T) {
	for flaps, want := range map[int]bool{4: false, 5: true} {
		r := findCheck(t, linkChecks([]Link{{Ifname: "eth0", State: &LinkState{Flaps: flaps}}}, &config.CheckThresholds{}), CheckLinkFlaps)
		if got := r.Status == health.StatusFail; got != want {
			t.Errorf("%d flaps: %+v, want fail %v", flaps, r, want)
		}
	}
}

// The lane count of a speed when the driver does not report it.
func TestLaneRate(t *testing.T) {
	for speed, want := range map[int]int{10000: 10000, 25000: 25000, 40000: 10000, 50000: 25000, 100000: 25000, 200000: 50000, 400000: 50000} {
		if got := laneRate(&LinkState{SpeedMbps: speed}); got != want {
			t.Errorf("%d Mb/s: lane rate %d, want %d", speed, got, want)
		}
	}
}
