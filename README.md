# Extended BTRFS Node Exporter

A Prometheus exporter for detailed btrfs filesystem metrics. Collects subvolume sizes, qgroup accounting, commit stats, replace/balance/scrub progress, bees dedup stats, and more.

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `btrfs_total_bytes` | gauge | Total filesystem bytes |
| `btrfs_used_bytes` | gauge | Used filesystem bytes |
| `btrfs_free_bytes` | gauge | Free filesystem bytes |
| `btrfs_subvolume_generations` | counter | Subvolume generation counter |
| `btrfs_subvolume_referenced_bytes` | gauge | Subvolume referenced bytes (qgroup) |
| `btrfs_subvolume_exclusive_bytes` | gauge | Subvolume exclusive bytes (qgroup) |
| `btrfs_subvolume_disk_usage` | gauge | Subvolume disk usage (= exclusive bytes) |
| `btrfs_commit_commits` | counter | Total commits |
| `btrfs_commit_cur_commit_ms` | counter | Current commit duration |
| `btrfs_commit_last_commit_ms` | counter | Last commit duration |
| `btrfs_commit_max_commit_ms` | counter | Max commit duration |
| `btrfs_commit_total_commit_ms` | counter | Total commit time |
| `btrfs_commit_running` | gauge | Commit in progress (D-state) |
| `btrfs_exclusive_operation` | gauge | Current exclusive operation |
| `btrfs_defrag_running` | gauge | Defrag processes running |
| `btrfs_quota_rescan_running` | gauge | Quota rescan active |
| `btrfs_replace_progress_percent` | gauge | Device replace progress |
| `btrfs_balance_progress_percent` | gauge | Balance progress |
| `btrfs_clean_orphans_left_to_clean` | gauge | Orphan subvolumes pending |
| `bees_*` | counter/gauge | Bees dedup stats |

## Configuration

Via `.env` file or environment variables:

```env
PORT=9198                          # HTTP listen port

# Filesystem filters
BTRFS_INCLUDE_UUIDS=               # Comma-separated, empty = all
BTRFS_EXCLUDE_UUIDS=               # Comma-separated

# Subvolume name filters
BTRFS_INCLUDE_SUBVOLUMES=           # Comma-separated substring matches
BTRFS_EXCLUDE_SUBVOLUMES=           # Comma-separated substring matches

# Collector modules (all enabled by default)
COLLECT_SUBVOLUMES=true             # Non-snapshot subvolumes
COLLECT_SNAPSHOTS=true              # Snapshot subvolumes
COLLECT_QGROUPS=true
COLLECT_COMMIT=true
COLLECT_REPLACE=true
COLLECT_BALANCE=true
COLLECT_SCRUB=true
COLLECT_DEFRAG=true
COLLECT_BEES=true
COLLECT_ORPHANS=true

BEES_STATUS_DIR=/run/bees
```

## Build & Run

```bash
go build -o extended-btrfs-node-exporter .
sudo ./extended-btrfs-node-exporter
```

Requires root for btrfs ioctls and sysfs access.

## Caveats

- **Snapshot detection** uses `parent_uuid` from btrfs metadata. A subvolume created via `btrfs subvolume snapshot` will be classified as a snapshot. If you restore a snapshot by renaming it to replace the original subvolume, it will still be detected as a snapshot (parent_uuid remains set). This only affects the `COLLECT_SNAPSHOTS` / `COLLECT_SUBVOLUMES` filtering — the metrics themselves are always accurate.
- **Simple quotas** (kernel 6.7+) may have delayed qgroup updates on older kernels (< 6.11). Kernel 6.17+ is recommended.
- **`btrfs_commit_running`** relies on `/run/btrfs-thread-map` for PID-to-filesystem mapping.
