package sff

import "fmt"

// decodeSFP decodes SFF-8472: the ID page at 0x50 (A0h) and the
// diagnostics at 0x51 (A2h).
func decodeSFP(read Reader, a0 []byte) (*Module, error) {
	m := &Module{
		Type: identifiers[a0[0]], Spec: "SFF-8472", Connector: connector(a0[2]),
		Vendor: text(a0[20:36]), OUI: oui(a0[37:40]), PartNumber: text(a0[40:56]), Revision: text(a0[56:60]),
		Serial: text(a0[68:84]), DateCode: date(a0[84:92]),
	}
	m.setCompliance(a0[36])
	if a0[8]&0x0c == 0 { // not a copper cable: bytes 60-61 are the wavelength
		m.WavelengthNm = float64(u16(a0[60:62]))
	}
	diag := a0[92]
	switch {
	case !bit(diag, 6):
		return m, nil // no digital diagnostics
	case bit(diag, 4):
		m.Notes = append(m.Notes, "externally calibrated diagnostics are not decoded")
		return m, nil
	}
	a2, err := read(0x51, 0, 0, 128)
	if err != nil {
		return nil, fmt.Errorf("read SFP diagnostics: %w", err)
	}
	if len(a2) < 128 {
		return nil, fmt.Errorf("read SFP diagnostics: short page (%d bytes)", len(a2))
	}
	m.DOM = true
	m.TempC, m.VccV = tempC(a2[96:98]), vccV(a2[98:100])
	m.Lanes = []Lane{{BiasMA: biasMA(a2[100:102]), TxPowerDBm: powerDBm(a2[102:104]), RxPowerDBm: powerDBm(a2[104:106])}}
	m.Thresholds = &Thresholds{
		TempC: rangeOf(a2[0:8], tempC), VccV: rangeOf(a2[8:16], vccV), BiasMA: rangeOf(a2[16:24], biasMA),
		TxPowerDBm: rangeOf(a2[24:32], powerDBm), RxPowerDBm: rangeOf(a2[32:40], powerDBm),
	}
	// Alarm flags in bytes 112-113, warning flags in 116-117, same bits.
	for i, alarm := range []bool{true, false} {
		f0, f1 := a2[112+4*i], a2[113+4*i]
		m.flag(bit(f0, 7), "temperature high", alarm)
		m.flag(bit(f0, 6), "temperature low", alarm)
		m.flag(bit(f0, 5), "vcc high", alarm)
		m.flag(bit(f0, 4), "vcc low", alarm)
		m.flag(bit(f0, 3), "tx-bias high", alarm)
		m.flag(bit(f0, 2), "tx-bias low", alarm)
		m.flag(bit(f0, 1), "tx-power high", alarm)
		m.flag(bit(f0, 0), "tx-power low", alarm)
		m.flag(bit(f1, 7), "rx-power high", alarm)
		m.flag(bit(f1, 6), "rx-power low", alarm)
	}
	return m, nil
}
