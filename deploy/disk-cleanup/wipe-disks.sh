#!/bin/bash
#
# Generic disk cleanup for hosts returning to the Assisted Installer inventory.
#
# Runs from the discovery ISO where the OS lives entirely in RAM, so NO disk is
# mounted and every block device can be safely wiped. This makes the script
# storage-agnostic: it clears LVM (LVMO/topolvm), software RAID, Portworx, Ceph,
# and any filesystem signatures regardless of which cluster the host came from.
#
# Order matters: tear down logical layers (LVM, RAID) before wiping the raw disks
# so nothing holds the devices open.

set +o errexit   # best-effort: never abort the boot because one command failed
set +o pipefail

log() { echo "[wipe-disks] $*"; }

log "=== Starting generic disk cleanup ==="
log "Block devices before cleanup:"
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT 2>/dev/null || true

########################################
# 1. Tear down LVM (LVs -> VGs -> PVs)
########################################
if command -v vgchange >/dev/null 2>&1; then
  log "Deactivating all volume groups"
  vgchange -an 2>/dev/null

  for lv in $(lvs --noheadings -o lv_path 2>/dev/null | tr -d ' '); do
    log "Removing LV: $lv"
    lvremove -f "$lv" 2>/dev/null
  done

  for vg in $(vgs --noheadings -o vg_name 2>/dev/null | tr -d ' '); do
    log "Removing VG: $vg"
    vgremove -f "$vg" 2>/dev/null
  done

  for pv in $(pvs --noheadings -o pv_name 2>/dev/null | tr -d ' '); do
    log "Removing PV: $pv"
    pvremove -ff -y "$pv" 2>/dev/null
  done
else
  log "LVM tools not present, skipping LVM teardown"
fi

########################################
# 2. Stop software RAID and zero superblocks
########################################
if command -v mdadm >/dev/null 2>&1; then
  log "Stopping all RAID arrays"
  # Stop active arrays
  for md in $(ls /dev/md* 2>/dev/null); do
    log "Stopping RAID array: $md"
    mdadm --stop "$md" 2>/dev/null
  done
  mdadm --stop --scan 2>/dev/null

  # Zero superblocks on every partition and disk that has one
  for dev in $(lsblk -pnlo NAME 2>/dev/null); do
    if mdadm --examine "$dev" >/dev/null 2>&1; then
      log "Zeroing RAID superblock on: $dev"
      mdadm --zero-superblock --force "$dev" 2>/dev/null
    fi
  done
else
  log "mdadm not present, skipping RAID teardown"
fi

########################################
# 3. Flush device-mapper / multipath leftovers
########################################
if command -v dmsetup >/dev/null 2>&1; then
  log "Removing all device-mapper mappings"
  dmsetup remove_all 2>/dev/null
fi
if command -v multipath >/dev/null 2>&1; then
  log "Flushing multipath maps"
  multipath -F 2>/dev/null
fi

########################################
# 4. Wipe every physical disk
########################################
# Only whole disks of type "disk" -- skip loop, rom, and removable USB if desired.
for dev in $(lsblk -dpnlo NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}'); do
  log "Wiping signatures on: $dev"
  wipefs --all --force "$dev" 2>/dev/null

  # Determine device size in bytes to locate the tail
  size_bytes=$(blockdev --getsize64 "$dev" 2>/dev/null)

  log "Zeroing first 100MB of: $dev"
  dd if=/dev/zero of="$dev" bs=1M count=100 conv=fsync 2>/dev/null

  # Zero the last 100MB too -- mdadm 0.90/1.0 superblocks and some storage
  # backends (Portworx, Ceph) keep metadata at the tail of the device.
  if [ -n "$size_bytes" ] && [ "$size_bytes" -gt 209715200 ]; then
    seek_mb=$(( size_bytes / 1048576 - 100 ))
    log "Zeroing last 100MB of: $dev (seek=${seek_mb}MB)"
    dd if=/dev/zero of="$dev" bs=1M count=100 seek="$seek_mb" conv=fsync 2>/dev/null
  fi

  # Discard blocks on SSDs/NVMe where supported (fast, frees flash)
  blkdiscard "$dev" 2>/dev/null && log "blkdiscard succeeded on: $dev"
done

# Re-read partition tables so the kernel sees the wiped state
partprobe 2>/dev/null

log "Block devices after cleanup:"
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT 2>/dev/null || true
log "=== Disk cleanup completed ==="
exit 0
