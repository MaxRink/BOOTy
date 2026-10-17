# Proposal: Full FRR and Netplan Parity in GoBGP Mode for CAPRF

## Status: Accepted design, implementation in progress

Decision: BOOTy extends its own parser and its GoBGP stack. The parser and
the stack must cover the full networking feature set that the legacy CAPRF
ramdisk gets from FRR and netplan. An earlier draft proposed to run FRR with
the provisioner `frr.conf` unchanged (FRR passthrough). That option is
rejected.

Tie-break rule: when GoBGP, das-schiff-network-operator (nwop) or BOOTy
semantics differ from FRR, BOOTy does what FRR does with the same `frr.conf`.
FRR's own tests (`tests/topotests/*`, `tests/bgpd/*`) are the reference.
Each decision below cites the FRR docs, source or test.

## 1. Inputs

### 1.1 Producers

| Producer | Files | Notes |
|---|---|---|
| bm4x-operator `internal/controller/infra/redfish/provcfg/templates` (cit/t-caas/bm4x-operator origin/main b90274c) | `etc/frr/frr.conf.tpl`, `etc/netplan/netplan.yaml.tpl`, `etc/resolv.conf.tpl` | Production. Rendered by `template.go` into the CAPRF provisioner config Secret `prov-<host>`. The inputs are listed in 1.3. |
| CAPRF `test/e2e/lib.sh` `generate_evpn_provisioner_config`, `deploy_type` `old` | same paths | e2e copy of the production files. Differences: `frr defaults datacenter`, `frr version 9.1`, numbered `remote-as external` peer, netplan `match: {name: enp*s4}` with `set-name: hbn`. |
| CAPRF `test/e2e/lib.sh`, `deploy_type` `booty` | same paths | BOOTy-only variant. It is removed once BOOTy handles the `old` files (section 5, slice g). |
| CAPRF `test/e2e/lib.sh` `generate_dhcp_provisioner_config` (`old`) | `/etc/netplan/10-provision.yaml` | `match: {name: "en*"}` and `{name: "eth*"}` with `dhcp4: true`. |
| CAPRF `cmd/genimg` | `/deploy/netplan/config.yaml` | Dev tool only, DHCP. |
| kindmetal | none | Its FRR templates are fabric-side only. In the CAPRF-on-kindmetal flow, the bm4x-operator renders the files against the kindmetal BM4X mock API. |

No producer emits netplan `vlans`, `bonds` or `routes`. BOOTy keeps its
existing VLAN and bond support from the vars and netplan files, and the
tests cover it (section 4). It is not new work in this stream.

### 1.2 FRR version

