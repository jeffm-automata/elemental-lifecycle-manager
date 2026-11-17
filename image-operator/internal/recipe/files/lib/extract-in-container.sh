#!/bin/bash
# Runs INSIDE the inspection container, unprivileged: nothing is mounted. Takes the image apart with
# userspace tools and leaves what the checks need in /out. /in/image is the node image (read-only).
set -euo pipefail
cd /tmp
qemu-img convert -O raw /in/image disk.raw
sfdisk -d disk.raw > /out/partitions.txt
read -r ESTART ESIZE < <(sed -n 's/.*start= *\([0-9]*\), size= *\([0-9]*\),.*name="EFI".*/\1 \2/p' /out/partitions.txt)
read -r RSTART RSIZE < <(sed -n 's/.*start= *\([0-9]*\), size= *\([0-9]*\),.*name="RECOVERY".*/\1 \2/p' /out/partitions.txt)
dd if=disk.raw of=efi.img bs=512 skip="$ESTART" count="$ESIZE" status=none
dd if=disk.raw of=rec.img bs=512 skip="$RSTART" count="$RSIZE" status=none
blkid efi.img rec.img > /out/filesystems.txt
mkdir efi rec /out/efi /out/recovery /out/initrd-ext /out/os
mcopy -s -n -i efi.img ::/ efi/ >/dev/null
debugfs -R "rdump / /tmp/rec" rec.img >/dev/null 2>&1
# EFI partition: file list, boot loader configuration and environment (the kernel command line)
( cd efi && find . | sort ) > /out/efi/files.txt
find efi -name 'grub.cfg' -o -name grubenv | while read -r f; do mkdir -p "/out/$(dirname "$f")"; cp "$f" "/out/$f"; done
# Recovery partition: file list; everything but the OS squashfs and the big RKE2 archive is copied out
( cd rec && find . -printf '%y %s %p\n' | sort -k3 ) > /out/recovery/files.txt
( cd rec && find . -type f ! -name squashfs.img ! -name '*.tar.gz' ! -name '*.tar.zst' ! -name elemental3ctl ! -name hauler -size -20M -print0 |
    cpio -0 -pdm --quiet /out/recovery )
( cd rec && find . -type f -exec sha256sum {} + ) > /out/recovery/sha256.txt
# Configuration that customize appends to the installed system's initrd (Ignition base config)
( cd /out/initrd-ext && cpio -idm --quiet < /tmp/rec/Install/initrdExt.cpio )
# The OS the installer copies to disk: full listing, plus every place a per-node identity or a
# credential could sit. unsquashfs exits 2 on paths that do not exist; that is the expected case.
unsquashfs -lls rec/LiveOS/squashfs.img > /out/os/files.txt
unsquashfs -q -n -no-xattrs -d /out/os/root rec/LiveOS/squashfs.img \
  etc/machine-id etc/hostname etc/ssh etc/NetworkManager etc/sysconfig/network etc/shadow etc/os-release \
  usr/lib/os-release etc/rancher var/lib/rancher var/lib/elemental etc/elemental root home \
  var/lib/dbus/machine-id var/lib/systemd/random-seed etc/systemd/network etc/iscsi etc/nvme \
  usr/lib/systemd/system-preset etc/systemd/system >/dev/null 2>&1 || true
chmod -R u+rwX,go+rX /out
