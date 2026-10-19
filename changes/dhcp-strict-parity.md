---
section: Behaviour changes in all modes
---

- DHCP replies that systemd-networkd 255 drops are dropped too: a DHCPv4
  message option (56) that is empty, has a NUL before its end or a byte
  over 127; a DHCPv6 reply with a second client or server identifier, a
  preference other than 1 byte, a status code under 2 bytes or other
  than success, or an information refresh time other than 4 bytes
  (#840).
