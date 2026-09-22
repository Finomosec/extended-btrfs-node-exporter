# Extended BTRFS Node Exporter

A Prometheus exporter for detailed btrfs filesystem metrics. Goes beyond what the standard node_exporter provides — subvolume sizes, qgroup accounting, commit stats, device replace/balance/scrub progress, bees dedup stats, orphan tracking, and more.

Auto-discovers all mounted btrfs filesystems. Modular collectors can be individually enabled/disabled. Reads from sysfs and procfs where possible, minimizing subprocess overhead.

## Prerequisites

- **Linux** with btrfs filesystems
- **Go 1.23+** (for building)
- **Root access** (required for btrfs ioctls, sysfs, and `/proc` access)
- **Kernel 6.8+** (btrfs sysfs interface required)

### Optional

- **[bees](https://github.com/Zygo/bees)** — for dedup metrics (`bees_*`), reads status from `/run/bees/<uuid>.status`

## Installation

### From source

```bash
git clone https://github.com/Finomosec/extended-btrfs-node-exporter.git
cd extended-btrfs-node-exporter
go build -o extended-btrfs-node-exporter .
sudo cp extended-btrfs-node-exporter /usr/local/bin/
```

### systemd service

```bash
sudo cp extended-btrfs-node-exporter.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now extended-btrfs-node-exporter
```

Configuration via environment file (optional):

```bash
sudo cp .env.example /etc/default/extended-btrfs-node-exporter
sudo vi /etc/default/extended-btrfs-node-exporter
```

### Verify

```bash
curl http://localhost:9198/metrics | head -20
```

## Metrics

### Filesystem-level

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_total_bytes` | gauge | uuid, mountpoint | Total filesystem bytes |
| `btrfs_used_bytes` | gauge | uuid, mountpoint | Used filesystem bytes |
| `btrfs_free_bytes` | gauge | uuid, mountpoint | Free filesystem bytes |
| `btrfs_exclusive_operation` | gauge | uuid, mountpoint, name | Current exclusive operation (balance, resize, etc.) |

### Subvolume metrics (module: `COLLECT_SUBVOLUMES`, `COLLECT_SNAPSHOTS`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_subvolume_generations` | counter | uuid, mountpoint, subvolume, subvolume_id | Subvolume generation counter |

### Qgroup metrics (module: `COLLECT_QGROUPS`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_subvolume_referenced_bytes` | gauge | uuid, mountpoint, subvolume, subvolume_id | Subvolume referenced bytes |
| `btrfs_subvolume_exclusive_bytes` | gauge | uuid, mountpoint, subvolume, subvolume_id | Subvolume exclusive bytes |
| `btrfs_subvolume_disk_usage` | gauge | uuid, mountpoint, subvolume, subvolume_id | Subvolume disk usage (= exclusive bytes) |
| `btrfs_quota_rescan_running` | gauge | uuid, mountpoint | Whether quota rescan is active |
| `btrfs_quota_rescan_current_key` | counter | uuid, mountpoint | Quota rescan progress key |

### Commit metrics (module: `COLLECT_COMMIT`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_commit_commits` | counter | uuid, mountpoint | Total number of commits |
| `btrfs_commit_cur_commit_ms` | counter | uuid, mountpoint | Current commit duration in ms |
| `btrfs_commit_last_commit_ms` | counter | uuid, mountpoint | Last commit duration in ms |
| `btrfs_commit_max_commit_ms` | counter | uuid, mountpoint | Max commit duration in ms |
| `btrfs_commit_total_commit_ms` | counter | uuid, mountpoint | Total commit time in ms |
| `btrfs_commit_running` | gauge | uuid, mountpoint | Whether a commit is in progress (D-state detection) |

### Replace metrics (module: `COLLECT_REPLACE`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_replace_progress_percent` | gauge | uuid, mountpoint, target_device, missing_devid | Device replace progress |
| `btrfs_replace_write_errors_total` | counter | uuid, mountpoint, target_device, missing_devid | Replace write errors |
| `btrfs_replace_read_errors_total` | counter | uuid, mountpoint, target_device, missing_devid | Replace uncorrectable read errors |

### Balance metrics (module: `COLLECT_BALANCE`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_balance_chunks_done` | gauge | uuid, mountpoint | Balance chunks completed |
| `btrfs_balance_chunks_total` | gauge | uuid, mountpoint | Balance chunks total |
| `btrfs_balance_chunks_considered` | gauge | uuid, mountpoint | Balance chunks considered |
| `btrfs_balance_progress_percent` | gauge | uuid, mountpoint | Balance progress percent |
| `btrfs_balance_status` | gauge | uuid, mountpoint, status | Balance status (running/paused/pausing) |

### Resize metrics (only while `btrfs_exclusive_operation{name="resize"}`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_resize_chunks_remaining` | gauge | uuid, mountpoint, device, btrfs_dev_uuid | Dev extents beyond the device size a shrink still has to relocate |
| `btrfs_resize_bytes_remaining` | gauge | uuid, mountpoint, device, btrfs_dev_uuid | Bytes of those dev extents |
| `btrfs_resize_bytes_total` | gauge | uuid, mountpoint, device, btrfs_dev_uuid | Remaining bytes when the exporter first saw the shrink |
| `btrfs_resize_progress_percent` | gauge | uuid, mountpoint, device, btrfs_dev_uuid | Progress relative to `btrfs_resize_bytes_total` |

`btrfs_device_unused_bytes` is signed and goes negative while a shrink is running — the device size is already reduced while the extents beyond it are still allocated.

### Defrag metrics (module: `COLLECT_DEFRAG`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_defrag_running` | gauge | uuid, mountpoint | Number of defrag processes running |

### Orphan metrics (module: `COLLECT_ORPHANS`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_clean_orphans_left_to_clean` | gauge | uuid, mountpoint | Orphan subvolumes pending cleanup |
| `btrfs_clean_orphans_max_to_clean` | gauge | uuid, mountpoint | Peak orphan count seen |

### bcache mapping (module: `COLLECT_BCACHE`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `btrfs_device_bcache_info` | gauge | uuid, mountpoint, device, backing_device, bcache, disk | Always `1`; maps a btrfs device to the bcache backing device below it |

The node_exporter bcache collector labels its metrics with `backing_device="bdevN"`
and nothing else. `bdevN` is the attach order within a cache set and matches
neither the `bcacheN` device numbering nor the disk order, so those metrics
cannot be attributed to a filesystem on their own. This metric supplies the
missing link:

```promql
node_bcache_bypassed_bytes_total
  * on(backing_device) group_left(mountpoint, device, disk)
    btrfs_device_bcache_info
```

Emitted only for btrfs devices that sit on top of bcache. Hosts without bcache,
or filesystems on plain devices, simply produce no series — the slave chain
below each btrfs device is walked in sysfs, and anything unreadable is skipped.

### Bees dedup metrics (module: `COLLECT_BEES`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `bees_<key>` | counter | uuid, mountpoint | Bees dedup counters — one metric per key (e.g. `bees_dedup_bytes`, `bees_block_bytes`) |
| `bees_tasks_in_progress` | gauge | uuid, mountpoint | Bees tasks in progress |
| `bees_tasks_queued` | gauge | uuid, mountpoint | Bees tasks queued |
| `bees_thread_workers` | gauge | uuid, mountpoint | Bees worker threads |

## Configuration

All configuration is via environment variables (or `/etc/default/extended-btrfs-node-exporter` for systemd).

### General

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `9198` | HTTP listen port |
| `RESOLVE_DEVICE_MAPPER` | `false` | Resolve `dm-X` to `/dev/mapper/*` names in labels |
| `BEES_STATUS_DIR` | `/run/bees` | Directory for bees status files |
| `BEES_STALE_AFTER_SECS` | `30` | Skip bees metrics if the status file is older than this (daemon stopped) |
| `IOCTL_TIMEOUT_SECS` | `30` | Timeout for ioctl calls (protects against kernel lock contention) |
| `DEBUG` | `false` | Enable verbose debug logging |

### Filesystem filters

| Variable | Default | Description |
|----------|---------|-------------|
| `BTRFS_INCLUDE_UUIDS` | *(empty = all)* | Comma-separated UUIDs to include |
| `BTRFS_EXCLUDE_UUIDS` | *(empty)* | Comma-separated UUIDs to exclude |

### Subvolume filters

| Variable | Default | Description |
|----------|---------|-------------|
| `BTRFS_INCLUDE_SUBVOLUMES` | *(empty = all)* | Comma-separated substring matches to include |
| `BTRFS_EXCLUDE_SUBVOLUMES` | *(empty)* | Comma-separated substring matches to exclude |

### Collector modules

| Variable | Default | Description |
|----------|---------|-------------|
| `COLLECT_SUBVOLUMES` | `true` | Collect non-snapshot subvolume metrics |
| `COLLECT_SNAPSHOTS` | `true` | Collect snapshot subvolume metrics |
| `COLLECT_QGROUPS` | `true` | Collect qgroup (quota) metrics |
| `COLLECT_COMMIT` | `true` | Collect commit stats and running detection |
| `COLLECT_REPLACE` | `true` | Collect device replace progress |
| `COLLECT_BALANCE` | `true` | Collect balance progress |
| `COLLECT_SCRUB` | `true` | Collect scrub progress |
| `COLLECT_DEFRAG` | `true` | Collect defrag process detection |
| `COLLECT_BEES` | `true` | Collect bees dedup stats |
| `COLLECT_ORPHANS` | `true` | Collect orphan subvolume counts |
| `COLLECT_BCACHE` | `true` | Map btrfs devices to their bcache backing devices |

## Data Sources

The exporter reads from these sources (no external scripts required):

| Data | Source | Method |
|------|--------|--------|
| Filesystem space | `statfs()` syscall | Native |
| Commit stats | `/sys/fs/btrfs/<uuid>/commit_stats` | sysfs read |
| Exclusive operation | `/sys/fs/btrfs/<uuid>/exclusive_operation` | sysfs read |
| Replace target device | `/sys/fs/btrfs/<uuid>/devinfo/*/replace_target` | sysfs read |
| Commit running (D-state) | `/proc/<pid>/stat` + starttime correlation | procfs scan |
| Defrag detection | `/proc/<pid>/cmdline` | procfs scan |
| Bees stats | `/run/bees/<uuid>.status` | File read |
| Orphan count | `BTRFS_IOC_TREE_SEARCH` (orphan items) | ioctl |
| Subvolume list | `BTRFS_IOC_TREE_SEARCH` (root tree) | ioctl |
| Qgroup data | `BTRFS_IOC_TREE_SEARCH` (quota tree) | ioctl |
| Resize progress | `BTRFS_IOC_TREE_SEARCH` (dev tree, extents past the device size) | ioctl |
| Replace status | `BTRFS_IOC_DEV_REPLACE` | ioctl |
| Balance status | `BTRFS_IOC_BALANCE_PROGRESS` | ioctl |
| Quota rescan | `BTRFS_IOC_QUOTA_RESCAN_STATUS` | ioctl |
| Device mapping | `BTRFS_IOC_DEV_INFO` + sysfs size matching | ioctl + sysfs |

## Caveats

- **Snapshot detection** uses `parent_uuid` from btrfs metadata. A subvolume created via `btrfs subvolume snapshot` will be classified as a snapshot. If you restore a snapshot by renaming it to replace the original subvolume, it will still be detected as a snapshot (`parent_uuid` remains set). This only affects `COLLECT_SNAPSHOTS` / `COLLECT_SUBVOLUMES` filtering — the metrics themselves are always accurate.
- **`btrfs_commit_running`** detects D-state on btrfs-transaction kernel threads. PID-to-filesystem mapping is done natively via `/proc` starttime correlation with kernel log mount timestamps. Accuracy depends on kernel log not being rotated since boot.
- **Root required** — the exporter must run as root to access btrfs ioctls, sysfs, and `/proc` data.

## License

MIT
