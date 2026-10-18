// Package linkcheck runs the cabling checks of check mode on every
// physical NIC: the LLDP neighbors against the expected cabling, and the
// link, PCIe and transceiver state, and writes one result document.
package linkcheck

import (
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
	"github.com/telekom/BOOTy/pkg/network/lldp"
	"github.com/telekom/BOOTy/pkg/network/sff"
)

// Schema is the version of the result document.
const Schema = "booty.check/v1"

// Document is the check result that the sinks send.
type Document struct {
	Schema     string               `json:"schema"`
	Mode       string               `json:"mode"`
	Profiles   []string             `json:"profiles"`
	StartedAt  time.Time            `json:"startedAt"`
	FinishedAt time.Time            `json:"finishedAt"`
	Host       Host                 `json:"host"`
	Status     string               `json:"status"` // pass, warn or fail
	Checks     []health.CheckResult `json:"checks"`
	Links      []Link               `json:"links,omitempty"`
	// Diff is the expected cabling diff in the bm4x ConnectionReport form.
	Diff []ConnectionEntry `json:"diff,omitempty"`
}

// Host identifies the machine. The serial number is left out, like in the
// hardware inventory.
type Host struct {
	Hostname     string `json:"hostname"`
	Product      string `json:"product,omitempty"`
	UUID         string `json:"uuid,omitempty"`
	BootyVersion string `json:"bootyVersion,omitempty"`
}

// Link is what BOOTy knows about one physical NIC.
type Link struct {
	Ifname   string               `json:"ifname"`
	MAC      string               `json:"mac"`
	PCI      string               `json:"pci,omitempty"`
	Driver   string               `json:"driver,omitempty"`
	Carrier  bool                 `json:"carrier"`
	State    *LinkState           `json:"link,omitempty"`
	PCIe     *PCIe                `json:"pcie,omitempty"`
	Module   *sff.Module          `json:"module,omitempty"`
	LLDP     *lldp.Neighbor       `json:"lldp,omitempty"`
	Expected *config.ExpectedLink `json:"expected,omitempty"`
}

// LinkState is the link layer state of a NIC: speed, duplex and
// autonegotiation (ethtool link modes), the link down reason, FEC, carrier
// losses and error counters since the driver loaded.
type LinkState struct {
	SpeedMbps int       `json:"speedMbps,omitempty"`
	Lanes     int       `json:"lanes,omitempty"`
	Duplex    string    `json:"duplex,omitempty"`
	Autoneg   bool      `json:"autoneg"`
	ExtState  string    `json:"extState,omitempty"`
	FEC       *FEC      `json:"fec,omitempty"`
	Flaps     int       `json:"flaps"` // carrier_down_count
	Errors    *Counters `json:"errors,omitempty"`
}

// FEC is the active forward error correction and its block counters.
type FEC struct {
	Active        string `json:"active,omitempty"`
	Corrected     uint64 `json:"corrected"`
	Uncorrectable uint64 `json:"uncorrectable"`
	Stats         bool   `json:"stats"`
}

// Counters are the receive and transmit error counters of a NIC
// (rtnl_link_stats64).
type Counters struct {
	RxErrors  uint64 `json:"rxErrors"`
	RxCRC     uint64 `json:"rxCRC"`
	RxFrame   uint64 `json:"rxFrame"`
	RxMissed  uint64 `json:"rxMissed"`
	RxDropped uint64 `json:"rxDropped"`
	TxErrors  uint64 `json:"txErrors"`
}

// PCIe is the PCIe link of the NIC function, its maximum and the maximum
// of the slot (the upstream port); a link can only train to the lower one.
type PCIe struct {
	Speed        string `json:"speed"` // for example "16.0 GT/s PCIe"
	Width        int    `json:"width"`
	MaxSpeed     string `json:"maxSpeed"`
	MaxWidth     int    `json:"maxWidth"`
	SlotMaxSpeed string `json:"slotMaxSpeed,omitempty"`
	SlotMaxWidth int    `json:"slotMaxWidth,omitempty"`
}

// Endpoint is one end of a connection in the bm4x ConnectionReport
// (ConnectionEntry.Endpoint). deviceName and deviceStatus, which only
// NetBox knows, are left out.
type Endpoint struct {
	Hostname      string `json:"hostname,omitempty"`
	InterfaceName string `json:"interfaceName,omitempty"`
}

// ConnectionEntry is one mismatch in the bm4x ConnectionReport.diff form:
// a is the host NIC, bactual the switch port that LLDP shows, bexpected
// the expected one. The names follow the bm4x JSON: Jackson writes the
// Lombok getters getBActual and getBExpected as "bactual" and "bexpected".
type ConnectionEntry struct {
	A         Endpoint `json:"a"`
	BActual   Endpoint `json:"bactual"`
	BExpected Endpoint `json:"bexpected"`
}
