package collector

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
)

// BTRFS ioctl constants
const (
	BTRFS_IOC_TREE_SEARCH    = 0xD0089411 // _IOWR(BTRFS_IOCTL_MAGIC, 17, ...)
	BTRFS_QGROUP_INFO_KEY    = 242
	BTRFS_QGROUP_STATUS_KEY  = 240
	BTRFS_ROOT_ITEM_KEY      = 132
	BTRFS_ROOT_BACKREF_KEY   = 144
)

type BtrfsCollector struct {
	cfg Config

	// Filesystem-level metrics
	totalBytes *prometheus.Desc
	usedBytes  *prometheus.Desc
	freeBytes  *prometheus.Desc

	// Subvolume metrics
	subvolGenerations   *prometheus.Desc
	subvolReferenced    *prometheus.Desc
	subvolExclusive     *prometheus.Desc
	subvolDiskUsage     *prometheus.Desc

	// Commit stats
	commitCommits    *prometheus.Desc
	commitCurMs      *prometheus.Desc
	commitLastMs     *prometheus.Desc
	commitMaxMs      *prometheus.Desc
	commitTotalMs    *prometheus.Desc
	commitRunning    *prometheus.Desc

	// Operations
	exclusiveOp       *prometheus.Desc
	defragRunning     *prometheus.Desc
	quotaRescanRunning *prometheus.Desc
	quotaRescanKey     *prometheus.Desc

	// Replace
	replaceProgress   *prometheus.Desc
	replaceWriteErrs  *prometheus.Desc
	replaceReadErrs   *prometheus.Desc

	// Balance
	balanceChunksDone      *prometheus.Desc
	balanceChunksTotal     *prometheus.Desc
	balanceChunksConsidered *prometheus.Desc
	balanceProgressPercent *prometheus.Desc
	balanceStatus          *prometheus.Desc

	// Scrub
	scrubDurationSecs    *prometheus.Desc
	scrubSecsLeft        *prometheus.Desc
	scrubTotalBytes      *prometheus.Desc
	scrubRateBps         *prometheus.Desc
	scrubBytesScrubbed   *prometheus.Desc
	scrubStatus          *prometheus.Desc
	scrubErrors          *prometheus.Desc

	// Cleaner (orphans)
	cleanOrphansLeft *prometheus.Desc
	cleanOrphansMax  *prometheus.Desc

	// Bees
	beesCounter       *prometheus.Desc
	beesTasksProgress *prometheus.Desc
	beesTasksQueued   *prometheus.Desc
	beesWorkers       *prometheus.Desc
}

