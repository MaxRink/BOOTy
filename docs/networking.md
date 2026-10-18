# Networking

This guide describes how BOOTy brings up the network in the ramdisk. It is
the canonical place for network behaviour. For the variable names, defaults
and accepted values, see [configuration.md](configuration.md).

Related documents:

- [gobgp-evpn-guide.md](gobgp-evpn-guide.md): GoBGP quick start, interfaces
  and troubleshooting.
- [evpn-e2e-gap-analysis.md](evpn-e2e-gap-analysis.md): history of the GoBGP
  EVPN data-plane fixes and their e2e coverage.
- [e2e-test-matrix.md](e2e-test-matrix.md): e2e suites and make targets.

## Sequence

BOOTy runs these steps before the mode-specific work (`setupNetworkMode` and
`setupNetworkAndTokenFlow` in `main.go`):

1. Read the network values from `/deploy/vars`.
2. If netplan files exist, parse them and the optional FRR config. Netplan
   values override the vars (see [Netplan and FRR config input](#netplan-and-frr-config-input)).
3. Parse `VLANS`.
4. Create the link layers: the bond, then the VLANs.
5. Select and set up one network stack (see [Stack selection](#stack-selection)).
6. Write `/etc/resolv.conf` from the resolvers.
7. Run the connectivity check (see [Connectivity check](#connectivity-check)).
8. Get the JWT, if `TOKEN_URL` is set.

`MODE=dry-run` skips all of these steps.

## Stack selection

BOOTy selects one stack. The first match wins:

| Order | Stack | Condition | Setup failure |
|---|---|---|---|
| 1 | GoBGP | `NETWORK_MODE=gobgp` (case-insensitive), or netplan with an ASN and a VXLAN VNI | Fatal. No DHCP fallback. |
| 2 | FRR | `asn_server` > 0 and `underlay_ip` or `underlay_subnet` is set | Fall back to DHCP |
| 3 | Static | `STATIC_IP` is set | Tear down, then fall back to DHCP |
| 4 | DHCP | Always | Error is logged; the connectivity check then fails |

Notes:

- `NETWORK_MODE` accepts `gobgp`, `frr`, `static` and `dhcp`. Config
  validation rejects other values. Only `gobgp` changes the selection. The
  values `frr`, `static` and `dhcp` do not force a stack. For example,
  `NETWORK_MODE=dhcp` with `asn_server` and `underlay_ip` still selects FRR.
- Bond and VLAN are not stacks. They are link layers below the stack (see
  [Link layers](#link-layers-bond-and-vlan)).
- A bond or VLAN setup failure is fatal.

When the network setup fails, BOOTy reboots. With `CAPRF_LEGACY_COMPAT=true`,
BOOTy reports the error to CAPRF and keeps the ramdisk running.

## Link layers: bond and VLAN

BOOTy creates the link layers before the stack:

- **Bond.** `BOND_INTERFACES` creates `bond0` from the listed NICs.
  `BOND_MODE` accepts `802.3ad` (or `lacp`), `balance-rr`, `active-backup`
  and `balance-xor`. An unknown value logs a warning and uses `802.3ad`.
  Without `STATIC_IFACE` and without VLANs, static mode uses `bond0`.
- **VLAN.** `VLANS` uses the format `ID:parent[:address[:gateway]]`, with
  entries separated by commas. Wrap IPv6 address and gateway fields in
  brackets. The ID must be 1 to 4094. A VLAN without an address gets no
  address. Without `STATIC_IFACE`, static mode uses the first VLAN.

Teardown removes the VLANs first, then the bond.

## Netplan and FRR config input

BOOTy reads provisioner netplan files when they exist:

| File | Content |
|---|---|
| `/deploy/file-system/etc/netplan/*.yaml` or `*.yml` | Netplan v2 (ethernets, bonds, bridges, VLANs, VXLAN tunnels, dummy devices, VRFs, routes) |
| `/deploy/file-system/etc/frr/frr.conf` | Optional FRR config |

`network.devices` (`NETWORK_DEVICES`) is the native form of the netplan
files: the keys of their `network:` section, read by the same parser with
the same checks. When it is set, BOOTy uses it instead of the provisioner
netplan files (one info log line when both exist), so both forms give the
same network config. Native devices are always strict: an unknown key or
an unsupported route key fails.

GoBGP mode sets the sysctls of the legacy ramdisk: `ip_forward` (on, or
off with `no ip forwarding` in `frr.conf`) and IPv6 `forwarding` (off, the
kernel default that zebra and netplan keep, or on with `ipv6 forwarding`;
`IP_FORWARDING` and `IPV6_FORWARDING` set them natively), `rp_filter` 0 for
`all` and `default`, and `accept_ra` 2 with `accept_ra_defrtr` 1 for `all`
and `default`. Netplan `accept-ra` (`ACCEPT_RA_OFF`) then sets single NICs.

Both inputs are optional: without netplan files BOOTy uses the vars only,
and without `frr.conf` it uses the netplan values without FRR. A file that is
present but cannot be read or parsed is an error and network setup stops.
This covers a netplan path that is not a readable directory, a netplan file
that does not parse, and an `frr.conf` that cannot be read or does not
parse. BOOTy does not fall back to the vars in these cases, because that
would bring up a different network than the operator configured. A key or
statement that BOOTy does not implement is also an error, unless
`NETPLAN_STRICT` or `FRR_CONFIG_STRICT` is `false` (see
[Strict input and the accepted grammar](#strict-input-and-the-accepted-grammar)).

BOOTy takes these values from netplan:

- The first VXLAN tunnel with an `id` gives the provision VNI, the underlay
  IP (`local`), the VXLAN underlay link (`link`) and the VXLAN device name.
- A dummy device address with `/32` gives the underlay IP and the underlay
  dummy name.
- The first bridge that contains a tunnel gives the bridge name. A bridge
  that contains a tunnel and has an address gives the provision IP. The
  `macaddress` of the bridge gives the router MAC.
- The addresses of `lo` stay on `lo`, like the IPv6 `/128` provisioning
  address of the bm4x templates. GoBGP adds them instead of the overlay
  loopback address.
- The first other ethernet address gives the static IP; a `default` route
  gives the static gateway. `nameservers` give the DNS resolvers.
- VLANs, the first bond, and VRFs with a table.

BOOTy takes these values from `frr.conf`:

- `router bgp N` gives the ASN. `bgp router-id` gives the underlay IP when
  netplan does not.
- Every neighbor of every `router bgp` block becomes one GoBGP session (see
  [BGP sessions](#bgp-sessions)), also numbered peers in `169.254.0.0/16`
  and `127.0.0.0/8`, like FRR.
- `advertise-all-vni` and the L3VNIs select the EVPN route mode (see
  [EVPN route modes](#evpn-route-modes-gobgp)).
- `ip route P blackhole` and `ipv6 route P blackhole` give blackhole routes
  in the main table, with protocol 196 and metric 20 like zebra.
- When the provision VNI is an L3VNI, the `network` lines of its VRF give
  Type-5 routes for each address family that `advertise ipv4 unicast` or
  `advertise ipv6 unicast` selects (see
  [Routes BOOTy originates](#routes-booty-originates)).

### BGP sessions

GoBGP mode opens one session per `frr.conf` neighbor, or per entry of the
native `network.bgp.sessions` (`BGP_SESSIONS`). Both inputs go through the
same resolution:

- An attribute comes from the neighbor, else from its peer-group, else from
  the `frr defaults` profile (`BGP_DEFAULTS`). This follows FRR's
  `tests/bgpd/test_peer_attr.c`.
- `remote-as` is a number, `internal` (iBGP, the router AS) or `external`
  (any other AS, learned from the peer's OPEN).
- `local-as N no-prepend replace-as` is the only local-as mode; GoBGP sends
  only N and does not add it to received paths. Like FRR 10.3, the session
  is iBGP when the remote AS is the effective local AS (the local-as, else
  the router AS): `remote-as 65501` with `local-as 65501` is iBGP in AS 65501.
- `timers K H` and `timers connect N` are per session. Without them the
  profile applies (current FRR): traditional 60/180/30 s, datacenter 3/9/10 s.
- The activated address families are those of the neighbor and its
  peer-group. In `frr.conf`, IPv4 unicast is active unless `no bgp default
  ipv4-unicast`. Native sessions have no default family: list
  `ipv4-unicast` in `families` when the session needs it.
- The VRF of the `router bgp` block binds the session to that VRF.
- An interface neighbor is unnumbered: BOOTy finds the peer's IPv6
  link-local address on the interface. FRR's `neighbor PORT interface`
  accepts any address on the port; a GoBGP peer has one. So a relearner
  checks the unnumbered peers every second: a peer without session for
  3 s on a port with carrier, whose address no longer answers on the BGP
  port while another link-local neighbor does, moves to that address
  (the switch came back with another MAC). A switch that does not answer
  at setup gets its peer on a placeholder address that moves the same
  way. Deviation: this adds 3 to 4 s (the 3 s grace and the 1 s round) to
  the recovery after such a change; FRR follows the RA at once (lab: FRR
  1.5 to 2.8 s, BOOTy 5.3 to 6.6 s after a 95 s outage with a new MAC).
- `neighbor X allowas-in [N]` (native `allowASIn`, FRR's default N is 3)
  accepts paths with the own AS up to N times. GoBGP has one count per
  session, so a session uses the highest count of its families.
- `bgp ebgp-requires-policy` follows `frr defaults` like FRR 10.3
  (`bgpd/bgp_vty.c` `BGP_EBGP_REQUIRES_POLICY`, `bgpd/bgp_route.c`
  `bgp_inbound_policy_exists`): on with `traditional`, off with
  `datacenter` and without the line (the legacy daemons profile); `[no]
  bgp ebgp-requires-policy` wins. With it on, an eBGP session without a
  route-map in a family and direction exchanges no routes there; BOOTy logs each
  such session, family and direction at warning level, like FRR's
  `zlog_warn`. Native sessions follow
  the same default; `network.bgp.ebgpRequiresPolicy`
  (`BGP_EBGP_REQUIRES_POLICY`) sets it for all sessions, and it is off for
  the `BGP_PEER_MODE` peers unless set.
- `neighbor X route-map M in|out` in an address family (native `routeMaps`
  with `family`, `in`, `out`) sets the route-maps of the session in that
  family; the peer's value wins over its peer-group's. The route-maps come
  from `frr.conf` or from `network.bgp.routeMaps` (`BGP_ROUTE_MAPS`). A
  route-map that a session names but that is not defined stops the setup
  (fail-fast); FRR instead denies all routes of that peer in that
  direction. A community-list `deny` entry stops the setup too.
- Route-map coverage: the production template (bm4x-operator provcfg
  `frr.conf.tpl`) uses `route-map NAME permit|deny SEQ`, `set
  local-preference`, `set community ... additive`, and `match community`
  with a standard community-list of `permit` entries, and BOOTy implements
  all of them in both forms. No template uses the other constructs, so they
  are rejected: `match ip address prefix-list`, `ip prefix-list`, as-path
  lists and `match as-path`, `set metric`, `set as-path prepend`,
  `continue`, `on-match`, expanded community-lists, EVPN matches (`match
  evpn route-type|vni`) and community-list `deny` entries.
- `neighbor X bfd [profile P]` (native `bfd`, `bfdProfile`) runs BFD with
  the timers of the `bfd` / `profile P` block (native
  `network.bgp.bfdProfiles`, `BFD_PROFILES`). An unknown profile and unset
  values use the FRR defaults 300 ms, 300 ms, 3. Intervals outside 10 to
  60000 ms and a multiplier outside 2 to 255 fail in both forms. Like FRR
  10.3, the session is multihop (RFC 5883) only for an iBGP peer that is
  not on a connected network of its VRF. When BFD goes from Up to Down,
  BOOTy resets the BGP session and connects again after 1 s, like the FRR
  start timer. Known deviation: the peer gets Cease/Administrative Reset
  (subcode 4), not Cease/BFD Down (subcode 10, RFC 9384), because the
  GoBGP API sends no other subcode. Only the peer log shows it. Two
  timing deviations of GoBGP 3.37 remain, filed upstream:
  - when the peer ends the session (for example `clear bgp` on the
    leaf), GoBGP waits a fixed 5 s in Idle (`holdtimeIdle`), where FRR
    waits 1 s, and refuses connections in that time
    ([osrg/gobgp#3735](https://github.com/osrg/gobgp/issues/3735)). In
    the lab a leaf-initiated session to BOOTy took 36 s against 2.3 s for
    FRR (the leaf then retries after its connect timer); a GoBGP build
    with the idle hold time after reset also used there took 5.3 s;
  - GoBGP waits a random 1 to 2 s before the first connect after Idle
    ([osrg/gobgp#3736](https://github.com/osrg/gobgp/issues/3736)), where
    FRR connects at once. After a BFD reset BOOTy restored ECMP in 4.0 to
    4.4 s against 2.3 to 2.9 s for FRR; without the delay 2.7 s.
  BOOTy uses the released GoBGP without a fork.
- `bgp bestpath as-path multipath-relax` (native `network.bgp.multipathRelax`,
  `BGP_MULTIPATH_RELAX`) installs equal-cost paths with different AS paths
  of the same length as one multipath route.
- `[no] bgp fast-external-failover` (native
  `network.bgp.fastExternalFailover`, `BGP_FAST_EXTERNAL_FAILOVER`; on by
  default like FRR) stops an unnumbered session as soon as its NIC loses
  carrier. Native sessions without the setting keep a
  `no bgp fast-external-failover` of `frr.conf` in the same VRF.

GoBGP mode installs the learned IPv4 and IPv6 unicast routes like zebra:
into the table of each session's VRF (the main table without a VRF), with
protocol `bgp` and metric 20, as ECMP over the paths of equal cost. Equal
cost follows FRR 10.3: the same local preference, AS path length, origin
and peer type, the same MED for the same leftmost AS, and without
multipath-relax eBGP paths from the same neighbor AS or iBGP paths with the
same AS path. A next hop that is not on a connected network is resolved to
its connected gateway (zebra next hop tracking) and again on every kernel
route change. BOOTy takes the one gateway the kernel uses for it; zebra
installs every ECMP member of the resolving route. A next hop that does
not resolve is skipped with a warning at most once a minute. The first
event of a burst updates the kernel at once, the rest 200 ms later, and
every 30 s BOOTy reinstalls all routes, so routes the kernel dropped come
back. The session NICs get `ignore_routes_with_linkdown=1`, so a dead ECMP
member is skipped at once.

The merge rule of each key is in the table at the end of this subsection.

With these sessions, the vars `BGP_PEER_MODE`, `BGP_INTERFACES`,
`BGP_NEIGHBORS`, `BGP_REMOTE_ASN`, `BGP_KEEPALIVE` and `BGP_HOLD` no longer
apply. BOOTy logs a warning for each of them that is set. `local_asn` does
not apply either, because each session has its own `local-as`.

These errors stop the setup in both inputs and are not relaxed by
`FRR_CONFIG_STRICT=false`:

- a neighbor without remote-as;
- a local-as mode other than `no-prepend replace-as`;
- local-as equal to the router AS, or a local-as with `remote-as internal`.
  FRR 10.3 accepts the second as an asymmetric iBGP session (its OPEN
  carries the local-as, but it expects the router AS from the peer); GoBGP
  has one AS per side and can not do that. Use the local-as as a number
  for iBGP in that AS;
- `remote-as auto` (FRR 10), which BOOTy does not support;
- a member remote-as that differs from its peer-group's;
- `timers` with 0, or a hold time below 3 (FRR `timers 0 0` disables
  keepalives; GoBGP would replace 0 with 30/90);
- a session without an address family;
- native only: a session name that is not an IP address without
  `interface: true`, the same session name twice in one VRF, `interface` or
  `peerGroup` on a peer-group, a peer-group `vrf` that differs from its
  member's, and `peerGroups` or `defaults` without `sessions`.

The FRR stack (`NETWORK_MODE=frr`) does not use `BGP_SESSIONS`; BOOTy logs
a warning when it is set there.

These statements are parsed but not applied yet, and BOOTy logs one
warning per statement: `soft-reconfiguration inbound`,
`[no] bgp suppress-duplicates` (it changes no routes), and `rd` and
`route-target` of a `vni` block (BOOTy creates no L2VNI of its own, so they
are only checked).

Native config and parsed files merge like this: a native list
(`network.bgp.sessions`) replaces the parsed list as a whole, with one info
log line; there is no element-wise merge. `network.bgp.bfdProfiles`,
`network.bgp.routeMaps` and `network.bgp.communityLists` replace the
`frr.conf` lists the same way, also for the `frr.conf` sessions. In the
vars, an unknown field of a JSON list fails, like in the YAML config. A native scalar that is set
(`network.bgp.multipathRelax`) overrides the parsed value; unset keeps it.

A key that exists in earlier releases keeps its rule for CAPRF
compatibility: a value from netplan overrides the same value from the
vars, and an empty netplan value keeps the vars value. A key that this
release adds follows the native rule: a set native value wins, and a
non-empty native list replaces the parsed list as a whole (one info log
line).

`VLANS` is the exception: BOOTy parses it after the netplan merge, so
`VLANS` from the vars replaces the netplan VLANs. When netplan and the FRR
config give an ASN and a VNI, BOOTy sets the stack to GoBGP. This overrides
`NETWORK_MODE`.

The peer keys `BGP_LOCAL_AS`, `BGP_NEIGHBOR_VRF`, `BGP_CONNECT_RETRY` and
`BGP_ALLOWAS_IN` are defaults: they fill only the session fields that the
`frr.conf` or `BGP_SESSIONS` session and its peer-group do not set. The
rule of every key:

| Key | Parsed source | Native | Rule |
|-----|---------------|--------|------|
| `NETWORK_MODE`, `asn_server`, `provision_vni`, `underlay_ip`, `provision_ip`, `dns_resolver`, `STATIC_*`, `BOND_*`, `VRF_NAME`, `VRF_TABLE_ID`, `BGP_PEER_MODE`, `BGP_NEIGHBORS`, `BGP_REMOTE_ASN`, `EVPN_L2_ENABLED` | netplan, `frr.conf` | vars keys of earlier releases | netplan or `frr.conf` wins |
| `BGP_SESSIONS`, `BGP_PEER_GROUPS` | `frr.conf` neighbors | `network.bgp.sessions`, `network.bgp.peerGroups` | list replaces |
| `BGP_DEFAULTS` | `frr defaults` | `network.bgp.defaults` | native wins; the profile of the timers and of the other profile defaults (datacenter without both) |
| `allowASIn` (session field) | `neighbor X allowas-in [N]` | `network.bgp.sessions[].allowASIn` | part of the session list |
| `BGP_MULTIPATH_RELAX` | `bgp bestpath as-path multipath-relax` | `network.bgp.multipathRelax` | native wins |
| `BGP_FAST_EXTERNAL_FAILOVER` | `[no] bgp fast-external-failover` | `network.bgp.fastExternalFailover` | native wins, for every session and the `BGP_PEER_MODE` peers (off for them when unset) |
| `EVPN_TYPE5_ONLY` | provision VNI is an `frr.conf` L3VNI | `network.evpn.type5Only` | an explicit value wins |
| `LOOPBACK_ADDRESSES` | netplan `lo` addresses | `network.loopbackAddresses` | list replaces |
| `BLACKHOLE_ROUTES` | `frr.conf` `ip[v6] route P blackhole` | `network.blackholeRoutes` | list replaces |
| `EVPN_TYPE5_NETWORKS` | `frr.conf` `network` with `advertise ipv4\|ipv6 unicast` | `network.evpn.type5Networks` | list replaces |
| `VXLAN_NAME` | netplan tunnel name | `network.evpn.vxlanName` | native wins |
| `BRIDGE_NAME` | netplan bridge name | `network.evpn.bridgeName` | native wins |
| `BRIDGE_MAC` | netplan bridge `macaddress` | `network.evpn.bridgeMAC` | native wins |
| `VXLAN_LINK` | netplan tunnel `link` | `network.evpn.vxlanLink` | native wins |
| `UNDERLAY_DUMMY_NAME` | netplan dummy device | `network.evpn.underlayDummyName` | native wins |
| `MTU` | the largest netplan `mtu` | `network.mtu` | native wins |
| `OVERLAY_VRF_NAME`, `OVERLAY_VRF_TABLE_ID` | netplan VRF of the bridge | `network.vrf.overlayName`, `network.vrf.overlayTableID` | native wins |
| `BGP_INSTALL_ROUTES` | none: a BOOTy setting, zebra always installs | `network.bgp.installRoutes` | native only; unset is on with sessions, off without |
| `BGP_LOCAL_AS` | `neighbor X local-as N` | `network.bgp.localAS` | session default |
| `BGP_NEIGHBOR_VRF` | `router bgp N vrf X` | `network.bgp.neighborVRF` | session default |
| `BGP_CONNECT_RETRY` | `neighbor X timers connect N` | `network.bgp.connectRetry` | session default |
| `BGP_ALLOWAS_IN` | `neighbor X allowas-in N` | `network.bgp.allowASIn` | session default |
| `BFD_PROFILES` | `frr.conf` `bfd` / `profile NAME` | `network.bgp.bfdProfiles` | list replaces, also for the `frr.conf` sessions |
| `BFD_TRANSMIT_MS`, `BFD_RECEIVE_MS` | profile timers | `network.bgp.bfdTransmitMS`, `network.bgp.bfdReceiveMS` | native wins, for the sessions with BFD |
| `BFD_DETECT_MULTIPLIER` | profile `detect-multiplier` | `network.bgp.bfdDetectMultiplier` | native wins, for the sessions with BFD |
| `routeMaps` (session field) | `neighbor X route-map M in\|out` | `network.bgp.sessions[].routeMaps` | part of the session list |
| `BGP_ROUTE_MAPS`, `BGP_COMMUNITY_LISTS` | `frr.conf` `route-map`, `bgp community-list` | `network.bgp.routeMaps`, `network.bgp.communityLists` | list replaces |
| `BGP_ROUTE_MAP_IN`, `BGP_ROUTE_MAP_OUT` | session route-maps | `network.bgp.routeMapIn`, `network.bgp.routeMapOut` | native wins, for every session and family |
| `EVPN_RT_IMPORT`, `EVPN_RT_EXPORT` | `frr.conf` `route-target import\|export` | `network.evpn.importRouteTargets`, `network.evpn.exportRouteTargets` | list replaces |
| `vpn_rt` | `frr.conf` route targets, else FRR's auto route target of an `frr.conf` L3VNI | `network.evpn.vpnRT` | netplan wins: the `frr.conf` route targets win over `vpn_rt`, as in FRR; `vpn_rt` applies only without them |
| `BGP_EBGP_REQUIRES_POLICY` | `[no] bgp ebgp-requires-policy` | `network.bgp.ebgpRequiresPolicy` | native wins, for every session; unset is on for sessions, off for `BGP_PEER_MODE` peers |
| `NETWORK_DEVICES` | the provisioner netplan files | `network.devices` | replaces the netplan files as a whole |
| `VXLAN_MTU`, `VXLAN_MAC`, `VXLAN_LEARNING`, `VXLAN_ARP_PROXY`, `VXLAN_PORT_HAIRPIN`, `VXLAN_PORT_LEARNING`, `VXLAN_IPV6_LINK_LOCAL`, `VXLAN_ACCEPT_RA` | netplan tunnel `mtu`, `macaddress`, `mac-learning`, `arp-proxy`, `hairpin`, `port-mac-learning`, `link-local`, `accept-ra` | `network.overlay.*` | native wins |
| `BRIDGE_MTU`, `BRIDGE_IPV6_LINK_LOCAL`, `BRIDGE_ACCEPT_RA` | netplan bridge `mtu`, `link-local`, `accept-ra` | `network.overlay.*` | native wins |
| `BRIDGE_STP` | netplan bridge `parameters` (`stp`, on when the block has no `stp`) | `network.overlay.bridgeSTP` | native wins |
| `ACCEPT_RA_OFF`, `LLDP_EMIT` | netplan ethernet `accept-ra: false`, `emit-lldp: true` | `network.noAcceptRA`, `network.emitLLDP` | list replaces |
| `BGP_GRACEFUL_RESTART`, `BGP_GRACEFUL_RESTART_TIME`, `BGP_ADDPATH_RX`, `BGP_SOFTWARE_VERSION` | none: `frr.conf` graceful restart and capability statements are rejected | `network.bgp.gracefulRestart`, `gracefulRestartTime`, `addPathReceive`, `softwareVersion` | native only; unset is the FRR default (for `BGP_SOFTWARE_VERSION`, of the `frr defaults` profile) |
| `BGP_HOSTNAME` | none: FRR ignores the `frr.conf` `hostname` line and sends the system host name | `network.bgp.hostname` | native only; unset keeps the system host name |
| `LLDP_RECEIVE` | none: the legacy ramdisk does not receive LLDP | `network.lldpReceive` | native only; unset is on |
| `IP_FORWARDING`, `IPV6_FORWARDING` | `frr.conf` `[no] ip[v6] forwarding` | `network.ipForwarding`, `network.ipv6Forwarding` | native wins |

### Strict input and the accepted grammar

The behaviour reference is the current FRR (the legacy ramdisk runs the
latest FRR release; the differential lab uses FRR 10.3.1). BOOTy accepts
the configuration syntax of the templates, which is still FRR 8.x style
(`frr version 8.2.2`, `frr defaults traditional`). Where FRR 8.2.2 and the
current FRR behave differently, the current FRR wins.

BOOTy reads `frr.conf` with a block parser that follows FRR's reading rules
(`lib/command.c`, `command_config_read_one_line`): indentation does not
matter, lines that start with `!` or `#` are comments, and a statement that
does not fit the current node is retried in the parent nodes. The parser
stays in the node where the statement matched.

By default both inputs are strict. A netplan key or an `frr.conf` statement
that BOOTy does not implement fails the network setup. The error names the
line, the statement and the node the parser was in, for example:

```text
parse FRR config /deploy/file-system/etc/frr/frr.conf: line 31
"maximum-paths 8": statement not supported by BOOTy
(in router bgp 4210000017 vrf Vrf_underlay > address-family ipv4 unicast)
```

`FRR_CONFIG_STRICT=false` and `NETPLAN_STRICT=false` (YAML
`network.frrStrict`, `network.netplanStrict`) log a warning with the line for
each such statement or key and go on without it. The FRR warning names the
file and the line.

Lenient mode skips only what BOOTy does not implement. An unsupported
statement inside a route-map, such as `set metric` or `match ip address
prefix-list`, is then skipped, so the route-map matches or sets less than in
FRR and the policy changes. These stay errors in both modes:

- an invalid value, such as `remote-as 65oo1`, a router-id that is not an
  IPv4 address or a network with prefix length 33;
- an unsupported statement that opens a block: skipping it would apply its
  body to the enclosing node. The FRR node keywords are `router`,
  `vrf`, `interface`, `address-family`, `vni`, `route-map`, `bfd`, `profile`
  and `peer` (BFD), `line`, `key`, `segment-routing`, `srv6`, `locators`,
  `locator`, `mpls`, `l2vpn`, `pseudowire`, `pbr-map`, `nexthop-group`,
  `rpki`, `bmp`, `vnc`, `vrf-policy`, `link-params`, `affinity-map` and
  `evpn`, also after `no`. A known
  opener with unknown arguments, such as `router bgp 65000 view foo`, is
  rejected too;
- `neighbor IFNAME interface` with an undefined peer-group;
- in netplan, a value of the wrong type, and an unknown key under `match`
  (it would select other NICs) or in a route (`on-link`, `type` and similar
  change the route).

A netplan route with `table`, `metric`, `scope` or `from` fails in strict
mode. BOOTy parses these keys but does not apply them. In lenient mode the
route is dropped with a warning, so a default route with `table: 100` does
not become the main-table gateway.

FRR applies a statement in the deepest node of the stack that has it and
stays in that node. For a skipped statement, BOOTy looks the first word up
in a table of the FRR commands per node and moves to the same node
as FRR, so the next statements land where FRR puts them. Example: `neighbor
leaf capability extended-nexthop` inside `address-family l2vpn evpn` is a
router-node command, so FRR leaves the address-family; a following
`neighbor leaf activate` is then also skipped instead of being applied to
l2vpn evpn. If no node of the stack has the statement, FRR stays where it
is, and so does BOOTy. Each later statement of the same block is applied
with a warning ("FRR may apply it in another node"), until a statement of a
parent node ends the block. The table (`pkg/network/frrconf/known.go`) has
the first words of the current FRR commands (checked against FRR 10.3.1) of the nodes that BOOTy parses:
top level, `router bgp`, the unicast address-families, `address-family
l2vpn evpn` (its own, shorter list, with its own neighbor attributes), the
L2VNI block, `route-map`, `bfd` and its profile, `vrf` and `line vty`. It
decides by the first word (and the neighbor attribute), not by the full
command syntax, and it does not cover the commands of other daemons. For a
statement outside the table the node can differ from FRR's; the warnings
show where.

A skipped statement does not change the model: for example `neighbor swp1
interface v6only` does not create the neighbor. Lenient mode is still a
risk: a skipped statement that changes routing in FRR (for example
`maximum-paths`) has no effect in BOOTy, and only the warning shows it.

Accepted `frr.conf` statements, per node. "Accepted" means parsed and
validated. GoBGP mode applies all routing statements among them, also
`frr defaults` (the session timer profile), `[no] bgp default
ipv4-unicast` and `[no] bgp ebgp-requires-policy` (see
[BGP sessions](#bgp-sessions)), except these, which are parsed and
validated only: `redistribute connected`,
`neighbor X soft-reconfiguration inbound`, `advertise-default-gw`,
`[no] bgp suppress-duplicates`, `rd` and `route-target` of an L2VNI block
and `hostname`. FRR mode passes the file to FRR.

| Node | Statements |
|---|---|
| top level | `frr version V`, `frr defaults traditional\|datacenter`, `hostname H`, `log stdout\|syslog [LEVEL]`, `log file PATH [LEVEL]`, `service integrated-vtysh-config`, `[no] ip forwarding`, `[no] ipv6 forwarding`, `vni N [prefix-routes-only]`, `ip route P blackhole`, `ipv6 route P blackhole`, `router bgp ASN [vrf NAME]`, `bfd`, `route-map NAME permit\|deny SEQ`, `bgp community-list standard NAME [seq N] permit\|deny COMMUNITY...`, `vrf NAME`, `line vty`, `exit`, `end` |
| `vrf NAME` | `vni N [prefix-routes-only]`, `exit-vrf` or `exit` |
| `router bgp` | `bgp router-id A.B.C.D`, `[no] bgp ebgp-requires-policy`, `[no] bgp suppress-duplicates`, `[no] bgp default ipv4-unicast`, `bgp bestpath as-path multipath-relax`, `[no] bgp fast-external-failover`, `neighbor NAME peer-group`, `neighbor IFNAME interface [peer-group G \| remote-as R]`, `neighbor ADDR peer-group G`, `neighbor X remote-as ASN\|internal\|external`, `neighbor X local-as N [no-prepend [replace-as]]`, `neighbor X bfd [profile P]`, `neighbor X timers K H`, `neighbor X timers connect N`, `address-family ipv4 [unicast]\|ipv6 [unicast]\|l2vpn evpn`, `exit` (also `end` and the top-level statements, which leave the node) |
| `address-family ipv4\|ipv6` | `network P`, `redistribute connected`, `neighbor X activate`, `neighbor X soft-reconfiguration inbound`, `neighbor X allowas-in [N]`, `neighbor X route-map NAME in\|out`, `exit-address-family` or `exit` |
| `address-family l2vpn evpn` | the neighbor statements above, `advertise-all-vni`, `advertise-default-gw`, `advertise ipv4\|ipv6 unicast`, `route-target import\|export\|both RT`, `vni N` (opens an L2VNI block with `rd RD`, `route-target ... RT`, `exit-vni` or `exit`), `exit-address-family` or `exit` |
| `route-map` | `match community LIST`, `set local-preference N`, `set community AA:NN... [additive]`, `exit` |
| `bfd` | `profile NAME` (opens a profile with `transmit-interval`, `receive-interval` 10-60000, `detect-multiplier` 2-255), `exit` |
| `line vty` | `exit` |

These statements have no effect on routing in FRR either, so BOOTy accepts
and ignores them also in strict mode: `neighbor X description TEXT` (the
neighbor must exist), `[no] bgp log-neighbor-changes`, `debug ...`,
`log timestamp precision N`. In netplan, `optional` on ethernets, bonds,
tunnels, bridges and VLANs is accepted and ignored: it only changes whether
`systemd-networkd-wait-online` waits for the device.

Everything else is rejected, for example the statements that the templates
do not use today but that change routing: `maximum-paths`, `redistribute
connected route-map`, `ip prefix-list`, `neighbor X capability
extended-nexthop`, `neighbor X ebgp-multihop`, `neighbor X password`,
`bgp graceful-restart`, `set metric`, BFD `echo-mode`, and in netplan
`gateway4`, `routes` with `on-link` or `type`, bond `dhcp4`, bond `routes`
and bond parameters such as `up-delay`. These constructs are rejected
on purpose although they are not in the templates:

- a UTF-8 byte order mark at the start of `frr.conf` (the first statement
  does not match),
- `neighbor IFNAME interface v6only`,
- well-known communities such as `no-export` in `set community` or in a
  community-list (only `AA:NN` is accepted),
- expanded community-lists,
- static routes with a next hop, a distance or `Null0` (only `blackhole`
  without a distance),
- `ip route` and `ipv6 route` inside a `vrf` block (BOOTy installs static
  routes only in the main table).

## FRR and GoBGP compared

| Feature | FRR | GoBGP |
|---|---|---|
| Implementation | FRR daemons (`zebra`, `bgpd`, `bfdd`), config rendered to `/etc/frr/frr.conf` | In-process GoBGP library, no daemons, no gRPC API |
| Image flavors | Default and `iso` flavors only (`make iso`, `make docker`, `make dockerx86`). `slim`, `micro`, `gobgp` and `gobgp-iso` ship no FRR binaries | All flavors. The BOOTy binary is the same in every flavor |
| Peer modes | Unnumbered only, on all detected physical NICs. `BGP_PEER_MODE`, `BGP_INTERFACES` and `BGP_NEIGHBORS` are ignored | With `frr.conf` neighbors or `BGP_SESSIONS`: one session per neighbor (see [BGP sessions](#bgp-sessions)). Otherwise `unnumbered`, `dual`, `numbered` (see [GoBGP peer modes](#gobgp-peer-modes)) |
| Remote AS | Peer group with `remote-as external` (eBGP only) | Per session: number, `internal` or `external`. Without sessions: unnumbered learns it from the peer OPEN, numbered uses `BGP_REMOTE_ASN` (`0` means iBGP) |
| BFD | Yes. `BFD_TRANSMIT_MS` and `BFD_RECEIVE_MS` together create the `datacenter` BFD profile on the fabric peers and the DCGWs | Yes, BOOTy's own BFD (`pkg/network/bfd`): asynchronous mode, RFC 5881 single-hop (IPv4 and IPv6 link-local) and RFC 5883 multihop, Poll/Final, no echo, demand or authentication. `neighbor X bfd` in `frr.conf` with the profile timers (FRR defaults 300/300/3), or `BFD_TRANSMIT_MS` and `BFD_RECEIVE_MS` for the native peers. BFD down resets the BGP session, which connects again after 1 s like FRR 10.3 bgpd (Cease subcode 4 instead of 10) |
| Authentication | `BGP_AUTH_PASSWORD` as TCP-MD5 on the fabric peer group and the DCGWs. Whitespace and control characters are rejected | `BGP_AUTH_PASSWORD` as TCP-MD5 on all peers |
| Timers | `BGP_KEEPALIVE` and `BGP_HOLD` apply only when both are set. Otherwise FRR uses its `datacenter` defaults | Per session from `frr.conf` or `BGP_SESSIONS`, else the `frr defaults` profile. Without sessions: keepalive 3 s, hold 9 s, connect retry 5 s; hold must be 0 or at least 3 and keepalive must not exceed hold / 3 |
| Peer readiness | Not checked. The connectivity check only polls the target URL | `BGP_MIN_PEERS` established peers (default 1) before the target poll |
| Underlay AF | IPv4. Adds IPv6 unicast when the overlay IP is IPv6 | IPv4 only (`BGP_UNDERLAY_AF=ipv4`) |
| Overlay type | EVPN/VXLAN only. `BGP_OVERLAY_TYPE` is ignored | `evpn-vxlan` (default) or `none`. `l3vpn` passes config validation but the GoBGP setup rejects it |
| EVPN routes | FRR rules: `advertise-all-vni`. With `dcgw_ips`, also `advertise ipv4 unicast` | Selected by BOOTy (see [EVPN route modes](#evpn-route-modes-gobgp)) |
| `EVPN_L2_ENABLED`, `EVPN_TYPE5_ONLY`, `provision_gateway` | Ignored | Used |
| DCGW (OneFabric) | `dcgw_ips` adds iBGP DCGW neighbors. `overlay_aggregate` and `vpn_rt` apply only with `dcgw_ips` | `dcgw_ips` and `overlay_aggregate` are ignored. `vpn_rt` sets the EVPN route target |
| `local_asn` | `neighbor fabric local-as N no-prepend replace-as` | Ignored. Each session has its own `local-as` |
| Kernel routes | Zebra installs the learned routes | With BGP sessions, BOOTy installs the learned unicast routes like zebra (see [BGP sessions](#bgp-sessions)). Without sessions, only the routes in [Kernel state per mode](#kernel-state-per-mode) |
| Wait behaviour | Restarts FRR every 120 s while the target is not reachable. Dumps the FRR state on timeout | Waits for peers, then polls the target |

`leaf_asn` is parsed but no stack uses it.

Both stacks:

- Use the provision bridge `br.provision` and the VXLAN device `vx<VNI>`
  (GoBGP: the netplan bridge and tunnel names when set) with UDP port 4789
  and MAC learning off. The VXLAN MTU is the NIC MTU minus 50. The NIC MTU
  is 9000, or the largest netplan `mtu`.
- Put the underlay IP on the `dummy.underlay` device (FRR falls back to
  `lo`) and the overlay IP on `lo`.
- Disable reverse-path filtering. The FRR stack enables IPv4 and IPv6
  forwarding; GoBGP mode keeps IPv6 forwarding off by default like the
  legacy ramdisk (see [Netplan and FRR config input](#netplan-and-frr-config-input)).
- Use `VRF_NAME` as the underlay VRF when set. Without it, they use the
  default routing table. `VRF_TABLE_ID` defaults to 1000. GoBGP replaces
  table 1 with 1000 and logs a warning.

### BGP capabilities and LLDP

The OPEN message of GoBGP has the capabilities that FRR 10 sends with the
bm4x template (`TestProdTplOpenCapabilities` compares the leaf and route
reflector views of BOOTy and the shadow FRR node):

- route refresh, 4-octet AS, extended next hop;
- the hostname (FQDN capability, no domain): the system host name, as
  GoBGP takes it from `os.Hostname()`. It is `HOSTNAME` for provisioning
  with `RAMDISK_HOSTNAME=true`, else the host name of the ramdisk.
  `BGP_HOSTNAME` (YAML `network.bgp.hostname`) wins: BOOTy sets it as the
  ramdisk host name before BGP starts. FRR 10.3.1 sends the system host
  name too: a `hostname` line in an integrated `frr.conf` does not reach
  the daemons (measured: with `hostname frr-from-conf`, bgpd, zebra and
  vtysh all show the uname, and the leaf receives it), so BOOTy does not
  take the `frr.conf` `hostname` either;
- the software version (`BGP_SOFTWARE_VERSION`): like FRR 10.3
  (`BGP_SOFT_VERSION_CAPABILITY`), to every peer with `frr defaults
  datacenter` (the CAPRF configs and the default without the line), to
  iBGP peers only with `traditional` (the bm4x templates). A set value
  wins;
- graceful restart as helper with restart time 120 s
  (`BGP_GRACEFUL_RESTART`, `BGP_GRACEFUL_RESTART_TIME`);
- long-lived graceful restart (without family tuples in GoBGP helper mode,
  so there is no stale time to set);
- add-path receive in every address family (`BGP_ADDPATH_RX`).

The minimum route advertisement interval is 0 in both: FRR shows
`minBtwnAdvertisementRunsTimerMsecs` 0 for these peers, and GoBGP sends
updates without a timer.

GoBGP 3.37 cannot send the following, so they are known deviations:

| FRR 10 sends | GoBGP | Fabric impact |
|---|---|---|
| Enhanced route refresh (RFC 7313) | Not sent | A peer that refreshes its routes from BOOTy uses plain route refresh, without the begin and end markers. Nothing is lost; stale routes of BOOTy are not swept at the end of a refresh. |
| Extended message (RFC 8654) | Not sent | Messages stay at 4096 bytes. The few routes of a server fit. |
| Paths-limit (with add-path) | Not sent | FRR sends limit 0 (no limit), so the peer behaves the same. |
| Dynamic capability: FRR sends it with `frr defaults datacenter` (`BGP_DYNAMIC_CAPABILITY`), so the CAPRF configs send it and the bm4x templates (`traditional`) do not | Not sent | A capability change needs a session reset. The fabric does not change capabilities at run time. |
| Graceful restart Restart State (R) bit | Off | The R bit tells the peer that the speaker restarted. BOOTy is only a helper and keeps no forwarding state, so the peer's behaviour does not change. |
| Long-lived graceful restart family tuples (stale time 0) | Without tuples in helper mode | With stale time 0 neither side keeps routes long-lived. |
| Software version `FRRouting/10.x` | `GoBGP/3.37.0` | Information only. |
| No extended next hop for l2vpn evpn | GoBGP also offers it for l2vpn evpn | The route reflectors do not offer it, so it is not negotiated. FRR on the route reflectors logs one capability warning per OPEN for the unexpected l2vpn evpn tuple. |

Defaults of `frr defaults`: FRR 10.3.1 has these profile-dependent
defaults (every `FRR_CFG_DEFAULT_*` with a `match_profile` in bgpd, zebra,
bfdd and lib). BOOTy uses `BGP_DEFAULTS`, else the profile of `frr.conf`,
else `datacenter` (the legacy daemons file):

| Default | traditional | datacenter | BOOTy |
|---|---|---|---|
| BGP keepalive, hold, connect retry | 60, 180, 30 s | 3, 9, 10 s | same per profile (sessions; `BGP_KEEPALIVE`/`BGP_HOLD` for the peer modes) |
| `bgp ebgp-requires-policy` | on | off | same per profile |
| software version capability | iBGP only | every peer | same per profile |
| dynamic capability | off | on | not sent (GoBGP has none) |
| `bgp deterministic-med` | off | on | GoBGP has no switch; it compares MED only between paths from the same neighbor AS. The fabric sets no MED, so the best path is the same |
| `bgp log-neighbor-changes` | off | on | GoBGP logs every peer state change in both |
| `bgp default show-hostname`, `show-nexthop-hostname` | off | on | display only, nothing to apply |
| zebra `ip nht resolve-via-default` | on | off | same per profile: with datacenter the FIB writer does not resolve a next hop that only a default route covers |

`bgp network import-check` is on in both profiles of FRR 10.3 (off only for
traditional before 7.4); BOOTy originates the `network` prefixes without a
RIB check, and with the templates they always exist locally (the underlay
/32 on its dummy, the lo /128 and the blackhole /64). The other
`FRR_CFG_DEFAULT_*` (suppress-duplicates, graceful-restart notification,
hard-admin-reset, enforce-first-as) depend on the FRR version, not on the
profile.

Graceful restart and switch failures: as helper, BOOTy keeps the routes
of a leaf or route reflector that restarts its BGP process with graceful
restart (forwarding bit set) for the peer's restart time, like FRR, so a
control plane restart of a switch does not take the server down. A switch
that fails ends the session through BFD or the carrier with a NOTIFICATION
and no N bit, so graceful restart does not apply and the routes go at
once, as in FRR. When BOOTy restarts BGP (BFD down, the end of the run),
its capability has no address family tuples, so the leaf removes BOOTy's
routes at once, the same as for the legacy FRR.

LLDP: like systemd-networkd with netplan `emit-lldp: true` (systemd 249,
`TestProdTplLLDPLikeNetworkd` compares the frames on the same leaf),
BOOTy sends on the `LLDP_EMIT` NICs: chassis ID the machine ID (locally
assigned; like systemd on the legacy ramdisk, BOOTy creates a random one
per boot, kept in `/run/booty/machine-id`; it never uses or writes
`/etc/machine-id`, so no ID is shared by hosts or written into the
provisioned OS), port ID the interface name, TTL 120 s, system name the
hostname, and the capabilities station, bridge and router (station
enabled). With `LLDP_RECEIVE` (on by default) BOOTy also listens on every
physical NIC, logs each new or changed neighbor and adds the current
neighbors to the hardware inventory report; the legacy ramdisk only
sends. The receive joins the LLDP multicast groups (01:80:c2:00:00:0e,
:03 and :00) like networkd and lldpd, which a NIC that is not
promiscuous needs; the lab veths pass LLDP without them, so the lab does
not show it (a root test checks the membership). It drops BOOTy's own
frames that come back (our source MAC or chassis ID). It keeps one
neighbor per NIC: a second chassis on the same link replaces the first.
The inventory runs seconds after the receive starts, so while no
neighbor is known it waits until the receive has run for 30 s, one
transmit interval of lldpd and networkd.

Intel ice and i40e NICs run an LLDP agent in firmware that takes the
LLDP frames before the host sees them, and on ice it also blocks the
frames the host sends. The legacy ramdisk does not switch the agent off
(networkd `emit-lldp` only, no `ethtool --set-priv-flags`), so BOOTy does
not either: on those NICs LLDP receive and send may see nothing. Turning
the agent off (`disable-fw-lldp`) also turns off firmware DCBX, which
RoCE and PFC in the installed OS may need, and some NVM versions keep
the flag across reboots. An opt-in that turns it off for BOOTy's run and
back on before the handoff comes with the cable check (one
implementation in `pkg/firmware/nic`).

## GoBGP peer modes

| Mode | Sessions | Address families | Requirements |
|---|---|---|---|
| `unnumbered` (default) | One session per fabric NIC to the IPv6 link-local peer that NDP finds | IPv4 unicast, L2VPN EVPN | Fabric NICs |
| `dual` | Unnumbered sessions per fabric NIC, plus numbered sessions to `BGP_NEIGHBORS` | Unnumbered: IPv4 unicast. Numbered: L2VPN EVPN only, like the bm4x template (`no bgp default ipv4-unicast`, `neighbor dcgw activate` only in `l2vpn evpn`) | `BGP_NEIGHBORS` |
| `numbered` | Numbered sessions to `BGP_NEIGHBORS` | IPv4 unicast, L2VPN EVPN | `BGP_NEIGHBORS` (IPv4). The host must already reach the neighbors |

Fabric NICs are the NICs in `BGP_INTERFACES`, or all detected physical NICs
when it is empty. BOOTy waits up to 10 s for the listed NICs. In unnumbered
and dual mode, BOOTy sends an IPv6 Router Advertisement on each fabric NIC
every 10 s, so that FRR peers learn the BOOTy link-local address. Teardown
stops these RAs.

With `VRF_NAME`, BOOTy puts the fabric NICs into the VRF. It skips a NIC
that carries an IPv4 default route, because that NIC is a management NIC.

In numbered mode, BOOTy does not configure the NICs. It only sets their
MTU.

GoBGP advertises the underlay IP (router ID) as an IPv4 unicast `/32` in all
peer modes.

## EVPN route modes (GoBGP)

This section applies to GoBGP. FRR ignores the EVPN mode settings.

### Mode selection

BOOTy decides the mode in this order:

1. The provision VNI is an FRR L3VNI: Type-5-only, like FRR, which
   originates only Type-5 routes for an L3VNI. An L3VNI is a `vni N`
   (optionally `prefix-routes-only`) at the top level of `frr.conf` or
   inside a `vrf` block. BOOTy checks the provision VNI after it merges the
   vars, so a VNI from `/deploy/vars` also counts. An explicit
   `EVPN_TYPE5_ONLY`, `EVPN_L2_ENABLED=true` or `provision_gateway` wins.
2. `EVPN_TYPE5_ONLY=true` selects Type-5-only.
3. `EVPN_L2_ENABLED=true` selects L2.
4. Auto-detection: the provisioner `frr.conf` contains `advertise-all-vni`.
   This selects L2.
5. Otherwise: Type-5 with gateway.

`EVPN_TYPE5_ONLY=true` wins over auto-detected L2. `EVPN_TYPE5_ONLY=true`
together with `EVPN_L2_ENABLED=true` is an error: the GoBGP setup fails
with `type5Only and EnableL2 cannot both be enabled`, and the network setup
is fatal.

| Mode | `provision_gateway` | Routes BOOTy originates | Routes BOOTy processes |
|---|---|---|---|
| L2 | Required | Type-5, Type-3, Type-2 | Type-5, Type-2, Type-3 |
| Type-5 with gateway (default) | Required | Type-5 | Type-5 |
| Type-5-only | Optional, IPv4 | Type-5 | Type-5 |

Without `provision_gateway`, the L2 and default modes fail with
`provision gateway is required for GoBGP VXLAN data plane`. With
`BGP_OVERLAY_TYPE=none`, BOOTy creates no overlay and does not need the
gateway. GoBGP still requires `provision_vni` in every overlay type.

### Routes BOOTy originates

All routes use the route distinguisher `asn_server:provision_vni` and the
router ID as next hop. The route target follows the precedence in the
`EVPN_RT_IMPORT` row of [Configuration](configuration.md): the native
`EVPN_RT_IMPORT`/`EVPN_RT_EXPORT` lists, then the `frr.conf` route targets
or FRR's auto route target of an `frr.conf` L3VNI, then `vpn_rt`, then
`asn_server:provision_vni`.

| Route | Content | Mode |
|---|---|---|
| Type-5 (IP prefix) | `/32` of the `provision_ip` host address, gateway `0.0.0.0`, label = VNI, extended communities: route target, VXLAN encapsulation, router MAC | All. Skipped when `provision_ip` is empty |
| Type-5 (IP prefix) | Each `network` of the L3VNI VRF in `frr.conf` (see [Netplan and FRR config input](#netplan-and-frr-config-input)), gateway `0.0.0.0` or `::`, same label and extended communities. Like the FRR import check, only a prefix that is a `lo` address, the `provision_ip` prefix or a blackhole route is advertised. BOOTy logs the others. | Type-5-only from an L3VNI |
| Type-3 (IMET) | Router ID, PMSI tunnel type ingress replication | L2 |
| Type-2 (MAC/IP) | Bridge MAC and `provision_ip` host address | L2 |

With `asn_server` > 65535, `provision_vni` must be 65535 or lower, because a
4-octet ASN route target carries only a 16-bit value.

### Routes BOOTy processes

BOOTy accepts a route update only when its route target matches the local
route target. A withdraw without a route target is accepted. BOOTy ignores
routes whose next hop is its own router ID.

| Route | Action |
|---|---|
| Type-5 | Installs an on-link kernel route for the prefix through the route gateway on the provision bridge, in the overlay VRF table when one is set. A gateway of `0.0.0.0` or `::` means the VTEP (next hop). With a router MAC, BOOTy also installs a neighbor entry for the gateway and an FDB entry `router MAC -> VTEP`. A withdraw removes them. An IPv6 prefix over an IPv4 VTEP is installed like zebra: gateway `::ffff:<VTEP>`, protocol `bgp`, metric 20, and the neighbor entry `extern_learn NOARP`. BOOTy does not install a route for the prefix of a blackhole route; the static route wins, as in zebra. |
| Type-2 (L2 only) | Installs the FDB entry `MAC -> VTEP`. A withdraw removes it. |
| Type-3 (L2 only) | Installs the BUM FDB entry `00:00:00:00:00:00 -> VTEP`. A withdraw removes it. |

### Kernel state per mode

| Item | L2 and default | Type-5-only without gateway | Type-5-only with gateway |
|---|---|---|---|
| BUM FDB entry `00:00:00:00:00:00 -> provision_gateway` on `vx<VNI>` | Yes | No | No |
| `/32` route to `provision_gateway` | Yes | No | Yes |
| `/32` route to each remote VTEP from a Type-5 next hop | No | Yes, except with `frr.conf` sessions: then the routes learned from the fabric reach the VTEP | No |

The underlay `/32` routes use a fabric NIC with link scope. With
`VRF_NAME`, BOOTy uses the first NIC in the VRF and the VRF table. Without
it, BOOTy uses the first fabric NIC without an IPv4 address. In numbered
mode, the routes go through the egress of the first usable numbered
neighbor. In numbered mode, a failed gateway route fails the connectivity
attempt. In the other modes, BOOTy logs a warning.

## Router MAC

The provision bridge MAC is also the EVPN router MAC. BOOTy selects it in
this order (`deriveBridgeMAC` in `pkg/network/frr/config.go`):

1. The netplan `macaddress` of the bridge that BOOTy creates. It must be a
   48-bit MAC. BOOTy uses it unchanged.
2. `02:54` followed by the last 4 bytes of the IPMI NIC MAC.
3. `02:54` followed by the 4 bytes of the underlay IPv4 address, like the
   legacy `configure-frr.py` script.

The derived MACs are locally administered unicast addresses and stable per
host. The underlay IP is the VTEP address, so rule 3 gives a unique MAC per
host. Rule 2 keeps only 4 bytes of the IPMI MAC, so a collision is
unlikely but possible. Set the netplan `macaddress` if you need a fixed
value. The underlay IP must be IPv4 in all cases.

## Underlay IP and IPMI detection

The underlay IP is the BGP router ID and the VTEP source address. BOOTy
selects it in this order:

1. `underlay_ip` (or the netplan value).
2. The IPMI IP offset applied to `underlay_subnet`. This needs the IPMI IP
   and `ipmi_subnet`.

Without one of these, FRR setup fails (DHCP fallback) and GoBGP setup fails
(fatal). The overlay IP is the IPMI offset applied to `overlay_subnet`. It is
the underlay IP when the IPMI data or `overlay_subnet` is missing.

In FRR and GoBGP mode, BOOTy detects the IPMI NIC on the host. It considers
only non-loopback NICs with a MAC and a usable IPv4 address, in this order:

1. A name that starts with `ipmi`, `bmc`, `idrac`, `ilo` or `imm`.
2. A name that starts with `mgmt` or `management`.
3. A NIC with an IPv4 address in `ipmi_subnet`, when `ipmi_subnet` is set.

Any other NIC is not an IPMI NIC. A data NIC with an address is never used.
When no NIC matches, BOOTy logs it and uses the underlay IP rules.

## Physical NIC detection

FRR, GoBGP (without `BGP_INTERFACES`), DHCP and static mode (without
`STATIC_IFACE`) use the physical NIC list (`DetectPhysicalNICs` in
`pkg/network/nic_linux.go`). A physical NIC:

- has a MAC address;
- is not a VRF device;
- does not have a name that starts with `lo`, `docker`, `veth`, `vx`, `br`,
  `dummy`, `virbr`, `bond`, `tun` or `tap`.

BOOTy retries the scan for up to 10 s while names start with `clab`
(containerlab renames) or while only one NIC is visible.

## DHCP

DHCP mode probes all physical NICs in parallel. Each probe has a 15 s
timeout. The first lease wins and BOOTy stops the other probes.

The client requests the default parameter list: subnet mask, router, time
server, DNS server, domain name, interface MTU and NTP servers. It sends the
ramdisk hostname (option 12) when one is set.

On the first lease, BOOTy:

- adds the leased address with the leased mask to the NIC;
- adds a default route through the first router from the lease.

BOOTy does not apply DNS servers or the MTU from the lease. Set
`dns_resolver` for DNS.

Because `bond*` names are not physical NICs, DHCP does not run on `bond0`.

Planned (#608): handle DHCP leases without a subnet mask and leases on
retry.

## Static mode

Static mode adds `STATIC_IP` (CIDR) to `STATIC_IFACE`, or to the first
physical NIC. With `STATIC_GATEWAY`, it adds a default route. Teardown
removes the route and the address.

## Connectivity check

BOOTy selects the target URL in this order:

1. `INIT_URL`
2. `SUCCESS_URL`
3. With `CRASH_ARTIFACTS_ENABLED=true`: `CRASH_ARTIFACTS_PREPARE_URL`, then
   `CRASH_ARTIFACTS_UPLOAD_URL`

Without a target, BOOTy skips the check.

The check sends HTTP `HEAD` requests to the target, one per second, with a
10 s request timeout. Any HTTP response, also a non-2xx status, counts as
success. One attempt lasts up to 5 minutes:

- GoBGP first waits up to 5 minutes for `BGP_MIN_PEERS` established peers.
  It then installs the gateway route and polls the target for up to 5
  more minutes.
- FRR polls the target and restarts FRR every 120 s. On timeout it logs
  `show bgp summary`, the IPv4, IPv6 and EVPN tables, the BFD peers and the
  interfaces. Each `vtysh` call has a 10 s limit.
- Static and DHCP mode poll the target.

After a failed attempt, BOOTy tears down the stack and the link layers and
runs the full network setup again. After 3 failed attempts, the network
setup fails (see [Stack selection](#stack-selection) for the failure
behaviour).

With `NETWORK_WAIT_MODE=rounds` (YAML `network.waitMode`, preset by
`CAPRF_LEGACY_COMPAT=true`) the wait follows the legacy `wait_for_network`
instead: one network setup and `NETWORK_WAIT_MAX_ATTEMPTS`
rounds of 20 seconds (unset: 15 rounds, about 5 minutes). The limit covers
the whole wait, also the GoBGP peer wait. In FRR mode FRR restarts after each
failed round unless a BGP session is established. `0` means no round: the
run fails at once, without the FRR state dump. Then BOOTy reports the error
and waits.

Logs show the target without credentials, query string and fragment.

## DNS

BOOTy writes `nameserver` lines for `dns_resolver` (comma-separated IPv4 or
IPv6 addresses) to `/etc/resolv.conf` after the stack setup. An invalid
address fails the network setup. With `CAPRF_LEGACY_COMPAT=true` and a
provisioner `/etc/resolv.conf` in the ramdisk, BOOTy keeps that file.

## Variables used

See [configuration.md](configuration.md) for defaults and accepted values.

- Selection: `NETWORK_MODE`
- BGP: `asn_server`, `BGP_PEER_MODE`, `BGP_INTERFACES`, `BGP_NEIGHBORS`,
  `BGP_REMOTE_ASN`, `BGP_AUTH_PASSWORD`, `BGP_KEEPALIVE`, `BGP_HOLD`,
  `BGP_MIN_PEERS`, `BGP_UNDERLAY_AF`, `BGP_OVERLAY_TYPE`, `BFD_TRANSMIT_MS`,
  `BFD_RECEIVE_MS`
- EVPN: `underlay_ip`, `underlay_subnet`, `overlay_subnet`, `ipmi_subnet`,
  `provision_vni`, `provision_ip`, `provision_gateway`, `EVPN_L2_ENABLED`,
  `EVPN_TYPE5_ONLY`, `vpn_rt`, `dcgw_ips`, `overlay_aggregate`,
  `local_asn`, `leaf_asn`
- VRF: `VRF_NAME`, `VRF_TABLE_ID`
- Static, bond, VLAN: `STATIC_IP`, `STATIC_GATEWAY`, `STATIC_IFACE`,
  `BOND_INTERFACES`, `BOND_MODE`, `VLANS`
- DNS: `dns_resolver`
- Connectivity target: `INIT_URL`, `SUCCESS_URL`, `CRASH_ARTIFACTS_ENABLED`,
  `CRASH_ARTIFACTS_PREPARE_URL`, `CRASH_ARTIFACTS_UPLOAD_URL`
- Connectivity wait: `NETWORK_WAIT_MODE`, `NETWORK_WAIT_MAX_ATTEMPTS`

## Planned

- Planned (#602): keep a GoBGP Type-5 kernel route while another path still
  carries the prefix.
- Planned (#504): skip local EVPN Type-5 provision routes.
- Planned (#608): DHCP leases without a mask and on retry.
