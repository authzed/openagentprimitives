#!/bin/bash
# rootfs-customize.sh — runs INSIDE the rootfs-builder image (see
# rootfs-builder.Dockerfile) as the container's ENTRYPOINT for `mage
# desktop:rootfs` (magefiles/desktop.go). Converts the bind-mounted Ubuntu
# cloud image to a raw disk, resizes it, loop-mounts its root partition,
# and copies in the k3s/apguest/systemd-unit payload — the exact same
# guest-side content the previous virt-customize invocation produced (same
# copy-ins, same mkdirs, same service-enable symlinks, same fstab line),
# just performed with qemu-img + losetup + mount instead of libguestfs.
# See the task report (.superpowers/sdd/task-rootfs-docker-report.md) for
# why: virt-customize's supermin appliance needs a working KVM/TCG backend
# that Docker Desktop for Mac's nested-virtualization story doesn't
# reliably provide; a loop-mounted ext4 partition needs nothing but
# --privileged and a *live* /dev (see below).
#
# Expected bind mounts (fixed paths; set by magefiles/desktop.go's
# desktopBuildRootfsInDocker):
#   /work/cloudimg.img                 Ubuntu qcow2 cloud image (ro)
#   /work/k3s-airgap-images.tar.zst    k3s's own air-gap image set (ro)
#   /work/static/                      build/desktop/*.service,
#                                       *.conf, k3s-config.yaml (ro,
#                                       checked into git)
#   /work/out/                         build/desktop/out/ (rw) — holds the
#                                       staged k3s + apguest binaries and
#                                       images-minimal.tar.zst as INPUTS,
#                                       and rootfs.img as OUTPUT
#
# Expected env vars:
#   DISK_SIZE           e.g. "10G" — qemu-img resize target for the raw disk
#   FSTAB_LINE          the virtiofs fstab line, appended to /etc/fstab
#                       verbatim
#   SSH_AUTHORIZED_KEY  the single-line public half of the ed25519 keypair
#                       magefiles/desktop.go's desktopGenerateSSHKeypair
#                       generates fresh for this build, baked into root's
#                       authorized_keys below — see
#                       cmd/oap/internal/desktop/vz/provider_darwin.go's
#                       package doc for the guest<->host SSH handoff this
#                       key exists for.
#
# Why `-v /dev:/dev` is required (not just --privileged): Docker gives
# each container its own private /dev (a fresh tmpfs seeded with a handful
# of static nodes), so device nodes the loop driver creates AFTER the
# container starts — the "p1"/"p15"/"p16" partition sub-devices `losetup
# -P` creates — never appear there; `mount` fails with "not a valid block
# device" even though `/proc/partitions` shows the kernel registered them
# correctly. Bind-mounting the HOST's real /dev (itself devtmpfs) gives
# the container a live view of the same instance the kernel is updating,
# which is what actually makes new loop-partition nodes visible.
set -euo pipefail

CLOUDIMG=/work/cloudimg.img
K3S_IMAGES_TAR=/work/k3s-airgap-images.tar.zst
STATIC=/work/static
OUT=/work/out
ROOTFS="$OUT/rootfs.img"
MNT=/mnt/rootfs

: "${DISK_SIZE:?DISK_SIZE env var required}"
: "${FSTAB_LINE:?FSTAB_LINE env var required}"
: "${SSH_AUTHORIZED_KEY:?SSH_AUTHORIZED_KEY env var required}"

for f in "$CLOUDIMG" "$K3S_IMAGES_TAR" "$OUT/k3s" "$OUT/apguest" "$OUT/images-minimal.tar.zst"; do
  if [ ! -f "$f" ]; then
    echo "rootfs-customize: expected input $f missing (bind-mount misconfigured?)" >&2
    exit 1
  fi
done

echo "==> rootfs-customize: qemu-img convert -O raw"
rm -f "$ROOTFS"
qemu-img convert -O raw "$CLOUDIMG" "$ROOTFS"

