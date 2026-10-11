# GoBGP EVPN Configuration Guide

This guide covers configuring BOOTy's pure-Go BGP stack for EVPN/VXLAN
overlay networking in bare-metal provisioning environments.

## Overview

BOOTy's GoBGP mode replaces FRR with an in-process BGP stack using a
three-tier architecture:

```
┌──────────────────────────────────────────────────┐
│ BOOTy (PID 1 in initramfs)                       │
│                                                  │
│  ┌──────────────────────────────────────────────┐│
│  │ Tier 1: Underlay                             ││
│  │  • eBGP peering with leaf/spine switches     ││
│  │  • Advertises local VTEP /32 (router-id)     ││
│  │  • Discovers peers via IPv6 link-local NDP   ││
│  └──────────────────────────────────────────────┘│
│  ┌──────────────────────────────────────────────┐│
│  │ Tier 2: Overlay (EVPN)                       ││
│  │  • Advertises Type-5 (IP Prefix) routes      ││
│  │  • Optional Type-2/3 L2 control plane        ││
│  │  • VXLAN + bridge for provisioning network   ││
│  │  • Gateway VTEP route for baseline reach     ││
│  └──────────────────────────────────────────────┘│
│  ┌──────────────────────────────────────────────┐│
│  │ Tier 3: IPMI (optional, planned)             ││
│  │  • L3 path to BMC                            ││
│  └──────────────────────────────────────────────┘│
└──────────────────────────────────────────────────┘
```

## Quick Start

Add these variables to your `/deploy/vars` file:

```bash
# Required for GoBGP mode
export NETWORK_MODE="gobgp"
export BGP_PEER_MODE="unnumbered"
export underlay_ip="10.0.0.20"
export asn_server="65020"
export provision_vni="100"
export provision_ip="10.100.0.20/24"
export provision_gateway="10.0.0.1"
export dns_resolver="8.8.8.8"
```

## Configuration Reference

### Core Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `NETWORK_MODE` | Yes | — | Must be `gobgp` to activate GoBGP stack |
| `BGP_PEER_MODE` | No | `unnumbered` | Peering mode (see below) |
| `BGP_INTERFACES` | No | — | Comma-separated interface allowlist for unnumbered/dual peers; empty uses all detected physical NICs |
| `underlay_ip` | Yes | — | Local VTEP IP / BGP router-id (e.g. `10.0.0.20`) |
| `asn_server` | Yes | — | Local BGP AS number |
| `provision_vni` | Yes | — | VXLAN VNI for provisioning network. With `asn_server > 65535`, values above 65535 are rejected because 4-octet ASN RD/RT local-admin values are 16-bit. |
| `provision_ip` | Yes | — | Overlay IP in CIDR notation (e.g. `10.100.0.20/24`) |
| `provision_gateway` | L2 only | — | Remote VTEP IP for gateway route + BUM FDB; optional for direct Type-5 |
| `dns_resolver` | No | — | DNS resolver IP |
| `overlay_subnet` | No | — | IPv6 overlay subnet (optional) |
| `EVPN_L2_ENABLED` | No | `false` | Enable Type-2/3 EVPN route origination and handling |
| `EVPN_TYPE5_ONLY` | No | `false` | Explicitly select direct Type-5 routing; mutually exclusive with `EVPN_L2_ENABLED` |

GoBGP currently does not implement BFD. `BFD_TRANSMIT_MS` and
`BFD_RECEIVE_MS` are consumed by the FRR backend; GoBGP relies on keepalive and
hold timers for failure detection.

### Peering Mode Variables

| Variable | Modes | Description |
|----------|-------|-------------|
| `BGP_INTERFACES` | `unnumbered`, `dual` | Restrict interface peering to listed NICs, useful when management or bootstrap NICs are present |
| `BGP_NEIGHBORS` | `dual`, `numbered` | Comma-separated peer IPs |
| `BGP_REMOTE_ASN` | `numbered` | Remote ASN (0 = iBGP) |

