// Package sff decodes the identity, the digital diagnostics (DOM) and the
// alarm and warning flags of pluggable transceivers from their management
// memory: SFF-8472 (SFP, SFP+, SFP28), SFF-8636 (QSFP+, QSFP28) and CMIS
// 4/5 (QSFP-DD, OSFP, QSFP with CMIS). Codes are from SFF-8024.
package sff

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Reader reads length bytes at offset from a page of the module memory at
// an I2C address (0x50, or 0x51 for the SFF-8472 diagnostics). Offsets
// below 128 are the lower page; a read does not cross offset 128. This is
// the contract of ETHTOOL_MSG_MODULE_EEPROM_GET.
type Reader func(addr, page, offset, length int) ([]byte, error)

// Module is a decoded transceiver.
type Module struct {
	Type         string  `json:"type"` // SFP, QSFP+, QSFP28, QSFP-DD, OSFP, QSFP-CMIS
	Spec         string  `json:"spec"` // SFF-8472, SFF-8636 or CMIS
	Connector    string  `json:"connector,omitempty"`
	Vendor       string  `json:"vendor,omitempty"`
	OUI          string  `json:"oui,omitempty"`
	PartNumber   string  `json:"partNumber,omitempty"`
	Revision     string  `json:"revision,omitempty"`
	Serial       string  `json:"serial,omitempty"`
	DateCode     string  `json:"dateCode,omitempty"`
	WavelengthNm float64 `json:"wavelengthNm,omitempty"`
	// Compliance is the extended specification compliance code (SFF-8024
	// table 4-4) of SFP and QSFP modules, for example 0x03 for
	// 100GBASE-LR4 or 25GBASE-LR; 0 when the module sets none.
	Compliance     byte   `json:"complianceCode,omitempty"`
	ComplianceName string `json:"compliance,omitempty"`
	// DOM reports whether the module has digital diagnostics; the values
	// below are only set then.
	DOM        bool        `json:"dom"`
	TempC      float64     `json:"tempC,omitempty"`
	VccV       float64     `json:"vccV,omitempty"`
	Lanes      []Lane      `json:"lanes,omitempty"`
	Thresholds *Thresholds `json:"thresholds,omitempty"`
	// Alarms and Warnings are the flags that the module raised, for
	// example "rx-power low lane 2". SFF-8636 and CMIS flags are latched:
	// they also show an event since the last read.
	Alarms   []string `json:"alarms,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// Notes are decoder limits, for example external calibration.
	Notes []string `json:"notes,omitempty"`
}

// Lane holds the per-lane diagnostics.
type Lane struct {
	BiasMA     float64 `json:"biasMA"`
	TxPowerDBm float64 `json:"txPowerDBm"`
	RxPowerDBm float64 `json:"rxPowerDBm"`
}

// Thresholds are the alarm and warning limits that the module reports.
type Thresholds struct {
	TempC      Range `json:"tempC"`
	VccV       Range `json:"vccV"`
	BiasMA     Range `json:"biasMA"`
	TxPowerDBm Range `json:"txPowerDBm"`
	RxPowerDBm Range `json:"rxPowerDBm"`
}

// Range is one set of limits.
type Range struct {
	HighAlarm float64 `json:"highAlarm"`
	LowAlarm  float64 `json:"lowAlarm"`
	HighWarn  float64 `json:"highWarn"`
	LowWarn   float64 `json:"lowWarn"`
}

// ErrUnknown is returned for an identifier that the decoder does not know.
var ErrUnknown = errors.New("unknown module identifier")

// Decode reads and decodes a module.
func Decode(read Reader) (*Module, error) {
	lower, err := read(0x50, 0, 0, 128)
	if err != nil {
		return nil, fmt.Errorf("read module: %w", err)
	}
	if len(lower) < 128 {
		return nil, fmt.Errorf("read module: short lower page (%d bytes)", len(lower))
	}
	switch id := lower[0]; id {
	case 0x03:
		return decodeSFP(read, lower)
	case 0x0c, 0x0d, 0x11:
		return decode8636(read, lower)
	case 0x18, 0x19, 0x1e:
		return decodeCMIS(read, lower)
	default:
		return nil, fmt.Errorf("%w 0x%02x", ErrUnknown, id)
	}
}

var identifiers = map[byte]string{0x03: "SFP", 0x0c: "QSFP", 0x0d: "QSFP+", 0x11: "QSFP28", 0x18: "QSFP-DD", 0x19: "OSFP", 0x1e: "QSFP-CMIS"}

var connectors = map[byte]string{0x01: "SC", 0x07: "LC", 0x0b: "optical pigtail", 0x0c: "MPO 1x12", 0x0d: "MPO 2x16",
	0x21: "copper pigtail", 0x22: "RJ45", 0x23: "no separable connector", 0x24: "MXC 2x16", 0x25: "CS",
	0x26: "SN", 0x27: "MPO 2x12", 0x28: "MPO 1x16"}

// extendedCompliance names the codes of SFF-8024 table 4-4 that matter
// for 25G lanes.
var extendedCompliance = map[byte]string{
	0x01: "100G AOC or 25GAUI C2M AOC (BER 5e-5)", 0x02: "100GBASE-SR4 or 25GBASE-SR",
	0x03: "100GBASE-LR4 or 25GBASE-LR", 0x04: "100GBASE-ER4 or 25GBASE-ER", 0x05: "100GBASE-SR10",
	0x06: "100G CWDM4", 0x07: "100G PSM4", 0x08: "100G ACC or 25GAUI C2M ACC (BER 5e-5)",
	0x0b: "100GBASE-CR4 or 25GBASE-CR CA-L", 0x0c: "25GBASE-CR CA-S", 0x0d: "25GBASE-CR CA-N",
	0x18: "100G AOC or 25GAUI C2M AOC (BER 1e-12)", 0x19: "100G ACC or 25GAUI C2M ACC (BER 1e-12)",
}

func (m *Module) setCompliance(code byte) {
	if code == 0 {
		return
	}
	m.Compliance = code
	if n, ok := extendedCompliance[code]; ok {
		m.ComplianceName = n
	} else {
		m.ComplianceName = fmt.Sprintf("0x%02x", code)
	}
}

func connector(b byte) string {
	if c, ok := connectors[b]; ok {
		return c
	}
	return fmt.Sprintf("0x%02x", b)
}

// text is an ASCII field, space padded.
func text(b []byte) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, string(b)))
}

func oui(b []byte) string {
	if b[0]|b[1]|b[2] == 0 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x", b[0], b[1], b[2])
}

// date is a YYMMDD date code (and two lot code characters).
func date(b []byte) string {
	t := text(b[:6])
	if len(t) != 6 {
		return text(b)
	}
	return "20" + t[0:2] + "-" + t[2:4] + "-" + t[4:6]
}

func u16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// The DOM units of SFF-8472, SFF-8636 and CMIS.
func tempC(b []byte) float64  { return round(float64(int16(u16(b))) / 256) }
func vccV(b []byte) float64   { return round(float64(u16(b)) / 10000) }
func biasMA(b []byte) float64 { return round(float64(u16(b)) * 0.002) }

// powerDBm converts 0.1 µW units to dBm; no light is -40 dBm.
func powerDBm(b []byte) float64 {
	mw := float64(u16(b)) / 10000
	if mw <= 0 {
		return -40
	}
	return round(10 * math.Log10(mw))
}

func round(v float64) float64 { return math.Round(v*100) / 100 }

// rangeOf reads four limits (high alarm, low alarm, high warning, low
// warning) of 2 bytes each.
func rangeOf(b []byte, conv func([]byte) float64) Range {
	return Range{HighAlarm: conv(b[0:2]), LowAlarm: conv(b[2:4]), HighWarn: conv(b[4:6]), LowWarn: conv(b[6:8])}
}

// flag adds a raised flag to the alarms or warnings.
func (m *Module) flag(raised bool, what string, alarm bool) {
	if !raised {
		return
	}
	if alarm {
		m.Alarms = append(m.Alarms, what)
	} else {
		m.Warnings = append(m.Warnings, what)
	}
}

// flagSet adds the high/low alarm/warning flags of one value from four
// bits (high alarm, low alarm, high warning, low warning).
func (m *Module) flagSet(name string, highAlarm, lowAlarm, highWarn, lowWarn bool) {
	m.flag(highAlarm, name+" high", true)
	m.flag(lowAlarm, name+" low", true)
	m.flag(highWarn, name+" high", false)
	m.flag(lowWarn, name+" low", false)
}

func bit(b byte, n uint) bool { return b&(1<<n) != 0 }

// latched reads a page of SFF-8636 or CMIS flags again. These flags latch
// until the host reads them, so the first read also reports conditions
// that are long gone, such as the loss of signal at power-up or a Tx
// disable. The second read reports only what still holds.
func latched(read Reader, page, offset int) ([]byte, error) {
	b, err := read(0x50, page, offset, 128)
	if err != nil || len(b) < 128 {
		return nil, fmt.Errorf("read the flags of page %02xh again: %w", page, errShort(err, b))
	}
	return b, nil
}
