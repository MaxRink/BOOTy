package sff

import "fmt"

// decodeCMIS decodes CMIS 4/5: the lower page, upper page 00h (identity)
// and, unless the memory is flat, pages 01h (wavelength, bias scale), 02h
// (thresholds) and 11h bank 0 (lane monitors and flags).
func decodeCMIS(read Reader, lower []byte) (*Module, error) {
	up, err := read(0x50, 0, 128, 128)
	if err != nil || len(up) < 128 {
		return nil, fmt.Errorf("read CMIS page 00h: %w", errShort(err, up))
	}
	m := &Module{
		Type: identifiers[lower[0]], Spec: "CMIS", Connector: connector(up[203-128]),
		Vendor: text(up[129-128 : 145-128]), OUI: oui(up[145-128 : 148-128]), PartNumber: text(up[148-128 : 164-128]),
		Revision: text(up[164-128 : 166-128]), Serial: text(up[166-128 : 182-128]), DateCode: date(up[182-128 : 190-128]),
		DOM: true, TempC: tempC(lower[14:16]), VccV: vccV(lower[16:18]),
	}
	flags, err := latched(read, 0, 0)
	if err != nil {
		return nil, err
	}
	f := flags[9]
	m.flagSet("temperature", bit(f, 0), bit(f, 1), bit(f, 2), bit(f, 3))
	m.flagSet("vcc", bit(f, 4), bit(f, 5), bit(f, 6), bit(f, 7))
	if bit(lower[2], 7) { // flat memory: passive copper, no lane pages
		m.DOM = false
		return m, nil
	}
	pages := map[int][]byte{}
	for _, p := range []int{0x01, 0x02, 0x11} {
		b, err := read(0x50, p, 128, 128)
		if err != nil || len(b) < 128 {
			return nil, fmt.Errorf("read CMIS page %02xh: %w", p, errShort(err, b))
		}
		pages[p] = b
	}
	p1, p2, p11 := pages[0x01], pages[0x02], pages[0x11]
	p11flags, err := latched(read, 0x11, 128)
	if err != nil {
		return nil, err
	}
	m.WavelengthNm = float64(u16(p1[138-128:140-128])) * 0.05
	scale := float64(int(1) << ((p1[160-128] >> 3) & 0x3)) // TxBias scaling x1, x2, x4
	m.Thresholds = &Thresholds{
		TempC: rangeOf(p2[0:8], tempC), VccV: rangeOf(p2[8:16], vccV), TxPowerDBm: rangeOf(p2[48:56], powerDBm),
		BiasMA: scaled(rangeOf(p2[56:64], biasMA), scale), RxPowerDBm: rangeOf(p2[64:72], powerDBm),
	}
	lanes := 8
	if lower[0] == 0x1e {
		lanes = 4 // ponytail: lane count by form factor; read the media lane count of the application if a 4-lane QSFP-DD shows up
	}
	for l := range lanes {
		m.Lanes = append(m.Lanes, Lane{
			TxPowerDBm: powerDBm(p11[154-128+2*l:]), BiasMA: round(biasMA(p11[170-128+2*l:]) * scale), RxPowerDBm: powerDBm(p11[186-128+2*l:]),
		})
		lane := fmt.Sprintf(" lane %d", l+1)
		n := uint(l)
		// Page 11h flags, one bit per lane: Tx power 139-142, Tx bias
		// 143-146, Rx power 149-152 (high alarm, low alarm, high
		// warning, low warning).
		m.flagSet("tx-power"+lane, bit(p11flags[139-128], n), bit(p11flags[140-128], n), bit(p11flags[141-128], n), bit(p11flags[142-128], n))
		m.flagSet("tx-bias"+lane, bit(p11flags[143-128], n), bit(p11flags[144-128], n), bit(p11flags[145-128], n), bit(p11flags[146-128], n))
		m.flagSet("rx-power"+lane, bit(p11flags[149-128], n), bit(p11flags[150-128], n), bit(p11flags[151-128], n), bit(p11flags[152-128], n))
	}
	return m, nil
}

func scaled(r Range, s float64) Range {
	return Range{HighAlarm: round(r.HighAlarm * s), LowAlarm: round(r.LowAlarm * s), HighWarn: round(r.HighWarn * s), LowWarn: round(r.LowWarn * s)}
}
