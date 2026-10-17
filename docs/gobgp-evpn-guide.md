# GoBGP EVPN Guide

This guide shows how to run BOOTy with the in-process GoBGP stack. For the
behaviour (stack selection, peer modes, EVPN route modes, router MAC,
connectivity check), see [networking.md](networking.md). For the variable
reference, see [configuration.md](configuration.md).

## Overview

GoBGP mode runs BGP inside the BOOTy process. It needs no FRR daemons, so it
works in every image flavor. The stack has two tiers (`pkg/network/gobgp`):

| Tier | Function |
|---|---|
| Underlay | BGP sessions to the fabric. Advertises the router ID `/32` in IPv4 unicast. Unnumbered peers are found through IPv6 NDP. |
| Overlay | VXLAN device and provision bridge. Advertises and processes EVPN routes. Installs the gateway or VTEP routes and the FDB entries. |

An IPMI tier is not implemented.

GoBGP does not support BFD. Use FRR mode for BFD. See
[FRR and GoBGP compared](networking.md#frr-and-gobgp-compared).

## Quick start

Type-5 with a gateway VTEP (the default route mode). Put this in
`/deploy/vars`:

```bash
export NETWORK_MODE="gobgp"
export BGP_PEER_MODE="unnumbered"
export underlay_ip="10.0.0.20"
export asn_server="65020"
export provision_vni="100"
export provision_ip="10.100.0.20/24"
export provision_gateway="10.0.0.1"
export dns_resolver="8.8.8.8"
```

Direct Type-5 without a gateway VTEP (as in the `topology-type5.clab.yml`
lab):

```bash
export NETWORK_MODE="gobgp"
export BGP_PEER_MODE="unnumbered"
export EVPN_TYPE5_ONLY="true"
export underlay_ip="192.168.4.10"
export asn_server="65010"
export provision_vni="1000"
export provision_ip="10.200.0.10/32"
export vpn_rt="65000:1000"
export dns_resolver="8.8.8.8"
```

Add `EVPN_L2_ENABLED="true"` to the first example to also originate and
process Type-2 and Type-3 routes. Do not set `EVPN_L2_ENABLED` and
`EVPN_TYPE5_ONLY` together. See
[EVPN route modes](networking.md#evpn-route-modes-gobgp).

## Peer mode examples

The peer modes are described in
[GoBGP peer modes](networking.md#gobgp-peer-modes).

Unnumbered, restricted to two fabric NICs:

```bash
export BGP_PEER_MODE="unnumbered"
export BGP_INTERFACES="eth1,eth2"
```

Dual: unnumbered IPv4 underlay plus numbered EVPN sessions to route
reflectors:

```bash
export BGP_PEER_MODE="dual"
export BGP_NEIGHBORS="10.0.0.1,10.0.0.2"
```

Numbered, eBGP to AS 65000. The host must already reach the neighbor:

```bash
export BGP_PEER_MODE="numbered"
export BGP_NEIGHBORS="10.0.0.1"
export BGP_REMOTE_ASN="65000"
```

With `BGP_REMOTE_ASN` unset or `0`, the numbered sessions are iBGP.

## Network interfaces created

| Interface | Content |
|---|---|
| `dummy.underlay` | Router ID (underlay IP) `/32`. A netplan dummy device name replaces the default name. |
| `lo` | Overlay IP `/32` or `/128`. Without `overlay_subnet`, this is the router ID. |
| `vx<VNI>` | VXLAN device, for example `vx100`. UDP port 4789, MAC learning off, MTU = NIC MTU minus 50. Bound to the netplan VXLAN `link`, or to the underlay dummy when `VRF_NAME` is set. |
| `br.provision` | Bridge with `vx<VNI>` as member. Carries `provision_ip` and the router MAC. A netplan bridge name replaces the default name. |
| `VRF_NAME` | Only when `VRF_NAME` is set. Table `VRF_TABLE_ID`. Contains the underlay dummy and the fabric NICs, and the provision bridge unless netplan puts the bridge in another VRF. |

## Lab topology

`test/e2e/clab/topology-gobgp.clab.yml` runs four GoBGP BOOTy containers
against an FRR fabric:

| Node | ASN | Role |
|---|---|---|
| `spine01` | 65000 | FRR. Loopback `10.0.0.1`. VXLAN gateway for VNI 100 (`br100`, `10.100.0.1/24`). |
| `leaf01` | 65001 | FRR leaf |
| `rr01` | 65000 | FRR route reflector for the `booty-dual` numbered EVPN session |
| `booty-unnumbered` | 65020 | Unnumbered. FRR config with `advertise-all-vni`, so L2 mode is auto-detected |
| `booty-dual` | 65010 | Dual. No FRR config, so Type-5 with gateway |
| `booty-numbered` | 65030 | Numbered. Provision VNI is an FRR L3VNI, so Type-5 with gateway |
| `booty-l2` | 65040 | Unnumbered. `EVPN_L2_ENABLED=true` without FRR config |

All BOOTy nodes use `provision_gateway="10.0.0.1"`.

## Troubleshooting

BOOTy does not start the GoBGP gRPC API, so the `gobgp` CLI cannot connect.
Check the BGP state on the FRR peer and the kernel state in the ramdisk.

On the FRR peer:

```bash
vtysh -c "show bgp summary"
vtysh -c "show bgp l2vpn evpn route type prefix"
vtysh -c "show bgp l2vpn evpn route type macip"
vtysh -c "show bgp l2vpn evpn route type multicast"
```

In the BOOTy ramdisk (the default and `gobgp` flavors ship `ip` and `bridge`):

```bash
ip -d link show type vxlan
ip addr show dev br.provision
bridge fdb show dev vx100
ip route show 10.0.0.1/32
```

| Symptom | Cause | Fix |
|---|---|---|
| `provision gateway is required for GoBGP VXLAN data plane` | L2 or default route mode without `provision_gateway` | Set `provision_gateway`, or set `EVPN_TYPE5_ONLY=true` |
| `type5Only and EnableL2 cannot both be enabled` | `EVPN_L2_ENABLED=true` and `EVPN_TYPE5_ONLY=true` | Set only one |
| `timed out waiting for N BGP peer(s) to establish` | Fewer than `BGP_MIN_PEERS` sessions are up | Check cabling, `BGP_INTERFACES`, `BGP_NEIGHBORS` and the peer ASN |
| No BGP sessions in unnumbered mode | The peer link-local address is not found through NDP | Check that the fabric NICs are up and that the peer sends IPv6 RAs or NDP replies |
| BGP up, no EVPN routes processed | Route target mismatch | Set `vpn_rt` to the fabric route target, or align `asn_server:provision_vni` |
| ARP timeout on the overlay | No BUM FDB entry | Check `bridge fdb show dev vx<VNI>` for `00:00:00:00:00:00` |
| VXLAN packets dropped after encapsulation | No route to the remote VTEP | Check `ip route show <vtep>/32` (in the VRF table when `VRF_NAME` is set) |

## E2E tests

| Make targets | Build tag | Scope |
|---|---|---|
| `make clab-gobgp-up && make test-e2e-gobgp` | `e2e_gobgp` | All peer modes, EVPN route modes, gateway FDB and route, overlay reachability (`gobgp_e2e_test.go`, `gobgp_route_mode_e2e_test.go`) |
| `make clab-type5-up && make test-e2e-type5` | `e2e_gobgp_type5` | Type-5-only on a per-machine leaf fabric: per-host router MAC, VTEP routes, no Type-2/3 routes (`gobgp_type5_e2e_test.go`) |
| `make clab-gobgp-vrnetlab-up && make test-e2e-gobgp-vrnetlab` | `e2e_gobgp_vrnetlab` | GoBGP in QEMU VMs, all peer modes, FRR fabric only (`gobgp_vrnetlab_e2e_test.go`) |

The test files are in `test/e2e/integration/`. See
[e2e-test-matrix.md](e2e-test-matrix.md) for all suites.
