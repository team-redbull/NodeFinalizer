# Generic Disk Cleanup for Inventory Hosts

Wipes **all** disks on a bare-metal host when it boots the Assisted Installer
discovery ISO, before the host registers as available in inventory. Because the
discovery OS runs entirely from RAM, no disk is mounted and every block device
can be wiped safely — regardless of which cluster the host came from or what
storage it ran (LVMO/topolvm, Portworx, Ceph, software RAID, plain filesystems).

This complements the NodeFinalizer webhook: the webhook removes the Kubernetes
objects (topolvm PVs, `LVMVolumeGroupNodeStatus`) while a hosted node is deleted,
and this cleanup guarantees the **physical** disks are clear before the host is
reused.

## Files

| File | Purpose |
|------|---------|
| `wipe-disks.sh` | The cleanup script. Tears down LVM, stops RAID + zeroes superblocks, flushes device-mapper/multipath, then `wipefs` + zeroes both ends of every disk and `blkdiscard`s SSDs. |
| `infraenv-ignition.yaml` | An `InfraEnv` that embeds the script as a base64 ignition file and runs it via a systemd unit ordered `Before=agent.service`. |

## What the script does

1. **LVM** — deactivate VGs, then remove LVs → VGs → PVs
2. **RAID** — stop all arrays, zero mdadm superblocks on every member
3. **device-mapper / multipath** — remove leftover mappings
4. **Every disk** — `wipefs --all --force`, zero first + last 100 MB (catches
   GPT/MBR, LVM, mdadm 0.90/1.0 tail superblocks, Portworx/Ceph headers), then
   `blkdiscard` where supported

The script is best-effort (`set +e`): a single failing command never blocks the
host from booting.

## Deploy

1. Edit `infraenv-ignition.yaml` and set `metadata.namespace`, `clusterRef`,
   `pullSecretRef`, and `sshAuthorizedKey` for your environment.
2. Apply it:
   ```bash
   oc apply -f infraenv-ignition.yaml
   ```
3. Any host that boots this InfraEnv's discovery ISO runs the wipe before it
   appears as available.

## Regenerating the embedded script

After editing `wipe-disks.sh`, re-encode and paste the result into the
`contents.source` field (after `base64,`) in `infraenv-ignition.yaml`:

```bash
base64 -w0 wipe-disks.sh
```

## Verify on a host

SSH into a booted discovery host (using `sshAuthorizedKey`) and check:

```bash
journalctl -u wipe-disks.service
lsblk -o NAME,SIZE,TYPE,FSTYPE   # data disks should show no FSTYPE / no children
```