echo "==> rootfs-customize: qemu-img resize to $DISK_SIZE"
qemu-img resize -f raw "$ROOTFS" "$DISK_SIZE"

LOOPDEV=""
cleanup() {
  local rc=$?
  set +e
  if mountpoint -q "$MNT" 2>/dev/null; then
    umount "$MNT"
  fi
  if [ -n "$LOOPDEV" ]; then
    losetup -d "$LOOPDEV"
  fi
  exit "$rc"
}
trap cleanup EXIT

echo "==> rootfs-customize: losetup -P $ROOTFS"
LOOPDEV=$(losetup -fP --show "$ROOTFS")
echo "    loop device: $LOOPDEV"

# The partition sub-device node can lag the LOOP_SET_STATUS64 ioctl by a
# beat even against a live devtmpfs (observed under Docker Desktop for
# Mac) — poll instead of assuming ${LOOPDEV}p1 exists immediately.
part=""
for _ in $(seq 1 30); do
  if [ -b "${LOOPDEV}p1" ]; then
    part="${LOOPDEV}p1"
    break
  fi
  sleep 0.5
done
if [ -z "$part" ]; then
  echo "rootfs-customize: ${LOOPDEV}p1 never appeared after losetup -P" >&2
  exit 1
fi

# Grow the root partition + ext4 filesystem to fill the resized disk. The
# Ubuntu cloud image ships a ~2.4G root partition (partition 1, laid out LAST
# on the disk precisely so it can grow); qemu-img enlarged the DISK to
# rootfsDiskSize but the PARTITION + filesystem still cap at ~2.4G. Left
# ungrown, the root fs fills the moment k3s imports the baked air-gap images
# and sets up its datastore — k3s then loops on "failed to setup db: database
# or disk is full" and never serves :6443 (bring-up fails downstream in
# `oap install`'s cloud-provider probe). growpart extends p1 into the free
# space; resize2fs then grows the ext4 fs onto the enlarged partition.
echo "==> rootfs-customize: growpart + resize2fs (fill the disk)"
growpart "$LOOPDEV" 1
partx -u "$LOOPDEV" 2>/dev/null || losetup -c "$LOOPDEV"
for _ in $(seq 1 30); do [ -b "$part" ] && break; sleep 0.5; done
e2fsck -fy "$part" || true   # exit 1 == errors auto-corrected; resize2fs needs a clean fs
resize2fs "$part"

mkdir -p "$MNT"
mount "$part" "$MNT"
echo "    mounted $part -> $MNT"

echo "==> rootfs-customize: mkdir target directories"
mkdir -p "$MNT/etc/rancher/k3s"
mkdir -p "$MNT/var/lib/rancher/k3s/agent/images"
mkdir -p "$MNT/var/lib/ap"
mkdir -p "$MNT/root/.ssh"
mkdir -p "$MNT/etc/ssh/sshd_config.d"
mkdir -p "$MNT/etc/systemd/system/ssh.service.d"
mkdir -p "$MNT/etc/systemd/network"
mkdir -p "$MNT/etc/systemd/system/systemd-networkd-wait-online.service.d"

echo "==> rootfs-customize: copy-in binaries + scripts"
install -m 0755 "$OUT/k3s" "$MNT/usr/local/bin/k3s"
install -m 0755 "$OUT/apguest" "$MNT/usr/local/bin/apguest"

echo "==> rootfs-customize: copy-in systemd units"
install -m 0644 "$STATIC/k3s.service" "$MNT/etc/systemd/system/k3s.service"
install -m 0644 "$STATIC/apguest.service" "$MNT/etc/systemd/system/apguest.service"
install -m 0644 "$STATIC/ssh-hostkeys.service" "$MNT/etc/systemd/system/ssh-hostkeys.service"
install -m 0644 "$STATIC/ssh-service-hostkeys.conf" "$MNT/etc/systemd/system/ssh.service.d/10-ap-hostkeys.conf"

