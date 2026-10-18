package sff

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// The fixtures are module memories built from the specifications: field
// offsets of SFF-8472 rev 12.4 (A0h, A2h), SFF-8636 rev 2.10a (lower page,
// pages 00h and 03h) and CMIS 5.2 (lower page, pages 00h, 01h, 02h, 11h),
// codes of SFF-8024. No real dumps were available; values are typical for
// the module types.

// memory is a module memory: 256 bytes per I2C address and page.
type memory map[[2]int]*[256]byte

func (m memory) page(addr, page int) *[256]byte {
	k := [2]int{addr, page}
	if m[k] == nil {
		m[k] = &[256]byte{}
	}
	return m[k]
}

func (m memory) set(addr, page, offset int, b ...byte) { copy(m.page(addr, page)[offset:], b) }

func (m memory) str(addr, page, offset, size int, s string) {
	b := []byte(fmt.Sprintf("%-*s", size, s))
	m.set(addr, page, offset, b[:size]...)
}

// u16s encodes 16-bit values big endian.
func u16s(v ...uint16) []byte {
	b := make([]byte, 0, 2*len(v))
	for _, x := range v {
		b = append(b, byte(x>>8), byte(x))
	}
	return b
}

// reader serves the memory like ETHTOOL_MSG_MODULE_EEPROM_GET; pages that
// do not exist fail.
func (m memory) reader(t *testing.T) Reader {
	t.Helper()
	return func(addr, page, offset, length int) ([]byte, error) {
		if offset < 128 && page != 0 || offset%128+length > 128 {
			t.Fatalf("read crosses a page: page %d offset %d length %d", page, offset, length)
		}
		p := m[[2]int{addr, page}]
		if p == nil {
			return nil, errors.New("no such page")
		}
		return append([]byte(nil), p[offset:offset+length]...), nil
	}
}

// sfp28 is a 25G SR SFP28 with internally calibrated diagnostics and a
// receive power below its low alarm limit.
func sfp28() memory {
	m := memory{}
	m.set(0x50, 0, 0, 0x03)
	m.set(0x50, 0, 2, 0x07) // LC
	m.str(0x50, 0, 20, 16, "FINISAR CORP.")
	m.set(0x50, 0, 37, 0x00, 0x90, 0x65)
	m.str(0x50, 0, 40, 16, "FTLF8536P4BCL")
	m.set(0x50, 0, 36, 0x02) // 25GBASE-SR
	m.str(0x50, 0, 56, 4, "A")
	m.set(0x50, 0, 60, u16s(850)...)
	m.str(0x50, 0, 68, 16, "PXK1ABC")
	m.str(0x50, 0, 84, 8, "210315AB")
	m.set(0x50, 0, 92, 0x68) // DDM implemented, internally calibrated
	m.set(0x51, 0, 0, u16s(
		0x4b00, 0xfb00, 0x4600, 0x0000, // temperature 75, -5, 70, 0 °C
		36000, 30000, 35000, 31000, // vcc 3.6, 3.0, 3.5, 3.1 V
		6000, 1000, 5750, 1500, // bias 12, 2, 11.5, 3 mA
		10000, 1000, 7943, 1259, // tx power 0, -10, -1, -9 dBm
		10000, 100, 5000, 200, // rx power 0, -20, -3.01, -16.99 dBm
	)...)
	m.set(0x51, 0, 96, u16s(0x2380, 33000, 3500, 5000, 40)...) // 35.5 °C, 3.3 V, 7 mA, -3.01 dBm, -23.98 dBm
	m.set(0x51, 0, 113, 0x40)                                  // rx power low alarm
	m.set(0x51, 0, 117, 0x40)                                  // rx power low warning
	return m
}