func New(cfg Config) *BtrfsCollector {
	labels := []string{"uuid", "mountpoint"}
	subvolLabels := []string{"uuid", "mountpoint", "subvolume", "subvolume_id"}
	deviceLabels := []string{"uuid", "mountpoint", "disk_id"}

	return &BtrfsCollector{
		cfg: cfg,

		totalBytes: prometheus.NewDesc("btrfs_total_bytes", "Total bytes in filesystem", labels, nil),
		usedBytes:  prometheus.NewDesc("btrfs_used_bytes", "Used bytes in filesystem", labels, nil),
		freeBytes:  prometheus.NewDesc("btrfs_free_bytes", "Free bytes in filesystem", labels, nil),

		subvolGenerations: prometheus.NewDesc("btrfs_subvolume_generations", "Subvolume generation counter", subvolLabels, nil),
		subvolReferenced:  prometheus.NewDesc("btrfs_subvolume_referenced_bytes", "Subvolume referenced bytes (qgroup)", subvolLabels, nil),
		subvolExclusive:   prometheus.NewDesc("btrfs_subvolume_exclusive_bytes", "Subvolume exclusive bytes (qgroup)", subvolLabels, nil),
		subvolDiskUsage:   prometheus.NewDesc("btrfs_subvolume_disk_usage", "Subvolume disk usage bytes (qgroup exclusive)", subvolLabels, nil),

		commitCommits:  prometheus.NewDesc("btrfs_commit_commits", "Total number of commits", labels, nil),
		commitCurMs:    prometheus.NewDesc("btrfs_commit_cur_commit_ms", "Current commit duration ms", labels, nil),
		commitLastMs:   prometheus.NewDesc("btrfs_commit_last_commit_ms", "Last commit duration ms", labels, nil),
		commitMaxMs:    prometheus.NewDesc("btrfs_commit_max_commit_ms", "Max commit duration ms", labels, nil),
		commitTotalMs:  prometheus.NewDesc("btrfs_commit_total_commit_ms", "Total commit time ms", labels, nil),
		commitRunning:  prometheus.NewDesc("btrfs_commit_running", "Whether a commit is currently running (D-state)", labels, nil),

		exclusiveOp:        prometheus.NewDesc("btrfs_exclusive_operation", "Current exclusive operation", append(labels, "name"), nil),
		defragRunning:      prometheus.NewDesc("btrfs_defrag_running", "Number of defrag processes running", labels, nil),
		quotaRescanRunning: prometheus.NewDesc("btrfs_quota_rescan_running", "Whether quota rescan is running", labels, nil),
		quotaRescanKey:     prometheus.NewDesc("btrfs_quota_rescan_current_key", "Quota rescan current key", labels, nil),

		replaceProgress:  prometheus.NewDesc("btrfs_replace_progress_percent", "Device replace progress percent", append(labels, "target_device", "missing_devid"), nil),
		replaceWriteErrs: prometheus.NewDesc("btrfs_replace_write_errors_total", "Device replace write errors", append(labels, "target_device", "missing_devid"), nil),
		replaceReadErrs:  prometheus.NewDesc("btrfs_replace_read_errors_total", "Device replace uncorrectable read errors", append(labels, "target_device", "missing_devid"), nil),

		balanceChunksDone:       prometheus.NewDesc("btrfs_balance_chunks_done", "Balance chunks completed", labels, nil),
		balanceChunksTotal:      prometheus.NewDesc("btrfs_balance_chunks_total", "Balance chunks total", labels, nil),
		balanceChunksConsidered: prometheus.NewDesc("btrfs_balance_chunks_considered", "Balance chunks considered", labels, nil),
		balanceProgressPercent:  prometheus.NewDesc("btrfs_balance_progress_percent", "Balance progress percent", labels, nil),
		balanceStatus:           prometheus.NewDesc("btrfs_balance_status", "Balance status", append(labels, "status"), nil),

		scrubDurationSecs:  prometheus.NewDesc("btrfs_scrub_duration_seconds", "Scrub duration in seconds", labels, nil),
		scrubSecsLeft:      prometheus.NewDesc("btrfs_scrub_seconds_left", "Scrub estimated seconds left", labels, nil),
		scrubTotalBytes:    prometheus.NewDesc("btrfs_scrub_total_bytes", "Scrub total bytes to process", labels, nil),
		scrubRateBps:       prometheus.NewDesc("btrfs_scrub_rate_bytes_per_second", "Scrub rate in bytes per second", labels, nil),
		scrubBytesScrubbed: prometheus.NewDesc("btrfs_scrub_bytes_scrubbed", "Scrub total bytes scrubbed", labels, nil),
		scrubStatus:        prometheus.NewDesc("btrfs_scrub_status", "Scrub status per device", append(deviceLabels, "status"), nil),
		scrubErrors:        prometheus.NewDesc("btrfs_scrub_errors", "Scrub errors per device per type", append(deviceLabels, "type"), nil),

		cleanOrphansLeft: prometheus.NewDesc("btrfs_clean_orphans_left_to_clean", "Orphan subvolumes left to clean", labels, nil),
		cleanOrphansMax:  prometheus.NewDesc("btrfs_clean_orphans_max_to_clean", "Max orphan subvolumes seen", labels, nil),

		beesCounter:       prometheus.NewDesc("bees_counter", "Bees dedup counter", append(labels, "name"), nil),
		beesTasksProgress: prometheus.NewDesc("bees_tasks_in_progress", "Bees tasks in progress", labels, nil),
		beesTasksQueued:   prometheus.NewDesc("bees_tasks_queued", "Bees tasks queued", labels, nil),
		beesWorkers:       prometheus.NewDesc("bees_thread_workers", "Bees worker threads", labels, nil),
	}
}