- The production template starts with `frr version 8.2.2` and
  `frr defaults traditional`. FRR selects version-dependent defaults by the
  `frr version` line, so 8.2.2 traditional defaults apply even on a newer
  FRR (FRR `doc/user/basic.rst`, "Configuration versioning, profiles and
  upgrade behavior").
- The legacy ramdisk installs FRR from the unpinned `frr-stable` channel
  (`image-building/deployer-base-image` tag 0.3.0, `static/img/init:91-93`).
  CAPRF pins base image 0.3.0 (`.gitlab-ci.yml:15`). The daemons file sets
  `frr_profile="datacenter"`, `bgpd=yes`, `bfdd=yes`
  (`static/img/pre-init/etc/frr/daemons:64`). The template's
  `frr defaults traditional` line overrides that profile.
- Reference version for BOOTy: FRR 8.2.2 semantics, as the template states.
  The labs run the FRR image that the BOOTy labs already use
  (`quay.io/frrouting/frr:10.3.1`) with the same `frr version 8.2.2` line.
  Where the 8.2.2 topotests assert nothing (see 4.2), the 10.x topotests are
  used.
- Open item: read `dpkg -l frr` from the deployer base image tarball and
  record the exact legacy FRR version here.

### 1.3 Template inputs (bm4x-operator origin/main)

| Input | Source |
|---|---|
| `.Interfaces`, `.IsLayer2` | `template.go:79-91`. Ports of logical interfaces with role != inactive. `IsLayer2` is the role `sriov` (`VTEP_LEAF`). |
| `.VNI`, `.ProvisioningNetwork`, `.ProvisioningAddress`, `.UnderlayAddress`, `.UnderlayMac` | `internal/controller/infra/common/networking.go:47-85` |
| `.ASNumber` | `provision.ResolveASNumber` |
| `.RouteTarget` | `netInstProv.BGPRouteTarget` |
| `.RouteReflectors` | `ResolveRouteReflectorIPs`, must not be empty |

## 2. Construct inventory

Legend for "today": the result of `netplan.ToNetworkConfig` on the rendered
production templates (two uplinks, ASN 4210000017, VNI 3000), before this
stream.

| # | Construct (bm4x template line) | FRR semantics (reference) | BOOTy today | Target | Slice |
|---|---|---|---|---|---|
| C1 | Block lines without indentation (`neighbor` at column 0) | Leading blanks are ignored. A line that does not fit the current node is retried in the parent nodes (`lib/command.c:1227-1276`, `command_config_read_one_line`). | Regexes need leading blanks. Only lines after a blank line match. Second uplink, `local-as` and the RR peers are lost. | Block parser with FRR node semantics. | a (regex fix in #634 first) |
| C2 | Unknown statements | FRR logs an error and goes on. | Silently ignored. | Hard error with file and line number. Deliberate difference from FRR: a silent drop would hide a parity gap. | a |
| C3 | `router bgp N vrf Vrf_underlay`, `router bgp N vrf default` | One BGP instance per VRF. | One global instance. VRF from netplan only. | Model: one instance per VRF. GoBGP: underlay instance sessions bound to `Vrf_underlay`. | a, b |
| C4 | `neighbor dcgw peer-group`, `remote-as internal`, `neighbor <RR> peer-group dcgw` | Members inherit the group's `remote-as`, timers, BFD, AFs and policies. A member override wins (`tests/bgpd/test_peer_attr.c`). `internal` means peer AS = local AS. | RR peers lost. `BGPPeerMode=unnumbered`. | Numbered iBGP to every RR. Source and listener in `Vrf_underlay` (`Transport.BindInterface`, plus `net.ipv4.tcp_l3mdev_accept=1` or a VRF-bound listener). Only l2vpn evpn is active. | b |
| C5 | `neighbor leaf remote-as 65500`, `neighbor <nic> interface peer-group leaf` (every non-SRIOV port) | Unnumbered eBGP over IPv6 link-local with extended next hop (RFC 5549). | First uplink only. Peers are auto-detected NICs. | All listed uplinks. Validate the NIC list against netplan ethernets. | b |
| C6 | `neighbor leaf local-as 65501 no-prepend replace-as` | `no-prepend`: L is not added to received paths. `replace-as`: only L is sent outbound. `replace-as` requires `no-prepend`. eBGP only (`doc/user/bgp.rst:1568-1584`). | Parsed by the old regex only when indented. GoBGP ignores `LocalASN`. | GoBGP v3 local-as is no-prepend replace-as by design (`fsm.go:829`, `table/path.go:256`, `oc/util.go:133`): map it to `PeerConf.LocalAsn`. Plain `local-as X` or only one keyword is a hard error. Gap to close: GoBGP's inbound AS-loop check uses only the router AS (`pkg/server/peer.go:607-610`, `hasOwnASLoop` with `peerInfo.LocalAS`, which `internal/pkg/table/destination.go:127` sets to the global AS; found in the slice c lab), while FRR also rejects paths that contain the local-as (`bgpd/bgp_route.c`, `aspath_loop_check` on `change_local_as`). BOOTy adds an import check for the local-as (subject to `allowas-in`, C11) and a lab test for it. | b (check: e) |
| C7 | `timers 30 90`, `timers connect 5` | Per-neighbor values override the profile. Negotiated hold = min of both sides; keepalive follows (`bgp.rst:1731-1739`). Profile defaults in current FRR: traditional 60/180/30, datacenter 3/9/10 (`bgpd/bgp_vty.c`, `bgpd/bgpd.h`; 8.2.2 had 120 for traditional). | Not parsed. GoBGP defaults. | `Timers.Config` keepalive, hold, connect-retry. Template defaults follow the `frr defaults` line. | b |
| C8 | `no bgp default ipv4-unicast`, `address-family ... activate` | Without the default, only activated AFs are negotiated (`bgp_default_afi_safi` topotest). | One AF set per mode. | Per-peer AF list from the model. | b |
| C9 | `no bgp ebgp-requires-policy` | Traditional profile: eBGP without policy sends and accepts nothing unless disabled (`bgp.rst:442-452`). | Not modelled. | Model the flag. Without it, under `traditional`, an eBGP peer without policy gets no routes, like FRR. | b |
| C10 | `bgp bestpath as-path multipath-relax` | ECMP across eBGP peers with different AS (`bgp_ecmp_topo1`). | Not modelled. | GoBGP multipath on. Install ECMP in the FIB (C17). | b, c |
| C11 | `allowas-in` | Default occurrence count 3 (`bgp_vty.c:8013`). | Not parsed. | GoBGP `PeerConf.AllowOwnAsn`, the largest count of the activated AFs. The bm4x RR loopbacks reach the server with the real ASN in the path, so the FIB writer needs it. | c |
| C12 | `soft-reconfiguration inbound` | Keeps pre-policy Adj-RIB-In. | Not parsed. | Accept. GoBGP keeps Adj-RIB-In anyway. No behavior change. | a |
| C13 | `route-map TAG-FABRIC-IN permit 10`: `set local-preference 90`, `set community 65169:200 additive`; applied `in` on leaf and dcgw | A matching permit entry applies its sets and stops. No match means implicit deny (`doc/user/routemap.rst:28-86`). `additive` appends (`bgp.rst:2263`). | Not parsed. | GoBGP import policy with the same statement order and an explicit final reject. | e |
| C14 | `bgp community-list standard cm-received-fabric permit 65169:200`, `route-map DENY-TAG-FABRIC-OUT deny 10 match community`, `permit 20`; applied `out` | A matching deny entry denies. A standard community list without a match returns deny (`bgp.rst:2127-2135`). | Not parsed. | GoBGP community `DefinedSet` and export policy. Any other route-map verb is a hard error. | e |
| C15 | `route-target import/export <RT>` in `router bgp N vrf default` l2vpn evpn | Explicit RTs replace auto RTs. Auto RT is `(AS & 0xFFFF):VNI` (`bgpd/bgp_evpn.c:509-518`). Export RTs apply only to locally originated routes. | Not parsed. BOOTy derives `ASN:VNI`. | Use the explicit RTs. When none are set, derive them exactly like `form_auto_rt` (also for 4-byte ASNs). | e |
| C16 | IPv6 `/128` provisioning address on `lo`; `network <addr>/128`, `network <prefix>`; `advertise ipv6 unicast`; `ipv6 route <prefix> blackhole` | `network` is advertised only when the prefix is in the RIB (import-check, `bgp.rst:1057-1065`); the blackhole route puts the prefix there. `advertise ipv6 unicast` creates Type-5 only (`bgp.rst:2885-2895`). | `lo` becomes the static interface (`StaticIP=<addr>/128`, `StaticIface=lo`). No Type-5 IPv6. | Address on `lo` (not a static interface). Blackhole route in the kernel. Type-5 IPv6 originated for both `network` lines, with the import check. | d |
| C17 | Routes learned from the fabric (RR reachability over the unnumbered sessions, default route) | zebra installs the best paths into the VRF table, with IPv6 link-local next hops on the interface and ECMP. | GoBGP installs nothing into the kernel (`pkg/network/gobgp/underlay.go` has no RouteReplace). | FIB writer ("mini zebra"): IPv4 and IPv6 unicast RIB to kernel routes in the VRF table, link-local next hops with device, multipath, withdraw. | c |
| C18 | IPv6 Type-5 install over the IPv4 VTEP | zebra installs next hop `::ffff:<VTEP>` on the L3VNI bridge (protocol `bgp`, metric 20), with a neighbor entry for that address with the RMAC (`extern_learn NOARP`), and an FDB entry RMAC to VTEP (`bgp_evpn_rt5` topotest, 10.x `test_router_check_ip`). | `overlay.go:846-870` falls back to the IPv4 VTEP as gateway. `buildType5KernelRoute` (1125-1141) puts an IPv4 gateway on an IPv6 destination. `buildType5GatewayNeighbor` (1143-1158) returns nil for IPv6. | Install like zebra. | d |
| C19 | BFD: `neighbor X bfd`, `bfd profile fabric`, `transmit-interval 150`, `receive-interval 150` | Defaults: detect multiplier 3, 300/300 ms. An unknown profile falls back to the defaults (`doc/user/bfd.rst:115-120` for the fallback, `147-171` for the defaults, `bfd_profiles_topo1`). BFD down tears the session down (`bfd_topo1`). | Not parsed. GoBGP has no BFD. | Own minimal BFD (section 3.6). | BFD (after b) |
| C20 | `vni N` at top level, `advertise-all-vni` | `advertise-all-vni` creates Type-2/3 for L2VNIs only. A top-level `vni` maps the default VRF to an L3VNI. | Done (#582 and existing README table). | Keep. Covered by the Type-5-only and Type-2/3+Type-5 labs. | f |
| C21 | `no bgp suppress-duplicates`, `no ip forwarding`, `service integrated-vtysh-config`, `hostname`, `log ...`, `frr version`, `frr defaults` | Daemon and logging options. | Ignored. | Accept and record in the model. `frr defaults` selects the timer defaults (C7). `no ip forwarding` is a no-op in the ramdisk. | a |
| N1 | netplan ethernets keyed by port name: `mtu`, `link-local: [ipv6]`, `accept-ra`, `emit-lldp`, `ipv6-address-generation: eui64` | netplan semantics | `mtu` sets the MTU. `link-local: [ipv6]` adds an auto-detected interface (`parser.go:207-212`). Other keys are ignored. | Apply all keys. `emit-lldp` is accepted; BOOTy has an LLDP package but does not need to send. | a, c |
| N2 | `match` with `name` glob or `macaddress`, `set-name` | netplan renames the single match before use. | Partial: a literal `match.name` becomes the static interface (`parser.go:309-321`); `macaddress` is parsed but unused; `set-name` is not in the types. | Its own PR before slice a (`network.ApplyLinkRenames`, `network.ResolveLinkMatch`). | before a |
| N3 | `vrfs` (`table`, `interfaces`), `dummy-devices`, `tunnels` vxlan (`id`, `local`, `link`, `port`, `macaddress`, `hairpin`, `mac-learning`, `arp-proxy`, `port-mac-learning`, `mtu`), `bridges` (`macaddress`, `parameters.stp`, `interfaces`, `mtu`) | netplan semantics | First-match heuristics (`extractBonds`, `extractVRFs`). `BridgeName` lost. VTEP not bound to `link`. | Model every device. Apply with netlink. The netplan bridge MAC is the RMAC; apply the same MAC to `vx.<VNI>` as the template does. | a, c |
| N4 | Unknown netplan keys | netplan rejects them. | Ignored. | Hard error. | a |

## 3. Design

### 3.1 Parser and model (slice a)

- One block parser for `frr.conf`. It tracks the FRR node stack:
  - top level
  - `router bgp [vrf]`
  - `address-family`
  - `route-map`
  - `bfd` and its `profile`
  - `vrf`
  - `exit`, `exit-address-family` and `!`
- A line that is not valid in the current node is tried in the parent
  nodes, like `command_config_read_one_line`.
- A whitelist grammar covers every statement in section 2. Any other
  statement fails with `file:line`.
- The netplan parser keeps every device instead of first-match heuristics.
  Unknown keys fail.
- The model follows nwop names where the concepts match:
  - `Neighbor{IP | Interface, RemoteASN, LocalASN, KeepaliveTime, HoldTime, BFDProfile, IPv4, IPv6, EVPN}` (nwop `pkg/config/baseconfig.go`)
  - `Filter{Items, DefaultAction}`, `Matcher{BGPCommunity}`, `Action{Type, ModifyRoute{AdditiveCommunities}}` (nwop `api/v1alpha1/nodenetworkconfig_types.go:185-255`)
  - `FabricVRF{VNI, EVPNImportRouteTargets, EVPNExportRouteTargets}` (same file, l.107)
- BOOTy adds what nwop does not model:
  - peer-groups
  - `remote-as internal`
  - `SetLocalPreference`
  - `ConnectRetry`
  - `AllowASIn`
  - `SoftReconfigInbound`
  - `EBGPRequiresPolicy`
  - the `frr defaults` profile
- The existing `network.Config` is filled from the model with the same
  derivation rules as today, so sessions and routes do not change in this
  slice. One behavior changes on purpose: an unknown FRR statement or
  netplan key now fails the run (C2, N4). Configs that parse today with
  ignored statements fail after this slice.

### 3.2 Sessions (slice b)

- Every member of every peer-group gets a session.
- RR members are numbered iBGP. Their source and listener are bound to
  `Vrf_underlay`, and both directions are tested.
- Leaf members are unnumbered eBGP on every listed uplink. Local-as is
  mapped as in C6.
- Timers come from the model with FRR profile defaults.
- AFs are activated per peer.

### 3.3 FIB writer (slice c)

- A GoBGP watch (`WatchBestPath`) over IPv4 and IPv6 unicast in the
  underlay VRF.
- Best paths become kernel routes in the VRF table. Link-local next hops
  carry the device. ECMP uses `Multipath`. Replace and delete are
  idempotent.
- This is what makes the RRs reachable in bm4x. They are only reachable
  over routes learned from the leaf.

### 3.4 EVPN Type-5 IPv6 (slice d)

- Install like zebra, as in C18. A received route for the prefix of a
  local blackhole route is not installed: zebra prefers the static route.
- Originate Type-5 for both `network` lines, with the import check. Add
  the blackhole route and the `lo` address.
- A provision VNI that is an L3VNI selects Type-5 only (C20). The VXLAN
  device takes its netplan name (`vx.3000`).
- With FRR config sessions, the FIB writer reaches the remote VTEPs. The
  scope-link VTEP routes of the Type-5-only mode would replace its routes,
  so BOOTy does not install them then.

### 3.5 Policy, RT (slice e)

- C13, C14, C15 as described in section 2, and the local-as loop check of C6.
- Route-map verbs are limited to the ones the template uses:
  - `permit` and `deny` with sequence numbers
  - `match community <list>`
  - `set local-preference`
  - `set community ... additive`
  - implicit deny

  Anything else fails.

### 3.6 BFD (decided: own minimal implementation)

Go has no usable BFD library (GoBGP has no BFD; nwop never renders peer
BFD). BOOTy implements a minimal BFD in its own package
(`pkg/network/bfd`):

- RFC 5880 and RFC 5881: single-hop, asynchronous mode, UDP 3784, IPv4 and
  IPv6 link-local.
- RFC 5883 multihop (UDP 4784) for numbered peers outside the connected
  prefixes, like FRR bgpd does for the iBGP route reflector peers of the
  template; sent from the router ID in the session VRF, received with
  FRR's default minimum TTL 254.
- Desired min TX, required min RX and detect multiplier come from
  `bfd profile` and `neighbor ... bfd profile`. When unset, the FRR 8.2.2
  defaults apply: multiplier 3, 300 ms RX and TX (`doc/user/bfd.rst:147-171`).
  An unknown profile falls back to the defaults, like FRR
  (`bfd_profiles_topo1`).
- Poll/Final sequence for parameter changes (RFC 5880 6.5).
- Demand mode and echo mode stay off. Authentication is not supported. A
  config that asks for authentication or echo fails the run.
- BFD Up to Down stops the BGP session, like FRR 10.3 (`bgpd/bgp_bfd.c`
  `bfd_session_status_update`: Cease/BFD Down and `BGP_Stop`). FRR then
  waits `BGP_INIT_START_TIMER` (1 s) in Idle and connects again
  (`bgp_stop_with_notify`). BOOTy uses GoBGP `ResetPeer` with an idle hold
  time of 1 s after a reset (GoBGP default 30 s); GoBGP sends Cease
  subcode 4, not 10. FRR also sends `BGP_Start` on BFD Up for a peer that
  is not established; GoBGP has no API for it and connects 1 to 2 s after
  Idle anyway. Every state change is logged.
- Unit tests:
  - the state machine (RFC 5880 6.8.6 transitions)
  - detection time math (6.8.4)
  - Poll/Final handling
- ContainerLab e2e against the FRR leaf:
  - session Up
  - negotiated intervals in `show bfd peers json`
  - failover within detect multiplier x interval when an uplink is cut
  - the differential check against the shadow FRR node

BFD lands after slice b.

### 3.7 nwop: followed and different

| Topic | nwop | BOOTy |
|---|---|---|
| Model names | `pkg/config/baseconfig.go` `Neighbor`, `api/v1alpha1` `Filter`/`Matcher`/`Action` | Same names (3.1). |
| Implicit deny | Rendered explicitly as the last route-map entry (`config/agent-cra-frr/frr.conf.tpl:23-70`) | Same: explicit final reject in the GoBGP policy. |
| L2VNI RT | Left empty so FRR auto-derives (`pkg/reconciler/intent/builder/l2a.go:773-780`) | Same rule: auto-derive like `form_auto_rt` when none is configured. |
| Netlink apply | `pkg/nl/create.go` `createVRF`, `createBridge`, `createVXLAN` (learning off, port 4789, VtepDevIndex) | Same pattern. BOOTy also binds the VTEP to the netplan `link` device. |
| set-name | `ensureInterfaceName` via `LinkSetName` (`pkg/reconciler/agent-hbn-l2/nodenetplanconfig_reconciler.go:373`) | Same, in `network.ApplyLinkRenames` from the N2 PR. |
| BFD | Not rendered | Section 3.6. |
| e2e | ContainerLab with kind nodes; no route-type, BFD or failover assertions (`e2etests/framework/frr.go`) | Uses the vtysh JSON helper idea and adds route-type, failover, BFD and differential assertions. |

nwop and BOOTy are both Apache-2.0 (nwop also has a `NOTICE` file). BOOTy
imports nothing from nwop. Small copied pieces, if any, keep the
attribution.

## 4. Tests

### 4.1 Golden and mutation tests (slice a)

- testdata contains the bm4x origin/main templates, rendered with
  documentation addresses (RFC 5737, RFC 3849) for:
  - 1 uplink
  - 2 uplinks
  - 2 uplinks plus one `IsLayer2` port
  - 1 and 3 route reflectors
- A golden test checks the full model and the resulting `network.Config`.
- A mutation corpus checks that each removed or changed statement changes
  the result, and that each unknown statement fails with its line number.
- The CAPRF `old` files from `test/e2e/lib.sh` (both refs) are also golden
  inputs.

### 4.2 FRR test scenarios ported

| BOOTy test (planned) | Mirrors FRR test |
|---|---|
| `TestParseUnindentedBlocks` | `lib/command.c` parent-node retry; `all_protocol_startup/test_all_protocol_startup.py::test_error_messages_daemons` |
| `TestPeerGroupInheritance` | `tests/bgpd/test_peer_attr.c` (peer vs group attribute table) |
| lab `peer_group_members_established` | `bgp_peer_group/test_bgp_peer-group.py::test_bgp_peer_group` |
| lab `local_as_replace_as_path` | 10.x `bgp_local_asn/test_bgp_local_asn_topo1.py::test_verify_bgp_local_as_in_EBGP_p0` (path strings with no-prepend replace-as) |
| lab `unnumbered_rfc5549_routes` | `bgp_ipv4_over_ipv6/test_rfc5549_ebgp_unnumbered_nbr.py::test_unnumbered_loopback_ebgp_nbr_p0` |
| lab `default_afi_only_evpn_to_rr` | `bgp_default_afi_safi/test_bgp-default-afi-safi.py::test_bgp_default_ipv4_ipv6_unicast` |
| lab `ecmp_two_uplinks` | `bgp_ecmp_topo1/test_bgp_ecmp_topo1.py::test_bgp_ecmp` |
| lab `ebgp_requires_policy` | `bgp_ebgp_requires_policy/test_bgp_ebgp_requires_policy.py::test_ebgp_requires_policy` |
| lab `timers_negotiated` | `bgp_basic_functionality_topo1/test_bgp_basic_functionality.py::test_bgp_timers_functionality` |
| lab `route_map_tag_in` / `route_map_deny_tag_out` | `bgp_route_map/test_route_map_topo2.py::test_add_and_remove_community_list_referenced_by_rmap_p0`, `::test_set_localpref_weight_to_ebgp_and_med_to_ibgp_peers_p0`; `bgp_multiview_topo1` (`additive`) |
| lab `allowas_in` | `bgp_as_allow_in/test_bgp_as_allow_in.py::test_bgp_allowas_in_p0` |
| lab `own_asn_loop_rejected` | `bgp_as_allow_in/test_bgp_as_allow_in.py::test_bgp_allowas_in_p0` (the path without allowas-in is rejected) |
| lab `rr_session_vrf_outbound`, `rr_session_vrf_inbound` | No FRR equivalent. BOOTy specific (GoBGP sockets bound to the VRF). |
| lab `explicit_rt_import_export` | `evpn_type5_test_topo1/test_evpn_type5_topo1.py::test_RT_verification_manual_p0` |
| lab `type5_ipv6_install` | 10.x `bgp_evpn_rt5/test_bgp_evpn.py::test_router_check_ip`, `::test_evpn_remove_ipv6` |
| lab `network_blackhole_import_check` | `bgp_basic_functionality_topo1/...::test_advertise_network_using_network_command`; `all_protocol_startup` (`ipv6 route ... blackhole`) |
| lab `bfd_profile_values` / `bfd_failover` | `bfd_profiles_topo1/test_bfd_profiles_topo1.py::test_bfd_profile_values`; `bfd_topo1/test_bfd_topo1.py::test_bfd_fast_convergence` |

### 4.3 Differential ContainerLab (slice f)

- Topology: one leaf (FRR, AS 65500), two DCGW route reflectors (FRR,
  iBGP), the BOOTy node, and a shadow FRR node. The shadow node gets the
  identical rendered bm4x `frr.conf` and netplan (applied with netplan) and
  peers with the same leaf and DCGWs.
- Assertions run on the leaf and DCGW views of both nodes, normalized (the
  node-specific addresses are replaced):
  - `show bgp l2vpn evpn json`: route types, prefixes, RT, RMAC
  - `show bgp ... advertised-routes json`: AS_PATH, communities, local-pref
  - `show bgp summary json`: sessions, AFs, negotiated timers
- Host kernel state is compared too: `ip -j route show table all`,
  `ip -j neigh`, `bridge -j fdb`. Any difference fails the job.
- Behavior: BOOTy reaches the CAPRF mock over the IPv6 Type-5 default route.
- Fault injection:
  - drop one uplink (ECMP failover)
  - restart dcgw01 (RR redundancy)
- Matrix: {Type-5 only, Type-2/3 + Type-5} x {1 uplink, 2 uplinks}. Timers,
  route-maps, RT, local-as, `lo` /128 and blackhole are asserted in each
  run.
- One KVM run boots the real BOOTy ISO with the rendered files.
- Every lab test is shown to fail without its feature: the PR runs it once
  against main (or with the feature switched off in the test image) and
  links the failed run.

### 4.4 e2e coverage matrix

Kept current as slices land.

| Construct | e2e test | CI job | Fails without feature (evidence) |
|---|---|---|---|
| N2 set-name, MAC match | `TestLegacyCompatNetplanSetNameQEMU` | KVM `caprf-legacy-compat` | planned |
| C2, N4 unknown statements and keys | unit and mutation tests only (a failing parse is fully observable without a lab) | `Test` | planned |
| C1, C4, C5, C6, C7, C8 | `TestProdTplLeafSessions`, `TestProdTplLocalASReplaceASPath`, `TestProdTplRRSessions` (peer-group members, local-as, timers, per-peer AFs; each also against the shadow FRR node) | E2E Production Templates | all fail on slice a (sessions from first-match heuristics), see the slice b PR |
| C6 local-as loop check | lab `own_asn_loop_rejected` | same | planned (slice e) |
| C3 VRF-bound RR sessions, both directions | `TestProdTplRRSessionVRFOutbound`, `TestProdTplRRSessionVRFInbound` | same | both fail on slice a, see the slice b PR |
| C5 RFC 5549 routes | lab `unnumbered_rfc5549_routes` | same | planned |
| C9 | lab `ebgp_requires_policy` | same | planned |
| N1, N3 devices | differential lab kernel-state comparison (`ip -j link`, `ip -j addr`, `bridge -j link`) | same | planned |
| C10, C17, C11 | `TestProdTplFIBRoutes` (ECMP over both uplinks, also against the shadow node), `TestProdTplFIBWithdraw` (uplink session down and up), `TestProdTplRRSessions` (RRs reachable only over FIB routes with the router AS in the path); fault `drop_uplink` (link down) in slice f | same | fail on slice b, see the slice c PR |
| C17 carrier loss (`bgp fast-external-failover`, on by default) | `TestProdTplUplinkCarrierLoss` (the next hop leaves the VRF table within 2 s, also on the shadow node; ECMP comes back with the carrier) | same | fails without the carrier watch (fd177071): BOOTy still had both next hops after 2.1 s, see the slice c PR |
| C13, C14 | lab `route_map_tag_in`, `route_map_deny_tag_out` | same | planned |
| C15 | lab `explicit_rt_import_export` | same | planned |
| C16, C18, C20 L3VNI Type-5 only | `TestProdTplType5IPv6Install` (IPv6 Type-5 routes over `::ffff:<VTEP>`, neighbor and FDB entries, `lo` /128, blackhole /64; equal to the shadow node), `TestProdTplType5Origination` (dcgw01 installs the `network` routes of both servers alike), `TestProdTplType5CAPRFOverIPv6` (HTTP to the IPv6 CAPRF mock over the Type-5 default, from the `lo` /128) | same | on slice c the lab does not start (no Type-5-only inference: `provision gateway is required`); with `EVPN_TYPE5_ONLY` forced, all three fail and `TestProdTplFIBRoutes` fails too (the scope-link VTEP routes replace the FIB writer routes), see the slice d PR |
| C19 | `TestProdTplBFDSessions` (leaf: single-hop sessions Up with the "fabric" profile 150/150/3 of every server; RRs: multihop sessions Up), `TestProdTplBFDFailover` (the leaf stops sending on one uplink with the carrier up; the next hop leaves the VRF table within 2.5 s instead of the 90 s hold time), each for BOOTy, the vars-only server and the shadow node; state machine, detection time and Poll/Final in `pkg/network/bfd` unit tests | same | without BOOTy BFD (slice n image) both tests fail for BOOTy and the vars-only server, see the BFD PR |
| C20 | existing `E2E Pure Type-5 Fabric`, `E2E EVPN Boot (vrnetlab)` plus the new matrix | existing and new | existing |
| VLAN, bond | existing `E2E ContainerLab` (LACP topology), VLAN unit and lab tests | existing | existing |
| Full template set | differential lab + KVM ISO run | same + KVM | planned |

## 5. Slices (one PR each, at most four not ready at a time)

1. #634: regex fix for unindented lines (C1, interim).
2. N2: set-name and match, its own PR.
3. a: block parser, model, golden and mutation tests. Sessions and routes
   do not change; unknown statements and keys fail (3.1).
4. b: peer-groups, `remote-as internal`, local-as, timers, all uplinks, AFs,
   VRF-bound sessions.
5. c: FIB writer, `allowas-in`.
6. d: IPv6 Type-5 install, `network`, blackhole, `lo` address, Type-5 only for an L3VNI.
7. e: route-map subset, RT, local-as loop check.
8. f: production-template ContainerLab with the shadow FRR node.
9. BFD (section 3.6), after slice b.
10. g: CAPRF switch. The e2e `booty` fixtures go away; BOOTy gets the `old`
   files. Needs a BOOTy release.

## 6. Open questions

1. Exact FRR version in the legacy base image (1.2).
2. Should BOOTy also accept the CAPRF `old` e2e variants (`frr defaults
   datacenter`, `frr version 9.1`)? Proposed: yes, and the defaults follow
   the `frr defaults` line.
