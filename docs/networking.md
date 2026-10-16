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
  IP (`local`) and the VXLAN underlay link (`link`).
- A dummy device address with `/32` gives the underlay IP and the underlay
  dummy name.
- The bridge that contains a tunnel and has an address gives the bridge name
  and the provision IP. Its `macaddress` gives the router MAC.
- The first ethernet address gives the static IP; a `default` route gives
  the static gateway. `nameservers` give the DNS resolvers.
- VLANs, the first bond, and VRFs with a table.

BOOTy takes these values from `frr.conf`:

- `router bgp N` gives the ASN. `bgp router-id` gives the underlay IP when
  netplan does not.
- Every neighbor of every `router bgp` block becomes one GoBGP session (see
  [BGP sessions](#bgp-sessions)), also numbered peers in `169.254.0.0/16`
  and `127.0.0.0/8`, like FRR.
- `advertise-all-vni` and the L3VNIs select the EVPN route mode (see
  [EVPN route modes](#evpn-route-modes-gobgp)).

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
  link-local address on the interface.

With these sessions, the vars `BGP_PEER_MODE`, `BGP_INTERFACES`,
`BGP_NEIGHBORS`, `BGP_REMOTE_ASN`, `BGP_KEEPALIVE` and `BGP_HOLD` no longer
apply. BOOTy logs a warning for each of them that is set. `local_asn` does
not apply either, because each session has its own `local-as`.

These errors stop the setup in both inputs and are not relaxed by
`FRR_CONFIG_STRICT=false`:

- a neighbor without remote-as;
- a local-as mode other than `no-prepend replace-as`;
- local-as equal to the router AS, or a local-as with `remote-as internal`
  (use the number);
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
warning per statement: `allowas-in`, `route-map ... in|out`,
`soft-reconfiguration inbound`, `[no] bgp ebgp-requires-policy`,
`[no] bgp suppress-duplicates` and `bgp bestpath as-path multipath-relax`.
`neighbor X bfd` is logged as not implemented yet.

Native config and parsed files merge like this: a native list
(`network.bgp.sessions`) replaces the parsed list as a whole, with one info
log line; there is no element-wise merge.

A value from netplan overrides the same value from the vars. An empty
netplan value keeps the vars value. `VLANS` is the exception: BOOTy parses it
after the netplan merge, so `VLANS` from the vars replaces the netplan VLANs. When netplan and the FRR config give an
ASN and a VNI, BOOTy sets the stack to GoBGP. This overrides
`NETWORK_MODE`.

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
line, the statement and the node the parser was in, for example
`parse FRR config /deploy/file-system/etc/frr/frr.conf: line 31 "maximum-paths 8": statement not supported by BOOTy (in router bgp 4210000017 vrf Vrf_underlay > address-family ipv4 unicast)`.
`FRR_CONFIG_STRICT=false` and `NETPLAN_STRICT=false` (YAML
`network.frrStrict`, `network.netplanStrict`) log a warning with the line for
each such statement or key and go on without it. The FRR warning names the
file and the line.

Lenient mode skips only what BOOTy does not implement. These stay errors in
both modes:

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
validated. Of these, this release applies: `router bgp` (ASN and VRF),
`bgp router-id`, the neighbors with peer-groups, `remote-as`, `local-as`,
`timers`, `timers connect` and the activated address families (see
[BGP sessions](#bgp-sessions)), `[no] bgp default ipv4-unicast`, `frr
defaults`, `address-family l2vpn evpn` with `advertise-all-vni`, and the
L3VNIs from `vni N`. BOOTy reads the other statements into its model;
later releases apply them, and this list grows with them.

| Node | Statements |
|---|---|
| top level | `frr version V`, `frr defaults traditional\|datacenter`, `hostname H`, `log stdout\|syslog [LEVEL]`, `log file PATH [LEVEL]`, `service integrated-vtysh-config`, `[no] ip forwarding`, `[no] ipv6 forwarding`, `vni N [prefix-routes-only]`, `ip route P blackhole`, `ipv6 route P blackhole`, `router bgp ASN [vrf NAME]`, `bfd`, `route-map NAME permit\|deny SEQ`, `bgp community-list standard NAME [seq N] permit\|deny COMMUNITY...`, `vrf NAME`, `line vty`, `exit`, `end` |
| `vrf NAME` | `vni N [prefix-routes-only]`, `exit-vrf` or `exit` |
| `router bgp` | `bgp router-id A.B.C.D`, `[no] bgp ebgp-requires-policy`, `[no] bgp suppress-duplicates`, `[no] bgp default ipv4-unicast`, `bgp bestpath as-path multipath-relax`, `neighbor NAME peer-group`, `neighbor IFNAME interface [peer-group G \| remote-as R]`, `neighbor ADDR peer-group G`, `neighbor X remote-as ASN\|internal\|external`, `neighbor X local-as N [no-prepend [replace-as]]`, `neighbor X bfd [profile P]`, `neighbor X timers K H`, `neighbor X timers connect N`, `address-family ipv4 [unicast]\|ipv6 [unicast]\|l2vpn evpn`, `exit` (also `end` and the top-level statements, which leave the node) |
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
| BFD | Yes. `BFD_TRANSMIT_MS` and `BFD_RECEIVE_MS` together create the `datacenter` BFD profile on the fabric peers and the DCGWs | Not yet. `BFD_TRANSMIT_MS`/`BFD_RECEIVE_MS` are rejected; `neighbor X bfd` in `frr.conf` is logged as not implemented |
| Authentication | `BGP_AUTH_PASSWORD` as TCP-MD5 on the fabric peer group and the DCGWs. Whitespace and control characters are rejected | `BGP_AUTH_PASSWORD` as TCP-MD5 on all peers |
| Timers | `BGP_KEEPALIVE` and `BGP_HOLD` apply only when both are set. Otherwise FRR uses its `datacenter` defaults | Per session from `frr.conf` or `BGP_SESSIONS`, else the `frr defaults` profile. Without sessions: keepalive 3 s, hold 9 s, connect retry 5 s; hold must be 0 or at least 3 and keepalive must not exceed hold / 3 |
| Peer readiness | Not checked. The connectivity check only polls the target URL | `BGP_MIN_PEERS` established peers (default 1) before the target poll |
| Underlay AF | IPv4. Adds IPv6 unicast when the overlay IP is IPv6 | IPv4 only (`BGP_UNDERLAY_AF=ipv4`) |
| Overlay type | EVPN/VXLAN only. `BGP_OVERLAY_TYPE` is ignored | `evpn-vxlan` (default) or `none`. `l3vpn` passes config validation but the GoBGP setup rejects it |
| EVPN routes | FRR rules: `advertise-all-vni`. With `dcgw_ips`, also `advertise ipv4 unicast` | Selected by BOOTy (see [EVPN route modes](#evpn-route-modes-gobgp)) |
| `EVPN_L2_ENABLED`, `EVPN_TYPE5_ONLY`, `provision_gateway` | Ignored | Used |
| DCGW (OneFabric) | `dcgw_ips` adds iBGP DCGW neighbors. `overlay_aggregate` and `vpn_rt` apply only with `dcgw_ips` | `dcgw_ips` and `overlay_aggregate` are ignored. `vpn_rt` sets the EVPN route target |
| `local_asn` | `neighbor fabric local-as N no-prepend replace-as` | Ignored. Each session has its own `local-as` |
| Kernel routes | Zebra installs the learned routes | BOOTy installs only the routes in [Kernel state per mode](#kernel-state-per-mode). GoBGP does not install learned routes |
| Wait behaviour | Restarts FRR every 120 s while the target is not reachable. Dumps the FRR state on timeout | Waits for peers, then polls the target |

`leaf_asn` is parsed but no stack uses it.

Both stacks:

- Use the provision bridge `br.provision` and the VXLAN device `vx<VNI>`
  with UDP port 4789 and MAC learning off. The VXLAN MTU is the NIC MTU
  minus 50. The NIC MTU is 9000, or the largest netplan `mtu`.
- Put the underlay IP on the `dummy.underlay` device (FRR falls back to
  `lo`) and the overlay IP on `lo`.
- Enable IPv4 and IPv6 forwarding and disable reverse-path filtering.
- Use `VRF_NAME` as the underlay VRF when set. Without it, they use the
  default routing table. `VRF_TABLE_ID` defaults to 1000. GoBGP replaces
  table 1 with 1000 and logs a warning.

## GoBGP peer modes

| Mode | Sessions | Address families | Requirements |
|---|---|---|---|
| `unnumbered` (default) | One session per fabric NIC to the IPv6 link-local peer that NDP finds | IPv4 unicast, L2VPN EVPN | Fabric NICs |
| `dual` | Unnumbered sessions per fabric NIC, plus numbered sessions to `BGP_NEIGHBORS` | Unnumbered: IPv4 unicast. Numbered: IPv4 unicast, L2VPN EVPN | `BGP_NEIGHBORS` |
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

1. `EVPN_TYPE5_ONLY=true` selects Type-5-only.
2. `EVPN_L2_ENABLED=true` selects L2.
3. Auto-detection: the provisioner `frr.conf` contains `advertise-all-vni`,
   and the provision VNI is not an FRR L3VNI. This selects L2. An L3VNI is a
   `vni N` (optionally `prefix-routes-only`) at the top level of `frr.conf`
   or inside a `vrf` block. BOOTy checks the provision VNI after it merges
   the vars, so a VNI from `/deploy/vars` also counts.
4. Otherwise: Type-5 with gateway.

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
router ID as next hop. The route target is `vpn_rt`, or
`asn_server:provision_vni` when `vpn_rt` is empty.

| Route | Content | Mode |
|---|---|---|
| Type-5 (IP prefix) | `/32` of the `provision_ip` host address, gateway `0.0.0.0`, label = VNI, extended communities: route target, VXLAN encapsulation, router MAC | All. Skipped when `provision_ip` is empty |
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
| Type-5 | Installs an on-link kernel route for the prefix through the route gateway on the provision bridge, in the overlay VRF table when one is set. A gateway of `0.0.0.0` means the VTEP (next hop). With a router MAC, BOOTy also installs a neighbor entry for the gateway and an FDB entry `router MAC -> VTEP`. A withdraw removes them. |
| Type-2 (L2 only) | Installs the FDB entry `MAC -> VTEP`. A withdraw removes it. |
| Type-3 (L2 only) | Installs the BUM FDB entry `00:00:00:00:00:00 -> VTEP`. A withdraw removes it. |

### Kernel state per mode

| Item | L2 and default | Type-5-only without gateway | Type-5-only with gateway |
|---|---|---|---|
| BUM FDB entry `00:00:00:00:00:00 -> provision_gateway` on `vx<VNI>` | Yes | No | No |
| `/32` route to `provision_gateway` | Yes | No | Yes |
| `/32` route to each remote VTEP from a Type-5 next hop | No | Yes | No |

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
