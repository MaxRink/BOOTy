---
section: Behaviour changes in all modes
---

- BOOTy converts qcow2 images to raw in process and the ramdisks no longer
  ship `qemu-img`. qcow2 versions 2 and 3 work, with zlib or zstd compressed
  clusters, extended L2 entries and snapshots (the active state is written).
  Images with a backing file, an external data file or encryption fail as
  before. A truncated image (a table or cluster past the end of the file)
  and an image whose virtual size is larger than the target disk now fail
  before anything is written. The `split` build has no `qemu-img.cpio.zst`
  payload any more: remove it from the initrd list of the boot loader. A
  qcow2 image never needs a payload, so `BOOTY-PAY-001` covers only FRR and
  ipmitool. `qemu-img` is gone from the rescue shell too (#815).