## Peering Modes

### Unnumbered (Default)

Best for leaf-spine topologies with IPv6 link-local peering:

```bash
export BGP_PEER_MODE="unnumbered"
export BGP_INTERFACES="eth1,eth2"  # optional; omit to use all physical NICs
```

- Discovers peers automatically via IPv6 NDP on all physical NICs
- Restricts discovery to `BGP_INTERFACES` when set, avoiding peering on management or provisioning-only NICs
- Carries both IPv4 unicast and L2VPN-EVPN on the same sessions
- No explicit neighbor configuration needed

### Dual

Separate underlay and overlay peering — underlay on link-local, overlay
on numbered sessions to route reflectors:

```bash
export BGP_PEER_MODE="dual"
export BGP_NEIGHBORS="10.0.0.1,10.0.0.2"
```

- Underlay: unnumbered eBGP (IPv4 unicast only)
- Overlay: numbered eBGP to `BGP_NEIGHBORS` (L2VPN-EVPN)

### Numbered

Traditional IP-based peering — requires the machine to already have
underlay connectivity (e.g. via DHCP or static IP):

```bash
export BGP_PEER_MODE="numbered"
export BGP_NEIGHBORS="10.0.0.1"
export BGP_REMOTE_ASN="65000"
```

## EVPN Data Plane

### What BOOTy Advertises

BOOTy advertises **Type-5 (IP Prefix)** routes for provisioning host
reachability. Set `EVPN_TYPE5_ONLY=true` to use direct Type-5 forwarding
without a configured gateway VTEP.

Type-5-only advertisements use a `0.0.0.0` EVPN gateway address and retain
the VTEP as the BGP next hop. They include the VXLAN tunnel-encapsulation and
router-MAC extended communities. This makes them direct Type-5 routes: peers
install an on-link route through the VTEP and program the router-MAC FDB entry
without requiring Type-2 MAC/IP resolution.

The router MAC is the provision bridge MAC. BOOTy selects it in this order:

1. The netplan `macaddress` of the provision bridge, used as given. It must
   be a 48-bit MAC.
2. `02:54` and the last 4 bytes of the IPMI MAC.
3. `02:54` and the 4 bytes of the underlay IPv4, like the legacy
   `configure-frr.py` ramdisk.

The derived MACs are stable, host specific, locally administered unicast
addresses. The IPv4 underlay MAC is unique because the underlay IP is the
VTEP address. The IPMI MAC keeps only 4 bytes, so a collision is
unlikely but possible. Set the netplan `macaddress` if you need a fixed value.

When `EVPN_L2_ENABLED=true`, BOOTy also originates:

| Route Type | Purpose |
|------------|---------|
| **Type-2** (MAC/IP) | Advertise the local bridge MAC and provisioning IP |
| **Type-3** (Inclusive Multicast) | Advertise this VTEP as a BUM flooding target |

### What BOOTy Receives

The `watchRoutes()` function monitors GoBGP's route stream and processes
incoming EVPN routes from the fabric. Type-5 routes are always handled. Type-2
and Type-3 handling is enabled only when `EVPN_L2_ENABLED=true`.

| Route Type | Action | Purpose |
|------------|--------|---------|
| **Type-5** (IP Prefix) | Learns remote VTEP prefix reachability | Route overlay traffic toward remote VTEPs |
| **Type-2** (MAC/IP) | Installs unicast FDB entry (`MAC → VTEP`) | Learn remote MACs via control plane |
| **Type-3** (Inclusive Multicast) | Installs BUM FDB entry (`00:00:00:00:00:00 → VTEP`) | Enable BUM flooding to remote VTEPs |

Routes originating from BOOTy's own router-id are skipped.

### Gateway Connectivity

In an L2 overlay, `provision_gateway` is required. When set, BOOTy:

1. **Installs a /32 kernel route** to the gateway VTEP via the first
   physical NIC (`installGatewayRoute()`). This ensures VXLAN-encapsulated
   packets can reach the remote VTEP.

