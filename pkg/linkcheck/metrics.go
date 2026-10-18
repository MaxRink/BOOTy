package linkcheck

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
)

// Link check names.
const (
	CheckLinkSpeed     = "link-speed"
	CheckLinkFlaps     = "link-flaps"
	CheckLinkErrors    = "link-errors"
	CheckFEC           = "fec"
	CheckPCIeLink      = "pcie-link"
	CheckOpticsAlarm   = "optics-alarm"
	CheckOpticsWarning = "optics-warning"
)

// defaultMaxFlaps is the carrier losses since the driver loaded that are
// fine: the switch port may bounce while the host boots, BOOTy's link
// setup takes the link down and up, and the firmware LLDP agent switch
// resets the port of ice and i40e NICs.
const defaultMaxFlaps = 4

// linkChecks runs the link speed, flap, error, FEC and PCIe checks.
func linkChecks(links []Link, t *config.CheckThresholds) []health.CheckResult {
	maxFlaps := defaultMaxFlaps
	if t.MaxFlaps != nil {
		maxFlaps = *t.MaxFlaps
	}
	var speed, flaps, errs, fec, pci []string
	for i := range links {
		l := &links[i]
		if s := l.State; s != nil {
			speed = append(speed, speedProblems(l, t.MinSpeedMbps)...)
			if s.Flaps > maxFlaps {
				flaps = append(flaps, fmt.Sprintf("%s: carrier lost %d times", l.Ifname, s.Flaps))
			}
			if s.Errors != nil && s.Errors.RxErrors+s.Errors.TxErrors > t.MaxErrors {
				errs = append(errs, fmt.Sprintf("%s: %d receive errors (CRC %d, frame %d), %d transmit errors",
					l.Ifname, s.Errors.RxErrors, s.Errors.RxCRC, s.Errors.RxFrame, s.Errors.TxErrors))
			}
			fec = append(fec, fecProblems(l)...)
		}
		if p := l.PCIe; p != nil {
			if speed, width := pcieCap(p); gts(p.Speed) < gts(speed) || p.Width < width {
				pci = append(pci, fmt.Sprintf("%s: PCIe %s x%d, device and slot can %s x%d", l.Ifname, p.Speed, p.Width, speed, width))
			}
		}
	}
	warn := config.CheckActionWarn
	return []health.CheckResult{
		result(CheckLinkSpeed, action(t.LinkSpeed, warn), speed),
		result(CheckLinkFlaps, action(t.LinkFlaps, warn), flaps),
		result(CheckLinkErrors, action(t.LinkErrors, warn), errs),
		result(CheckFEC, action(t.FEC, warn), fec),
		result(CheckPCIeLink, action(t.PCIe, warn), pci),
	}
}

func speedProblems(l *Link, minMbps int) []string {
	s := l.State.SpeedMbps
	if !l.Carrier || s == 0 {
		return nil
	}
	var out []string
	if minMbps > 0 && s < minMbps {
		out = append(out, fmt.Sprintf("%s: %d Mb/s, want at least %d", l.Ifname, s, minMbps))
	}
	if l.Expected != nil && l.Expected.SpeedMbps > 0 && s != l.Expected.SpeedMbps {
		out = append(out, fmt.Sprintf("%s: %d Mb/s, want %d", l.Ifname, s, l.Expected.SpeedMbps))
	}
	return out
}

// fecProblems: uncorrectable blocks, or no FEC on a link whose lanes need
// it (fecRequired).
func fecProblems(l *Link) []string {
	f := l.State.FEC
	if f == nil {
		return nil
	}
	var out []string
	if f.Uncorrectable > 0 {
		out = append(out, fmt.Sprintf("%s: %d uncorrectable FEC blocks", l.Ifname, f.Uncorrectable))
	}
	if l.Carrier && f.Active == "off" && fecRequired(l) {
		out = append(out, fmt.Sprintf("%s: no FEC at %d Mb/s (%d Mb/s per lane)", l.Ifname, l.State.SpeedMbps, laneRate(l.State)))
	}
	return out
}

// laneRate is the speed of one lane: the speed divided by the lanes that
// the driver reports, else by the usual lane count of the speed (40G and
// 100G have 4 lanes, 50G 2, 400G 8). An unknown lane count of a PAM4 link
// (100G-R2 or -R1) gives 25G here, which still needs FEC: only the 25G
// exception codes of fecRequired turn it off, and those name NRZ modules
// with 4 (100G) or 1 (25G) lanes.
func laneRate(s *LinkState) int {
	lanes := s.Lanes
	if lanes == 0 {
		switch s.SpeedMbps {
		case 40000, 100000, 200000:
			lanes = 4
		case 50000:
			lanes = 2
		case 400000:
			lanes = 8
		default:
			lanes = 1
		}
	}
	return s.SpeedMbps / lanes
}

// fecRequired: PAM4 lanes (50G and more) always need RS-FEC. 25G lanes
// (25G, 50G-R2, 100G-R4 such as CR4 and SR4) need it (IEEE 802.3 clauses
// 91, 108, 110), except for the long-reach optics (LR, LR4, ER, ER4), the
// short CA-N cables and the BER 1e-12 AOC and ACC (SFF-8024 codes 0x03,
// 0x04, 0x0d, 0x18, 0x19). 10G lanes (10G, 40G) need none.
func fecRequired(l *Link) bool {
	switch rate := laneRate(l.State); {
	case rate >= 50000:
		return true
	case rate >= 25000:
		if m := l.Module; m != nil {
			switch m.Compliance {
			case 0x03, 0x04, 0x0d, 0x18, 0x19:
				return false
			}
		}
		return true
	}
	return false
}

// pcieCap is the best link that the device and its slot both support: a
// x16 device in a x8 slot (or a Gen4 device in a Gen3 slot) is not
// downtrained.
func pcieCap(p *PCIe) (speed string, width int) {
	speed, width = p.MaxSpeed, p.MaxWidth
	if p.SlotMaxSpeed != "" && gts(p.SlotMaxSpeed) < gts(speed) {
		speed = p.SlotMaxSpeed
	}
	if p.SlotMaxWidth > 0 && p.SlotMaxWidth < width {
		width = p.SlotMaxWidth
	}
	return speed, width
}

// gts parses a PCIe link speed such as "16.0 GT/s PCIe"; 0 when unknown.
func gts(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// opticsChecks reports the alarm and warning flags of the transceivers.
func opticsChecks(links []Link, t *config.CheckThresholds) []health.CheckResult {
	var alarms, warnings []string
	for i := range links {
		m := links[i].Module
		if m == nil {
			continue
		}
		for _, a := range m.Alarms {
			alarms = append(alarms, fmt.Sprintf("%s: %s %s alarm", links[i].Ifname, m.PartNumber, a))
		}
		for _, w := range m.Warnings {
			warnings = append(warnings, fmt.Sprintf("%s: %s %s warning", links[i].Ifname, m.PartNumber, w))
		}
	}
	return []health.CheckResult{
		result(CheckOpticsAlarm, action(t.OpticsAlarm, config.CheckActionBlock), alarms),
		result(CheckOpticsWarning, action(t.OpticsWarning, config.CheckActionWarn), warnings),
	}
}