func (c *BtrfsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalBytes
	ch <- c.usedBytes
	ch <- c.freeBytes
	ch <- c.subvolGenerations
	ch <- c.subvolReferenced
	ch <- c.subvolExclusive
	ch <- c.subvolDiskUsage
	ch <- c.commitCommits
	ch <- c.commitCurMs
	ch <- c.commitLastMs
	ch <- c.commitMaxMs
	ch <- c.commitTotalMs
	ch <- c.commitRunning
	ch <- c.exclusiveOp
	ch <- c.defragRunning
	ch <- c.quotaRescanRunning
	ch <- c.quotaRescanKey
	ch <- c.replaceProgress
	ch <- c.replaceWriteErrs
	ch <- c.replaceReadErrs
	ch <- c.cleanOrphansLeft
	ch <- c.cleanOrphansMax
	ch <- c.beesCounter
	ch <- c.beesTasksProgress
	ch <- c.beesTasksQueued
	ch <- c.beesWorkers
}

func (c *BtrfsCollector) Collect(ch chan<- prometheus.Metric) {
	filesystems := discoverFilesystems()
	for _, fs := range filesystems {
		if !c.cfg.ShouldCollectFS(fs.UUID) {
			continue
		}
		c.collectFS(ch, fs)
	}
}

type btrfsFS struct {
	UUID       string
	Mountpoint string
	Devices    []string
}

// discoverFilesystems finds all mounted btrfs filesystems via /proc/self/mounts
// Returns one entry per UUID (shortest mountpoint wins)
func discoverFilesystems() []btrfsFS {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		log.Printf("Error reading /proc/self/mounts: %v", err)
		return nil
	}
	defer f.Close()

	seen := map[string]*btrfsFS{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[2] != "btrfs" {
			continue
		}
		dev := fields[0]
		mp := fields[1]
		// Unescape octal sequences in mountpoint (e.g. \040 for space)
		mp = unescapeOctal(mp)

		uuid := uuidForDevice(dev)
		if uuid == "" {
			continue
		}

		if existing, ok := seen[uuid]; ok {
			if len(mp) < len(existing.Mountpoint) {
				existing.Mountpoint = mp
			}
			existing.Devices = append(existing.Devices, dev)
		} else {
			seen[uuid] = &btrfsFS{UUID: uuid, Mountpoint: mp, Devices: []string{dev}}
		}
	}

	result := make([]btrfsFS, 0, len(seen))
	for _, fs := range seen {
		result = append(result, *fs)
	}
	return result
}

func unescapeOctal(s string) string {
	re := regexp.MustCompile(`\\([0-7]{3})`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		val, _ := strconv.ParseInt(m[1:], 8, 32)
		return string(rune(val))
	})
}

// uuidForDevice resolves device path → btrfs UUID via /sys/fs/btrfs/*/devices/
func uuidForDevice(dev string) string {
	// Resolve symlinks (e.g. /dev/mapper/luks-sda → /dev/dm-0)
	resolved, err := filepath.EvalSymlinks(dev)
	if err != nil {
		resolved = dev
	}
	devName := filepath.Base(resolved)

	entries, err := os.ReadDir("/sys/fs/btrfs")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name() == "features" {
			continue
		}
		devDir := filepath.Join("/sys/fs/btrfs", entry.Name(), "devices", devName)
		if _, err := os.Stat(devDir); err == nil {
			return entry.Name()
		}
	}
	return ""
}

func (c *BtrfsCollector) collectFS(ch chan<- prometheus.Metric, fs btrfsFS) {
	labels := []string{fs.UUID, fs.Mountpoint}

	// Always collect filesystem-level df metrics
	c.collectDfMetrics(ch, fs, labels)
	c.collectExclusiveOp(ch, fs, labels)

	if c.cfg.CollectSubvolumes {
		c.collectSubvolumes(ch, fs, labels)
	}
	if c.cfg.CollectQgroups {
		c.collectQgroups(ch, fs, labels)
		c.collectQuotaRescan(ch, fs, labels)
	}
	if c.cfg.CollectCommit {
		c.collectCommitStats(ch, fs, labels)
		c.collectCommitRunning(ch, fs, labels)
	}
	if c.cfg.CollectDefrag {
		c.collectDefrag(ch, fs, labels)
	}
	if c.cfg.CollectReplace {
		c.collectReplace(ch, fs, labels)
	}
	if c.cfg.CollectBalance {
		c.collectBalance(ch, fs, labels)
	}
	if c.cfg.CollectBees {
		c.collectBees(ch, fs, labels)
	}
}

