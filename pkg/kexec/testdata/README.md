# grub.cfg fixtures

Real GRUB configurations for `DefaultEntry`:

| Directory | Source |
|---|---|
| `ubuntu-2204-cloud` | `/boot/grub/grub.cfg` and `grubenv` of `ubuntu-22.04-server-cloudimg-amd64.img`, release 20260926 |
| `ubuntu-2404-gcp` | `/boot/grub/grub.cfg` of a GCP Ubuntu 24.04 VM (GRUB_FORCE_PARTUUID: the initrdless boot fallback, `linux` in both branches of `if [ "${initrdfail}" = 1 ]`) |
| `centos-stream-9` | `/boot/grub2/grub.cfg`, `grubenv` and the BLS entry of `CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2` |
| `flatcar` | `build_library/grub.cfg` of github.com/flatcar/scripts, placed at `boot/grub/grub.cfg` |
