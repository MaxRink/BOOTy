# Dependency forks

BOOTy fixes bugs in its Go dependencies in forks under
[github.com/MaxRink](https://github.com/MaxRink). A fork is a branch
`booty/<version line>` from the upstream tag that BOOTy used, with the
fixes on top, tagged `<upstream version>-booty.N`. The fork keeps the
upstream module path in its `go.mod`; BOOTy selects it with a `replace`
directive:

| Module | Replace in `go.mod` | Fork branch |
|---|---|---|
| `github.com/osrg/gobgp/v4` | `github.com/MaxRink/gobgp/v4 v4.10.0-booty.10` | `booty/v4.10`, see [GoBGP version and fork](networking.md#gobgp-version-and-fork) |
| `github.com/gopacket/gopacket` | `github.com/MaxRink/gopacket v1.7.4-booty.2` | [`booty/v1.7`](https://github.com/MaxRink/gopacket/tree/booty/v1.7) |
| `github.com/vishvananda/netlink` | `github.com/MaxRink/netlink v1.3.1-booty.1` | [`booty/v1.3`](https://github.com/MaxRink/netlink/tree/booty/v1.3) |
| `github.com/insomniacslk/dhcp` | `github.com/MaxRink/dhcp v0.0.0-20260901064844-234b97448fae-booty.2` | [`booty/master`](https://github.com/MaxRink/dhcp/tree/booty/master) |

Dependabot ignores the forked modules (`.github/dependabot.yml`): an
upstream bump does not change the build while the `replace` is in place.
Move a fork to a new upstream release by hand: branch from the new tag,
pick the fixes that upstream does not have, tag `-booty.1`, and change
the `replace`.

## gopacket

BOOTy decodes LLDP with gopacket. It used `github.com/google/gopacket`
v1.1.19, which has had no release since 2020; the fork is based on the
maintained `github.com/gopacket/gopacket` v1.7.4. v1.7.1 and later also
fix the DHCPv4 decoder panic on a hardware address length above 16
(GHSA-8mcr-459q-5mx2 / CVE-2026-65819, still present in
google/gopacket).

| Fix | Effect without it | Upstream (gopacket/gopacket v1.7.4, master 11ee6a1) |
|---|---|---|
| Short LLDP TLVs give an error, not a panic: 802.1 VLAN name and protocol identity, 802.3 MDI power, LLDP-MED civic location, and the management address string and OID lengths (a string length of 0, or uint8 sums that wrap) | One crafted LLDPDU from the link panics the receiver; `Decode8021`, `Decode8023` and `DecodeMedia` run outside the panic recovery of `gopacket.NewPacket`. Before the fork, BOOTy (PID 1) skipped these TLVs itself and recovered panics in `parseLLDP` | Present; not reported yet |
| Chassis ID, Port ID and TTL must be the first three TLVs, in this order, each once; the End TLV is optional (IEEE 802.1AB-2016) | A misordered LLDPDU, or one without TTL TLV (decoded as TTL 0), is accepted; a second TTL TLV overwrites the first; an LLDPDU without End TLV is dropped. Before the fork, BOOTy checked the order and appended an End TLV itself | Present; [gopacket/gopacket#173](https://github.com/gopacket/gopacket/issues/173) |
| One zero byte after an LLDPDU without End TLV is Ethernet padding (booty.2) | A 45-byte LLDPDU without End TLV in a minimum-size frame gets one byte of padding and is dropped as truncated | Upstream rejects every LLDPDU without End TLV (#173) |

DHCP does not use gopacket: the DHCPv4 and DHCPv6 clients encode and
decode with `github.com/insomniacslk/dhcp` and build their broadcast
frames themselves (#835), so `github.com/google/gopacket` is no longer in
the build.

## netlink

| Fix | Effect without it | Upstream (v1.3.1, the latest release; main 3fdd742) |
|---|---|---|
| `LinkSubscribeWithOptions`, `AddrSubscribeWithOptions`, `RouteSubscribeWithOptions` and `NeighSubscribeWithOptions` close the netlink socket when the receive goroutine ends, not only when `done` is closed | After a receive error (ENOBUFS when updates overrun the socket buffer), the update channel closes but the socket and a goroutine stay until `done` is closed. BOOTy subscribes again after such an error to follow NICs for LLDP, so each error leaked a socket for the life of the process | Present; not reported yet |

## go-dhclient

`github.com/digineo/go-dhclient` v1.0.2 has known bugs (a lease never
expires when renewals fail; RENEWING requests are broadcast with server
identifier and requested address; no retransmission backoff). It is not
forked: #790 replaces it with BOOTy's own DHCPv4 client. Until then, BOOTy
works around the missing expiry with its own timer (#751).

## insomniacslk/dhcp

The DHCPv4 and DHCPv6 clients encode and decode their messages with
`github.com/insomniacslk/dhcp` ([message encoding](networking.md#message-encoding)).
The module has no releases; the fork starts at master 234b97448fae
(2026-09-01), so its version line is `master` and its tags carry the
pseudo-version of that commit.

| Fix | Effect without it | Upstream (master c6bb46ad3f3a, 2026-10-14) |
|---|---|---|
| `rfc1035label` follows a compression pointer to a name that ends with a pointer itself (RFC 1035 section 4.1.4) | A DHCPv4 search list (option 119) from a server that compresses names like `c.b.example`, `b.example`, `example` is rejected as a whole: no search domains | Present; not reported yet |
| `rfc1035label` rejects a pointer that does not point before its name and before the previous pointer of the name (the jump barrier of systemd-networkd), a pointer past the end, the reserved label types 01 and 10, and names over 255 octets | Forward pointers, pointers past the end (read as a partial name) and label lengths 64 to 191 are accepted, unlike in networkd | Present; not reported yet |
| `OptElapsedTime` sends 0xffff for 655.35 s and more (RFC 8415 section 21.9) | The elapsed time wraps around: 700 s goes out as 44.64 s. BOOTy does not depend on it: the client starts a new transaction long before | Present; not reported yet |

## When to drop a fork

Drop a `replace` when an upstream release contains all fixes of its
fork, then run the unit tests: `TestParseLLDPMandatoryTLVs` and
`TestParseLLDPShortOrgTLVs` (gopacket) and
`TestFollowLLDPMatchesResubscribeClosesSocket` (netlink),
`TestLeaseSearchDomains` and `TestMalformedRouteAndSearchOptions`
(dhcp, `pkg/network`) and `TestGolden` (`pkg/network/dhcp6`, the elapsed
time) fail with a release that lacks a fix.
`FuzzParseLLDP` does not: `parseLLDP` recovers decoder panics.