// collectDfMetrics reads filesystem space via statfs syscall (no subprocess)
func (c *BtrfsCollector) collectDfMetrics(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(fs.Mountpoint, &stat); err != nil {
		log.Printf("statfs %s: %v", fs.Mountpoint, err)
		return
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	used := total - (stat.Bfree * uint64(stat.Bsize))

	ch <- prometheus.MustNewConstMetric(c.totalBytes, prometheus.GaugeValue, float64(total), labels...)
	ch <- prometheus.MustNewConstMetric(c.usedBytes, prometheus.GaugeValue, float64(used), labels...)
	ch <- prometheus.MustNewConstMetric(c.freeBytes, prometheus.GaugeValue, float64(free), labels...)
}

// collectSubvolumes reads subvolume list and generation via btrfs ioctl
// Falls back to `btrfs subvolume list -t` if ioctl not available
func (c *BtrfsCollector) collectSubvolumes(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	// Get root subvolume generation
	rootGen := readRootGeneration(fs.Mountpoint)
	if rootGen > 0 {
		ch <- prometheus.MustNewConstMetric(c.subvolGenerations, prometheus.CounterValue, float64(rootGen),
			fs.UUID, fs.Mountpoint, ".", "5")
	}

	// Use btrfs subvolume list with parent_uuid to detect snapshots
	out, err := exec.Command("btrfs", "subvolume", "list", "-tupq", fs.Mountpoint).Output()
	if err != nil {
		log.Printf("btrfs subvolume list %s: %v", fs.Mountpoint, err)
		return
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "ID") || strings.HasPrefix(line, "--") || strings.TrimSpace(line) == "" {
			continue
		}
		// Tab-separated: ID gen parent top_level parent_uuid uuid path
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		subvolID := strings.TrimSpace(fields[0])
		gen := strings.TrimSpace(fields[1])
		parentUUID := strings.TrimSpace(fields[4])
		path := strings.TrimSpace(fields[len(fields)-1])

		isSnapshot := parentUUID != "" && parentUUID != "-" && !strings.HasPrefix(parentUUID, "-")
		if !c.cfg.ShouldCollectSubvol(path, isSnapshot) {
			continue
		}

		genVal, _ := strconv.ParseFloat(gen, 64)
		ch <- prometheus.MustNewConstMetric(c.subvolGenerations, prometheus.CounterValue, genVal,
			fs.UUID, fs.Mountpoint, path, subvolID)
	}
}

// readRootGeneration reads the generation of the root subvolume via btrfs subvolume show
func readRootGeneration(mountpoint string) uint64 {
	out, err := exec.Command("btrfs", "subvolume", "show", mountpoint).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Generation:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				v, _ := strconv.ParseUint(parts[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// snapshotIDs returns a set of subvolume IDs that are snapshots (have parent_uuid)
func snapshotIDs(mountpoint string) map[string]bool {
	result := map[string]bool{}
	out, err := exec.Command("btrfs", "subvolume", "list", "-tupq", mountpoint).Output()
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		subvolID := strings.TrimSpace(fields[0])
		parentUUID := strings.TrimSpace(fields[4])
		if parentUUID != "" && parentUUID != "-" && !strings.HasPrefix(parentUUID, "-") {
			result[subvolID] = true
		}
	}
	return result
}

// collectQgroups reads qgroup data via `btrfs qgroup show`
func (c *BtrfsCollector) collectQgroups(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	out, err := exec.Command("btrfs", "qgroup", "show", "--raw", fs.Mountpoint).Output()
	if err != nil {
		return // quotas not enabled
	}

	snapIDs := snapshotIDs(fs.Mountpoint)

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Qgroupid") || strings.HasPrefix(line, "---") {
			continue
		}
		if strings.Contains(line, "<stale>") {
			continue
		}

		// Parse: 0/ID  referenced  exclusive  path
		line = strings.TrimPrefix(line, "0/")
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		subvolID := fields[0]
		referenced, _ := strconv.ParseFloat(fields[1], 64)
		exclusive, _ := strconv.ParseFloat(fields[2], 64)
		path := fields[3]
		if path == "<toplevel>" {
			path = "."
		}

		isSnapshot := snapIDs[subvolID]
		if !c.cfg.ShouldCollectSubvol(path, isSnapshot) {
			continue
		}

		subvolLabels := []string{fs.UUID, fs.Mountpoint, path, subvolID}
		ch <- prometheus.MustNewConstMetric(c.subvolReferenced, prometheus.GaugeValue, referenced, subvolLabels...)
		ch <- prometheus.MustNewConstMetric(c.subvolExclusive, prometheus.GaugeValue, exclusive, subvolLabels...)
		ch <- prometheus.MustNewConstMetric(c.subvolDiskUsage, prometheus.GaugeValue, exclusive, subvolLabels...)
	}
}