// qsfp28 is a 100G LR4 QSFP28 (paged memory) with a lane 2 receive power
// low alarm and a temperature high warning.
func qsfp28() memory {
	m := memory{}
	m.set(0x50, 0, 0, 0x11)
	m.set(0x50, 0, 6, 0x20)                                 // temperature high warning
	m.set(0x50, 0, 9, 0x04)                                 // lane 2 rx power low alarm
	m.set(0x50, 0, 22, u16s(0x2800)...)                     // 40 °C
	m.set(0x50, 0, 26, u16s(33000)...)                      // 3.3 V
	m.set(0x50, 0, 34, u16s(8000, 0, 5000, 6310)...)        // rx power
	m.set(0x50, 0, 42, u16s(20000, 20000, 20000, 20000)...) // 40 mA
	m.set(0x50, 0, 50, u16s(10000, 10000, 10000, 10000)...) // 0 dBm
	m.set(0x50, 0, 128, 0x11)
	m.set(0x50, 0, 130, 0x07)
	m.str(0x50, 0, 148, 16, "Arista Networks")
	m.set(0x50, 0, 165, 0x00, 0x1c, 0x73)
	m.str(0x50, 0, 168, 16, "QSFP-100G-LR4")
	m.str(0x50, 0, 184, 2, "01")
	m.set(0x50, 0, 186, u16s(26200)...) // 1310 nm
	m.set(0x50, 0, 192, 0x03)           // 100GBASE-LR4
	m.str(0x50, 0, 196, 16, "XYZ123")
	m.str(0x50, 0, 212, 8, "190102")
	m.set(0x50, 0, 220, 0x3c)
	m.set(0x50, 3, 128, u16s(0x4b00, 0xfb00, 0x4600, 0x0000)...) // temperature
	m.set(0x50, 3, 144, u16s(36000, 30000, 35000, 31000)...)     // vcc
	m.set(0x50, 3, 176, u16s(25119, 316, 12589, 794)...)         // rx power 4, -15, 1, -11 dBm
	m.set(0x50, 3, 184, u16s(50000, 5000, 45000, 7500)...)       // bias 100, 10, 90, 15 mA
	m.set(0x50, 3, 192, u16s(31623, 1000, 15849, 1585)...)       // tx power 5, -10, 2, -8 dBm
	return m
}

// qsfpDD is a 400G DR4 QSFP-DD (CMIS, paged) with a temperature high
// alarm, a lane 3 receive power low warning and Tx bias scale x2.
func qsfpDD() memory {
	m := memory{}
	m.set(0x50, 0, 0, 0x18)
	m.set(0x50, 0, 9, 0x01) // temperature high alarm
	m.set(0x50, 0, 14, u16s(0x4c00, 32900)...)
	m.str(0x50, 0, 129, 16, "INNOLIGHT")
	m.set(0x50, 0, 145, 0x44, 0x7c, 0x7f)
	m.str(0x50, 0, 148, 16, "T-DP4CNT-NCI")
	m.str(0x50, 0, 164, 2, "1A")
	m.str(0x50, 0, 166, 16, "INKBD1234")
	m.str(0x50, 0, 182, 8, "230830")
	m.set(0x50, 0, 203, 0x0c) // MPO 1x12
	m.set(0x50, 1, 138, u16s(26220)...)
	m.set(0x50, 1, 160, 0x08) // Tx bias x2
	m.set(0x50, 2, 128, u16s(0x4b00, 0xfb00, 0x4600, 0x0000)...)
	m.set(0x50, 2, 136, u16s(36000, 30000, 35000, 31000)...)
	m.set(0x50, 2, 176, u16s(31623, 1000, 15849, 1585)...)
	m.set(0x50, 2, 184, u16s(25000, 2500, 22500, 3750)...) // 50, 5, 45, 7.5 mA before x2
	m.set(0x50, 2, 192, u16s(25119, 316, 12589, 794)...)
	p := m.page(0x50, 0x11)
	for l := range 8 {
		copy(p[154+2*l:], u16s(10000))
		copy(p[170+2*l:], u16s(4000)) // 8 mA, 16 mA with x2
		copy(p[186+2*l:], u16s(8000))
	}
	copy(p[190:], u16s(700)) // lane 3: -11.55 dBm
	p[152] = 0x04            // lane 3 rx power low warning
	return m
}