2. **Adds a BUM FDB entry** on the VXLAN device pointing to the gateway
   VTEP (`addGatewayFDB()`). This ensures ARP/broadcast frames are
   flooded to the gateway before dynamic routes arrive.

When netplan defines a VXLAN `link`, BOOTy binds the VXLAN device to that
underlay link (`VtepDevIndex`) and creates the configured underlay dummy with
the same name. This preserves the selected VRF's routing table for outer
VXLAN packets; configurations without an explicit link retain the historical
unbound behavior unless an underlay VRF is configured.

For a direct Type-5-only fabric, set `EVPN_TYPE5_ONLY=true` and omit
`provision_gateway`. BOOTy learns each remote VTEP from the Type-5 next hop,
installs its underlay /32 reachability route, and programs the router-MAC FDB
from the route's extended community. DCGWs in this topology must not own an
L2VNI, SVI, bridge, VXLAN interface, or provisioning gateway address.

## Network Interfaces Created

| Interface | Description |
|-----------|-------------|
| `lo` (modified) | Router-id /32 added to loopback |
| `vx<VNI>` | VXLAN device (e.g. `vx100` for VNI 100) |
| `br.provision` | Linux bridge enslaving the VXLAN device |
| `dummy-prov` | Dummy interface for overlay loopback (in VRF) |
| `vrf-prov` | VRF device for provisioning network isolation |

## Topology Example

A minimal ContainerLab topology for testing:

```
         ┌─────────────┐
         │   spine01    │
         │ AS 65000     │
         │ lo: 10.0.0.1 │
         │ VTEP gateway │
         └──┬───────┬──┘
            │       │
     eth1-2 │       │ eth3-5
            │       │
    ┌───────┴─┐  ┌──┴────────┐
    │ leaf01  │  │  BOOTy    │
    │ AS 65001│  │  AS 65020 │
    │         │  │  VTEP:    │
    │         │  │ 10.0.0.20 │
    └─────────┘  └───────────┘
```

The spine acts as both the BGP route reflector and the VXLAN gateway.
Its loopback (`10.0.0.1`) is used as `provision_gateway` by BOOTy.

## Troubleshooting

### Verify BGP Sessions

```bash
# Inside the BOOTy container/VM
gobgp neighbor          # List BGP peers and state
gobgp global rib -a l2vpn-evpn   # Show EVPN routes
```

### Verify VXLAN Data Plane

```bash
# Check VXLAN interface
ip -d link show type vxlan

# Check bridge
ip addr show dev br.provision

# Check FDB entries (should see BUM entry for gateway)
bridge fdb show dev vx100

# Check kernel route to gateway VTEP
ip route show 10.0.0.1/32

# Test overlay connectivity
ping -c 1 -I br.provision <gateway_provision_ip>
```

### Common Issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| No BGP sessions | NICs not detected | Check physical NIC availability, retry timing |
| BGP up but no EVPN routes | Wrong address family | Verify `L2VPN-EVPN` family is negotiated |
| VXLAN created but no connectivity | Missing `provision_gateway` | Set `provision_gateway` to spine loopback IP |
| ARP timeout on overlay | No BUM FDB entry | Check `bridge fdb show dev vx<VNI>` for `00:00:00:00:00:00` entry |
| Packets dropped after VXLAN encap | No route to VTEP | Check `ip route show <gateway_ip>/32` exists |

## E2E Testing

GoBGP E2E tests use ContainerLab with the `e2e_gobgp` build tag:

```bash
make clab-gobgp-up && make test-e2e-gobgp
```

For full EVPN validation with QEMU BOOTy VMs and FRR-based fabric containers:

```bash
make clab-gobgp-vrnetlab-up && make test-e2e-gobgp-vrnetlab
```

See `test/e2e/integration/gobgp_e2e_test.go` for the test suite, which
covers BGP peering, EVPN route exchange, VXLAN interface creation,
FDB entries, gateway routes, and overlay ping connectivity.
