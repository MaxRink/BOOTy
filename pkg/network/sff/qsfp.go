package sff

import "fmt"

// decode8636 decodes SFF-8636: the lower page, upper page 00h (identity)
// and, unless the memory is flat, page 03h (thresholds).
func decode8636(read Reader, lower []byte) (*Module, error) {
	up, err := read(0x50, 0, 128, 128)
	if err != nil || len(up) < 128 {
		return nil, fmt.Errorf("read QSFP page 00h: %w", errShort(err, up))
	}
	m := &Module{
		Type: identifiers[lower[0]], Spec: "SFF-8636", Connector: connector(up[130-128]),
		Vendor: text(up[148-128 : 164-128]), OUI: oui(up[165-128 : 168-128]), PartNumber: text(up[168-128 : 184-128]),
		Revision: text(up[184-128 : 186-128]), Serial: text(up[196-128 : 212-128]), DateCode: date(up[212-128 : 220-128]),
		WavelengthNm: float64(u16(up[186-128:188-128])) / 20,
	}
	m.setCompliance(up[192-128])
	if up[220-128]&0x30 == 0 { // no temperature and no supply voltage monitor
		return m, nil
	}
	m.DOM = true
	m.TempC, m.VccV = tempC(lower[22:24]), vccV(lower[26:28])
	flags, err := latched(read, 0, 0)
	if err != nil {
		return nil, err
	}
	for l := range 4 {
		m.Lanes = append(m.Lanes, Lane{
			RxPowerDBm: powerDBm(lower[34+2*l:]), BiasMA: biasMA(lower[42+2*l:]), TxPowerDBm: powerDBm(lower[50+2*l:]),
		})
	}
	m.flagSet("temperature", bit(flags[6], 7), bit(flags[6], 6), bit(flags[6], 5), bit(flags[6], 4))
	m.flagSet("vcc", bit(flags[7], 7), bit(flags[7], 6), bit(flags[7], 5), bit(flags[7], 4))
	// Lane flags: two lanes per byte, high nibble first, bits high alarm,
	// low alarm, high warning, low warning.
	for _, f := range []struct {
		name string
		at   int
	}{{"rx-power", 9}, {"tx-bias", 11}, {"tx-power", 13}} {
		for l := range 4 {
			b, shift := flags[f.at+l/2], uint(4-4*(l%2))
			m.flagSet(fmt.Sprintf("%s lane %d", f.name, l+1), bit(b, shift+3), bit(b, shift+2), bit(b, shift+1), bit(b, shift))
		}
	}
	if bit(lower[2], 2) { // flat memory: no page 03h
		return m, nil
	}
	p3, err := read(0x50, 3, 128, 128)
	if err != nil || len(p3) < 128 {
		return nil, fmt.Errorf("read QSFP page 03h: %w", errShort(err, p3))
	}
	m.Thresholds = &Thresholds{
		TempC: rangeOf(p3[0:8], tempC), VccV: rangeOf(p3[16:24], vccV), RxPowerDBm: rangeOf(p3[48:56], powerDBm),
		BiasMA: rangeOf(p3[56:64], biasMA), TxPowerDBm: rangeOf(p3[64:72], powerDBm),
	}
	return m, nil
}

// errShort is err, or an error for a short page.
func errShort(err error, b []byte) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("short page (%d bytes)", len(b))
}
