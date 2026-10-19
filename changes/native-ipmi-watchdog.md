---
section: Behaviour changes in all modes
---

- `WATCHDOG_IPMI_FALLBACK` sends the BMC watchdog commands (Set and Reset
  Watchdog Timer) over `/dev/ipmi0` in process, and the ramdisks no longer
  ship `ipmitool` or `libfreeipmi`. The `split` build has no
  `ipmitool.cpio.zst` payload any more: remove it from the initrd list of
  the boot loader. Only FRR is a payload now. The rescue shell has no
  `ipmitool`: read or change BMC settings over Redfish, the BMC web
  interface or `ipmitool -I lanplus` from another host (#816).