func TestDecodeSFP28(t *testing.T) {
	got, err := Decode(sfp28().reader(t))
	if err != nil {
		t.Fatal(err)
	}
	want := &Module{
		Type: "SFP", Spec: "SFF-8472", Connector: "LC", Vendor: "FINISAR CORP.", OUI: "00:90:65",
		PartNumber: "FTLF8536P4BCL", Revision: "A", Serial: "PXK1ABC", DateCode: "2021-03-15", WavelengthNm: 850,
		Compliance: 0x02, ComplianceName: "100GBASE-SR4 or 25GBASE-SR",
		DOM: true, TempC: 35.5, VccV: 3.3, Lanes: []Lane{{BiasMA: 7, TxPowerDBm: -3.01, RxPowerDBm: -23.98}},
		Thresholds: &Thresholds{
			TempC: Range{75, -5, 70, 0}, VccV: Range{3.6, 3, 3.5, 3.1}, BiasMA: Range{12, 2, 11.5, 3},
			TxPowerDBm: Range{0, -10, -1, -9}, RxPowerDBm: Range{0, -20, -3.01, -16.99},
		},
		Alarms: []string{"rx-power low"}, Warnings: []string{"rx-power low"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SFP28\n got  %+v\n want %+v", got, want)
	}
}

func TestDecodeQSFP28(t *testing.T) {
	got, err := Decode(qsfp28().reader(t))
	if err != nil {
		t.Fatal(err)
	}
	want := &Module{
		Type: "QSFP28", Spec: "SFF-8636", Connector: "LC", Vendor: "Arista Networks", OUI: "00:1c:73",
		PartNumber: "QSFP-100G-LR4", Revision: "01", Serial: "XYZ123", DateCode: "2019-01-02", WavelengthNm: 1310,
		Compliance: 0x03, ComplianceName: "100GBASE-LR4 or 25GBASE-LR",
		DOM: true, TempC: 40, VccV: 3.3,
		Lanes: []Lane{
			{BiasMA: 40, TxPowerDBm: 0, RxPowerDBm: -0.97}, {BiasMA: 40, TxPowerDBm: 0, RxPowerDBm: -40},
			{BiasMA: 40, TxPowerDBm: 0, RxPowerDBm: -3.01}, {BiasMA: 40, TxPowerDBm: 0, RxPowerDBm: -2},
		},
		Thresholds: &Thresholds{
			TempC: Range{75, -5, 70, 0}, VccV: Range{3.6, 3, 3.5, 3.1}, RxPowerDBm: Range{4, -15, 1, -11},
			BiasMA: Range{100, 10, 90, 15}, TxPowerDBm: Range{5, -10, 2, -8},
		},
		Alarms: []string{"rx-power lane 2 low"}, Warnings: []string{"temperature high"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("QSFP28\n got  %+v\n want %+v", got, want)
	}
}

func TestDecodeQSFPDD(t *testing.T) {
	got, err := Decode(qsfpDD().reader(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "QSFP-DD" || got.Spec != "CMIS" || got.Connector != "MPO 1x12" || got.Vendor != "INNOLIGHT" ||
		got.OUI != "44:7c:7f" || got.PartNumber != "T-DP4CNT-NCI" || got.Revision != "1A" || got.Serial != "INKBD1234" ||
		got.DateCode != "2023-08-30" || got.WavelengthNm != 1311 || !got.DOM || got.TempC != 76 || got.VccV != 3.29 {
		t.Errorf("identity = %+v", got)
	}
	if len(got.Lanes) != 8 || got.Lanes[0] != (Lane{BiasMA: 16, TxPowerDBm: 0, RxPowerDBm: -0.97}) || got.Lanes[2].RxPowerDBm != -11.55 {
		t.Errorf("lanes = %+v", got.Lanes)
	}
	if got.Thresholds.BiasMA != (Range{100, 10, 90, 15}) || got.Thresholds.RxPowerDBm != (Range{4, -15, 1, -11}) {
		t.Errorf("thresholds = %+v", got.Thresholds)
	}
	if !reflect.DeepEqual(got.Alarms, []string{"temperature high"}) || !reflect.DeepEqual(got.Warnings, []string{"rx-power lane 3 low"}) {
		t.Errorf("alarms %v, warnings %v", got.Alarms, got.Warnings)
	}
}

// A passive DAC has flat memory: no diagnostics and no further pages.
func TestDecodeFlat(t *testing.T) {
	q := memory{}
	q.set(0x50, 0, 0, 0x11)
	q.set(0x50, 0, 2, 0x04) // flat memory
	q.set(0x50, 0, 128, 0x11)
	q.set(0x50, 0, 130, 0x23) // no separable connector
	got, err := Decode(q.reader(t))
	if err != nil || got.DOM || got.Connector != "no separable connector" || got.Lanes != nil {
		t.Fatalf("QSFP DAC = %+v, %v", got, err)
	}
	// An active cable with flat memory has diagnostics but no page 03h
	// (the reader fails for it), so no thresholds.
	q.set(0x50, 0, 220, 0x30)
	q.set(0x50, 0, 22, u16s(0x2000)...)
	got, err = Decode(q.reader(t))
	if err != nil || !got.DOM || got.TempC != 32 || got.Thresholds != nil {
		t.Fatalf("QSFP AOC = %+v, %v", got, err)
	}
	c := memory{}
	c.set(0x50, 0, 0, 0x18)
	c.set(0x50, 0, 2, 0x80) // flat memory
	c.set(0x50, 0, 203, 0x23)
	got, err = Decode(c.reader(t))
	if err != nil || got.DOM || got.Lanes != nil {
		t.Fatalf("QSFP-DD DAC = %+v, %v", got, err)
	}
}

func TestDecodeSFPLimits(t *testing.T) {
	m := sfp28()
	m.set(0x50, 0, 92, 0x50) // externally calibrated
	got, err := Decode(m.reader(t))
	if err != nil || got.DOM || len(got.Notes) != 1 {
		t.Fatalf("externally calibrated = %+v, %v", got, err)
	}
	m.set(0x50, 0, 0, 0x7f)
	if _, err := Decode(m.reader(t)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown identifier: %v", err)
	}
	if _, err := Decode(memory{}.reader(t)); err == nil {
		t.Fatal("no module decoded")
	}
}

// SFP28 modules carry the SFF-8024 extended compliance code in A0h byte
// 36 (SFF-8472 table 5-3); the FEC rule needs it for 25G LR and CA-N.
func TestDecodeSFP28Compliance(t *testing.T) {
	for _, tt := range []struct {
		code byte
		want string
	}{
		{0x02, "100GBASE-SR4 or 25GBASE-SR"},
		{0x03, "100GBASE-LR4 or 25GBASE-LR"},
		{0x0b, "100GBASE-CR4 or 25GBASE-CR CA-L"},
		{0x0c, "25GBASE-CR CA-S"},
		{0x0d, "25GBASE-CR CA-N"},
		{0x7f, "0x7f"},
	} {
		m := sfp28()
		m.set(0x50, 0, 36, tt.code)
		got, err := Decode(m.reader(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.Compliance != tt.code || got.ComplianceName != tt.want {
			t.Errorf("byte 36 = 0x%02x: compliance 0x%02x %q, want %q", tt.code, got.Compliance, got.ComplianceName, tt.want)
		}
	}
}

// clearOnRead clears flag bytes of a page after their first read, like the
// latched flags of SFF-8636 and CMIS modules.
func clearOnRead(t *testing.T, m memory, page int, offsets ...int) Reader {
	t.Helper()
	r := m.reader(t)
	return func(addr, p, offset, length int) ([]byte, error) {
		b, err := r(addr, p, offset, length)
		if addr == 0x50 && p == page {
			for _, o := range offsets {
				if o >= offset && o < offset+length {
					m.page(0x50, page)[o] = 0
				}
			}
		}
		return b, err
	}
}

// A latched flag of a past event (cleared by the first read) is not
// reported; a condition that still holds is.
func TestDecodeLatchedFlags(t *testing.T) {
	got, err := Decode(clearOnRead(t, qsfp28(), 0, 6)) // temperature warning latched only
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Alarms, []string{"rx-power lane 2 low"}) || got.Warnings != nil {
		t.Errorf("SFF-8636: alarms %v, warnings %v", got.Alarms, got.Warnings)
	}
	m := qsfpDD()
	got, err = Decode(clearOnRead(t, m, 0, 9)) // temperature alarm latched only
	if err != nil {
		t.Fatal(err)
	}
	if got.Alarms != nil || !reflect.DeepEqual(got.Warnings, []string{"rx-power lane 3 low"}) {
		t.Errorf("CMIS module flags: alarms %v, warnings %v", got.Alarms, got.Warnings)
	}
	got, err = Decode(clearOnRead(t, qsfpDD(), 0x11, 152)) // rx power low warning latched only
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Alarms, []string{"temperature high"}) || got.Warnings != nil {
		t.Errorf("CMIS lane flags: alarms %v, warnings %v", got.Alarms, got.Warnings)
	}
}