echo "==> rootfs-customize: bake guest DHCP networking (no cloud-init, no netplan)"
# The stock Noble cloud image ships an EMPTY /etc/netplan/ and an EMPTY
# /etc/systemd/network/ — systemd-networkd.service is enabled (confirmed by
# loop-mount inspection) but has nothing to configure, so the virtio NIC
# never DHCPs and never gets an IP. This is the root cause of GuestIP
# (cmd/oap/internal/desktop/vz/provider_darwin.go) timing out after 2
# minutes: no DHCP lease ever appears in the host's NAT lease table, and
# the guest agent's vsock IP report never fires because the interface it
# would report never comes up. Bake a DHCP config directly (bypassing
# cloud-init/netplan entirely, same posture as every other unit this
# script enables) so systemd-networkd brings the NIC up unconditionally.
# The Name= glob covers both vz's typical enp0s1-style name and the
# en*/eth* aliases udev may assign.
install -m 0644 "$STATIC/10-ap-dhcp.network" "$MNT/etc/systemd/network/10-ap-dhcp.network"
# systemd-networkd-wait-online.service is already enabled (WantedBy
# network-online.target) and, until now, had nothing to wait for since no
# interface was ever managed. Now that 10-ap-dhcp.network gives it
# something to wait for, bound the wait so a DHCP hiccup delays boot
# instead of deadlocking it — see wait-online-timeout.conf's doc comment.
install -m 0644 "$STATIC/wait-online-timeout.conf" "$MNT/etc/systemd/system/systemd-networkd-wait-online.service.d/10-ap-timeout.conf"

echo "==> rootfs-customize: copy-in k3s config + air-gap image sets"
install -m 0644 "$STATIC/k3s-config.yaml" "$MNT/etc/rancher/k3s/config.yaml"
install -m 0644 "$K3S_IMAGES_TAR" "$MNT/var/lib/rancher/k3s/agent/images/k3s-airgap-images-arm64.tar.zst"
install -m 0644 "$OUT/images-minimal.tar.zst" "$MNT/var/lib/rancher/k3s/agent/images/images-minimal.tar.zst"

echo "==> rootfs-customize: bake SSH access (root, key-only — see ssh-hostkeys.service and sshd-ap.conf)"
install -m 0644 "$STATIC/sshd-ap.conf" "$MNT/etc/ssh/sshd_config.d/10-ap.conf"
printf '%s\n' "$SSH_AUTHORIZED_KEY" >"$MNT/root/.ssh/authorized_keys"
chmod 0700 "$MNT/root/.ssh"
chmod 0600 "$MNT/root/.ssh/authorized_keys"

echo "==> rootfs-customize: enable services (systemctl-enable-equivalent symlinks)"
mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
# k3s/apguest/ssh-hostkeys are OUR units, installed directly under
# /etc/systemd/system above.
for unit in k3s apguest ssh-hostkeys; do
  ln -sf "/etc/systemd/system/${unit}.service" "$MNT/etc/systemd/system/multi-user.target.wants/${unit}.service"
done
# ssh.service is Ubuntu's vendored unit, under /lib/systemd/system.
# Confirmed by direct loop-mount inspection: the stock Noble cloud image
# does NOT enable it (no multi-user.target.wants symlink) even though its
# sibling ssh.socket IS enabled (socket-activates ssh.service on the first
# connection) — enable it directly the same way as the units above instead
# of relying on that socket-activation path, for the same reason this
# bundle bypasses cloud-init everywhere else: a static, unconditional
# enable is simpler to reason about than a generator- or datasource-gated
# default.
ln -sf "/lib/systemd/system/ssh.service" "$MNT/etc/systemd/system/multi-user.target.wants/ssh.service"

echo "==> rootfs-customize: append virtiofs fstab entry"
echo "$FSTAB_LINE" >>"$MNT/etc/fstab"

echo "==> rootfs-customize: done -> $ROOTFS"
