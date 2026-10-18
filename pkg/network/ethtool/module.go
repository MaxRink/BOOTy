//go:build linux

package ethtool

import (
	"fmt"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// The module EEPROM attributes of include/uapi/linux/ethtool_netlink.h
// (Linux 5.13+); x/sys/unix does not have them.
const (
	moduleOffset  = 2 // ETHTOOL_A_MODULE_EEPROM_OFFSET, u32
	moduleLength  = 3 // ETHTOOL_A_MODULE_EEPROM_LENGTH, u32
	modulePage    = 4 // ETHTOOL_A_MODULE_EEPROM_PAGE, u8
	moduleBank    = 5 // ETHTOOL_A_MODULE_EEPROM_BANK, u8
	moduleI2CAddr = 6 // ETHTOOL_A_MODULE_EEPROM_I2C_ADDRESS, u8
	moduleData    = 7 // ETHTOOL_A_MODULE_EEPROM_DATA, binary
)

// ReadModuleEEPROM reads length bytes at offset of a page (bank 0) of the
// transceiver memory at an I2C address (ETHTOOL_MSG_MODULE_EEPROM_GET,
// "ethtool -m IF page P offset O length L i2c A"). A read does not cross
// offset 128. It fails when the NIC has no module.
func ReadModuleEEPROM(ifname string, addr, page, offset, length int) ([]byte, error) {
	msgs, err := request(unix.ETHTOOL_MSG_MODULE_EEPROM_GET, 0, header(ifname, 0),
		nl.NewRtAttr(moduleOffset, nl.Uint32Attr(uint32(offset))), //nolint:gosec // 0-255
		nl.NewRtAttr(moduleLength, nl.Uint32Attr(uint32(length))), //nolint:gosec // 1-128
		nl.NewRtAttr(modulePage, []byte{byte(page)}),
		nl.NewRtAttr(moduleBank, []byte{0}),
		nl.NewRtAttr(moduleI2CAddr, []byte{byte(addr)}))
	if err != nil {
		return nil, fmt.Errorf("module EEPROM of %s: %w", ifname, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("module EEPROM of %s: no reply", ifname)
	}
	return moduleDataOf(msgs[0])
}

func moduleDataOf(b []byte) ([]byte, error) {
	attrs, err := parse(b)
	if err != nil {
		return nil, err
	}
	for _, a := range attrs {
		if a.typ == moduleData {
			return a.value, nil
		}
	}
	return nil, fmt.Errorf("module EEPROM reply without data")
}