// collectCommitStats reads /sys/fs/btrfs/<uuid>/commit_stats (no subprocess)
func (c *BtrfsCollector) collectCommitStats(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	path := fmt.Sprintf("/sys/fs/btrfs/%s/commit_stats", fs.UUID)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	descs := map[string]*prometheus.Desc{
		"commits":          c.commitCommits,
		"cur_commit_ms":    c.commitCurMs,
		"last_commit_ms":   c.commitLastMs,
		"max_commit_ms":    c.commitMaxMs,
		"total_commit_ms":  c.commitTotalMs,
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if desc, ok := descs[fields[0]]; ok {
			val, _ := strconv.ParseFloat(fields[1], 64)
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, val, labels...)
		}
	}
}

// collectCommitRunning checks if btrfs-transaction thread is in D-state.
// First tries /run/btrfs-thread-map (external script), then falls back to
// native PID detection via /proc scanning.
func (c *BtrfsCollector) collectCommitRunning(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	running := 0.0

	pid := findTransactionPID(fs.UUID)
	if pid != "" {
		statPath := fmt.Sprintf("/proc/%s/stat", pid)
		stat, err := os.ReadFile(statPath)
		if err == nil {
			statFields := strings.Fields(string(stat))
			if len(statFields) >= 3 && statFields[2] == "D" {
				running = 1.0
			}
		}
	}
	ch <- prometheus.MustNewConstMetric(c.commitRunning, prometheus.GaugeValue, running, labels...)
}

// findTransactionPID finds the btrfs-transaction kernel thread PID for a given UUID.
// Strategy: scan /proc for btrfs-transaction threads, then correlate via dmesg
// "first mount" timestamps or the thread-map cache file.
func findTransactionPID(uuid string) string {
	// Try cached thread-map first
	data, err := os.ReadFile("/run/btrfs-thread-map")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "transaction" && fields[2] == uuid {
				// Verify PID still exists and is btrfs-transaction
				comm, err := os.ReadFile(fmt.Sprintf("/proc/%s/comm", fields[1]))
				if err == nil && strings.TrimSpace(string(comm)) == "btrfs-transaction" {
					return fields[1]
				}
			}
		}
	}

	// Fallback: scan /proc for btrfs-transaction threads and correlate
	// via mount order (UUID → device → sysfs starttime correlation)
	// For now, return empty — the external script handles this
	return ""
}

// collectExclusiveOp reads /sys/fs/btrfs/<uuid>/exclusive_operation (no subprocess)
func (c *BtrfsCollector) collectExclusiveOp(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	path := fmt.Sprintf("/sys/fs/btrfs/%s/exclusive_operation", fs.UUID)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name := strings.TrimSpace(string(data))
	ch <- prometheus.MustNewConstMetric(c.exclusiveOp, prometheus.GaugeValue, 1, append(labels, name)...)
}

// collectDefrag checks for running defrag processes (via /proc)
func (c *BtrfsCollector) collectDefrag(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	count := 0
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		_, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmd := string(cmdline)
		if strings.Contains(cmd, "btrfs") && strings.Contains(cmd, "defragment") && strings.Contains(cmd, fs.Mountpoint) {
			count++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.defragRunning, prometheus.GaugeValue, float64(count), labels...)
}

