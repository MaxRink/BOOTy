---
section: Behaviour changes in all modes
---

- GoBGP mode: a new connection while the session is established is a
  graceful restart only when the new OPEN says that the peer preserved
  forwarding state. The decision uses the new OPEN, not the established
  session, so a peer that started with F=0 can still restart with
  forwarding preserved. The GR F bit wins per family; an LLGR tuple counts
  only for a family that the GR capability does not list and only with a
  nonzero stale time. Only families negotiated for the session and the
  locally advertised GR/LLGR capabilities count. FRR helper-only leaves
  advertise GR F=0, LLGR F=1 and stale time zero, so their late connection
  after a carrier fault no longer ends the just-recovered session a second
  time. The OPEN is read in the background, so `DisablePeer`/`EnablePeer`
  are not blocked by a slow peer, and the replayed connection keeps
  `syscall.Conn` for the socket options. The fork tag is v4.11.0-booty.5
  (#876).
