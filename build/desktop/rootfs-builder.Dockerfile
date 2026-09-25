# rootfs-builder.Dockerfile — the container `mage desktop:rootfs`
# (magefiles/desktop.go) runs to convert the Ubuntu cloud image to a raw
# disk and customize it (k3s + apguest + systemd units + the virtiofs
# fstab entry), entirely inside Docker.
#
# This is deliberately NOT a libguestfs/virt-customize image (that was
# tried first — see the task report at
# .superpowers/sdd/task-rootfs-docker-report.md — virt-customize's
# supermin appliance needs a working KVM/TCG backend that Docker Desktop
# for Mac's nested-virtualization story doesn't reliably provide). Instead
# this carries just qemu-img (image format conversion) and plain
# loop-mount tooling (losetup, mount, e2fsprogs) — rootfs-customize.sh
# loop-mounts the converted raw disk's root partition and copies files in
# directly, no appliance/hypervisor required. Needs `docker run
# --privileged -v /dev:/dev` (see that script's doc comment for why the
# /dev bind-mount specifically is required).
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      qemu-utils \
      util-linux \
      e2fsprogs \
      cloud-guest-utils \
      gdisk \
    && rm -rf /var/lib/apt/lists/*

COPY rootfs-customize.sh /usr/local/bin/rootfs-customize.sh
RUN chmod 0755 /usr/local/bin/rootfs-customize.sh

ENTRYPOINT ["/usr/local/bin/rootfs-customize.sh"]