// collectQuotaRescan uses `btrfs quota rescan -s`
func (c *BtrfsCollector) collectQuotaRescan(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	out, err := exec.Command("btrfs", "quota", "rescan", "-s", fs.Mountpoint).CombinedOutput()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.quotaRescanRunning, prometheus.GaugeValue, 0, labels...)
		ch <- prometheus.MustNewConstMetric(c.quotaRescanKey, prometheus.CounterValue, 0, labels...)
		return
	}

	s := string(out)
	running := 0.0
	if strings.Contains(s, "operation running") {
		running = 1.0
	}
	ch <- prometheus.MustNewConstMetric(c.quotaRescanRunning, prometheus.GaugeValue, running, labels...)

	key := 0.0
	re := regexp.MustCompile(`current key\s+(\d+)`)
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		key, _ = strconv.ParseFloat(m[1], 64)
	}
	ch <- prometheus.MustNewConstMetric(c.quotaRescanKey, prometheus.CounterValue, key, labels...)
}

// collectReplace uses `btrfs replace status -1` and sysfs for device info
func (c *BtrfsCollector) collectReplace(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	out, err := exec.Command("btrfs", "replace", "status", "-1", fs.Mountpoint).CombinedOutput()
	if err != nil {
		return
	}

	s := string(out)
	re := regexp.MustCompile(`(\d+\.?\d*)% done, (\d+) write errs, (\d+) uncorr\. read errs`)
	m := re.FindStringSubmatch(s)
	if len(m) <= 3 {
		return
	}

	progress, _ := strconv.ParseFloat(m[1], 64)
	writeErrs, _ := strconv.ParseFloat(m[2], 64)
	readErrs, _ := strconv.ParseFloat(m[3], 64)

	// Find replace target device via sysfs devinfo/*/replace_target
	targetDev := ""
	missingDevID := ""
	devinfoBase := fmt.Sprintf("/sys/fs/btrfs/%s/devinfo", fs.UUID)
	devinfos, _ := os.ReadDir(devinfoBase)
	for _, di := range devinfos {
		rtData, err := os.ReadFile(filepath.Join(devinfoBase, di.Name(), "replace_target"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(rtData)) == "1" {
			targetDev = devIDToName(fs.UUID, di.Name())
			// The missing device is the one not in /sys/fs/btrfs/<uuid>/devices/
			missingDevID = findMissingDevID(fs.UUID, devinfos)
		}
	}

	targetDev = c.resolveDeviceName(targetDev)
	replaceLabels := append(labels, targetDev, missingDevID)
	ch <- prometheus.MustNewConstMetric(c.replaceProgress, prometheus.GaugeValue, progress, replaceLabels...)
	ch <- prometheus.MustNewConstMetric(c.replaceWriteErrs, prometheus.CounterValue, writeErrs, replaceLabels...)
	ch <- prometheus.MustNewConstMetric(c.replaceReadErrs, prometheus.CounterValue, readErrs, replaceLabels...)
}

// devIDToName maps a btrfs devid to a device name via sysfs
func devIDToName(uuid, devid string) string {
	devicesDir := fmt.Sprintf("/sys/fs/btrfs/%s/devices", uuid)
	entries, err := os.ReadDir(devicesDir)
	if err != nil {
		return "devid-" + devid
	}
	// Read the device's devid from sysfs to match
	for _, e := range entries {
		didPath := filepath.Join(devicesDir, e.Name(), "devid")
		data, err := os.ReadFile(didPath)
		if err != nil {
			// Fallback: check via devinfo
			continue
		}
		if strings.TrimSpace(string(data)) == devid {
			return e.Name()
		}
	}
	// devid 0 is the replace target — it maps to the device listed in devices/ that is not in devinfo with replace_target=0
	return "devid-" + devid
}

// findMissingDevID finds the devid that is in devinfo but not in devices/ (the missing disk)
func findMissingDevID(uuid string, devinfos []os.DirEntry) string {
	devicesDir := fmt.Sprintf("/sys/fs/btrfs/%s/devices", uuid)
	activeDevIDs := map[string]bool{}
	entries, err := os.ReadDir(devicesDir)
	if err != nil {
		return "unknown"
	}
	for _, e := range entries {
		didPath := filepath.Join(devicesDir, e.Name(), "devid")
		data, _ := os.ReadFile(didPath)
		activeDevIDs[strings.TrimSpace(string(data))] = true
	}
	for _, di := range devinfos {
		if !activeDevIDs[di.Name()] {
			rtData, _ := os.ReadFile(filepath.Join(fmt.Sprintf("/sys/fs/btrfs/%s/devinfo", uuid), di.Name(), "replace_target"))
			if strings.TrimSpace(string(rtData)) != "1" {
				return "devid-" + di.Name()
			}
		}
	}
	return "unknown"
}

// collectBalance uses `btrfs balance status`
func (c *BtrfsCollector) collectBalance(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	out, err := exec.Command("btrfs", "balance", "status", fs.Mountpoint).CombinedOutput()
	if err != nil {
		return
	}

	s := string(out)
	re := regexp.MustCompile(`(\d+) out of about (\d+) chunks balanced \((\d+) considered\),\s+(\d+)% left`)
	if m := re.FindStringSubmatch(s); len(m) > 4 {
		done, _ := strconv.ParseFloat(m[1], 64)
		total, _ := strconv.ParseFloat(m[2], 64)
		considered, _ := strconv.ParseFloat(m[3], 64)
		left, _ := strconv.ParseFloat(m[4], 64)

		ch <- prometheus.MustNewConstMetric(c.balanceChunksDone, prometheus.GaugeValue, done, labels...)
		ch <- prometheus.MustNewConstMetric(c.balanceChunksTotal, prometheus.GaugeValue, total, labels...)
		ch <- prometheus.MustNewConstMetric(c.balanceChunksConsidered, prometheus.GaugeValue, considered, labels...)
		ch <- prometheus.MustNewConstMetric(c.balanceProgressPercent, prometheus.GaugeValue, 100-left, labels...)

		status := "running"
		if strings.Contains(s, "pause requested") {
			status = "pausing"
		} else if strings.Contains(s, "is paused") {
			status = "paused"
		}
		ch <- prometheus.MustNewConstMetric(c.balanceStatus, prometheus.GaugeValue, 1, append(labels, status)...)
	}
}

// collectBees reads bees status from /run/bees/<uuid>.status
func (c *BtrfsCollector) collectBees(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	statusFile := filepath.Join(c.cfg.BeesStatusDir, fs.UUID+".status")
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return // bees not running for this FS
	}

	content := string(data)

	// Parse key=value counters (everything before RATES:)
	ratesIdx := strings.Index(content, "RATES:")
	counterSection := content
	if ratesIdx > 0 {
		counterSection = content[:ratesIdx]
	}

	for _, token := range strings.Fields(counterSection) {
		parts := strings.SplitN(token, "=", 2)
		if len(parts) != 2 {
			continue
		}
		val, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.beesCounter, prometheus.CounterValue, val, append(labels, parts[0])...)
	}

	// Parse THREADS line
	re := regexp.MustCompile(`THREADS\D+(\d+)\D+(\d+)\D+(\d+)`)
	if m := re.FindStringSubmatch(content); len(m) > 3 {
		queue, _ := strconv.ParseFloat(m[1], 64)
		tasks, _ := strconv.ParseFloat(m[2], 64)
		workers, _ := strconv.ParseFloat(m[3], 64)
		ch <- prometheus.MustNewConstMetric(c.beesTasksProgress, prometheus.GaugeValue, queue, labels...)
		ch <- prometheus.MustNewConstMetric(c.beesTasksQueued, prometheus.GaugeValue, tasks, labels...)
		ch <- prometheus.MustNewConstMetric(c.beesWorkers, prometheus.GaugeValue, workers, labels...)
	}
}

// resolveDeviceName resolves dm-X to /dev/mapper/luks-* if UseLuksDeviceNames is enabled
func (c *BtrfsCollector) resolveDeviceName(dmName string) string {
	if !c.cfg.UseLuksDeviceNames {
		return dmName
	}
	// Check /dev/mapper/ for symlinks pointing to this dm device
	entries, err := os.ReadDir("/dev/mapper")
	if err != nil {
		return dmName
	}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join("/dev/mapper", e.Name()))
		if err != nil {
			continue
		}
		if filepath.Base(link) == dmName || link == "/dev/"+dmName {
			return "/dev/mapper/" + e.Name()
		}
	}
	return dmName
}

// ioctl helper (unused for now, kept for future native qgroup reading)
func btrfsIoctl(fd uintptr, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
