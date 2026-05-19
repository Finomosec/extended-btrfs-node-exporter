package collector

import (
	"bufio"
	"fmt"
	"log"
	"os"
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

		replaceProgress:  prometheus.NewDesc("btrfs_replace_progress_percent", "Device replace progress percent", append(labels, "old_device", "new_device"), nil),
		replaceWriteErrs: prometheus.NewDesc("btrfs_replace_write_errors_total", "Device replace write errors", append(labels, "old_device", "new_device"), nil),
		replaceReadErrs:  prometheus.NewDesc("btrfs_replace_read_errors_total", "Device replace uncorrectable read errors", append(labels, "old_device", "new_device"), nil),

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

	// All collectors use timeouts to prevent blocking on kernel locks
	c.collectDfMetrics(ch, fs, labels)
	c.collectExclusiveOp(ch, fs, labels)

	if c.cfg.CollectCommit {
		c.collectCommitStats(ch, fs, labels)
		c.collectCommitRunning(ch, fs, labels)
	}
	// Fetch subvolume list once, share between collectors
	var subvols []SubvolInfo
	if c.cfg.CollectSubvolumes || c.cfg.CollectQgroups {
		var err error
		subvols, err = ListSubvolumes(fs.Mountpoint, c.cfg.IoctlTimeout)
		if err != nil {
			log.Printf("[%s] ListSubvolumes: %v", fs.Mountpoint, err)
		}
	}
	if c.cfg.CollectSubvolumes && subvols != nil {
		c.emitSubvolGenerations(ch, fs, subvols)
	}
	if c.cfg.CollectQgroups {
		c.collectQgroups(ch, fs, labels, subvols)
		c.collectQuotaRescan(ch, fs, labels)
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
	if c.cfg.CollectOrphans {
		c.collectOrphans(ch, fs, labels)
	}
}

// readExclusiveOp reads the current exclusive operation from sysfs (never blocks)
func readExclusiveOp(uuid string) string {
	data, err := os.ReadFile(fmt.Sprintf("/sys/fs/btrfs/%s/exclusive_operation", uuid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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

// emitSubvolGenerations emits generation metrics for subvolumes
func (c *BtrfsCollector) emitSubvolGenerations(ch chan<- prometheus.Metric, fs btrfsFS, subvols []SubvolInfo) {
	for _, sv := range subvols {
		isSnapshot := !IsNullUUID(sv.ParentUUID)
		if !c.cfg.ShouldCollectSubvol(sv.Path, isSnapshot) {
			continue
		}
		subvolID := fmt.Sprintf("%d", sv.ID)
		ch <- prometheus.MustNewConstMetric(c.subvolGenerations, prometheus.CounterValue, float64(sv.Generation),
			fs.UUID, fs.Mountpoint, sv.Path, subvolID)
	}
}

// collectQgroups reads qgroup data via BTRFS_IOC_TREE_SEARCH on quota tree (no subprocess)
// Uses pre-fetched subvols list to filter and annotate qgroups
func (c *BtrfsCollector) collectQgroups(ch chan<- prometheus.Metric, fs btrfsFS, labels []string, subvols []SubvolInfo) {
	qgroups, err := ListQgroups(fs.Mountpoint, c.cfg.IoctlTimeout)
	if err != nil {
		log.Printf("[%s] collectQgroups: %v", fs.Mountpoint, err)
		return
	}

	// Build lookup maps from pre-fetched subvolume list
	snapSet := map[uint64]bool{}
	pathMap := map[uint64]string{5: "."}
	for _, sv := range subvols {
		pathMap[sv.ID] = sv.Path
		if !IsNullUUID(sv.ParentUUID) {
			snapSet[sv.ID] = true
		}
	}

	emitted := 0
	for _, qg := range qgroups {
		path := pathMap[qg.SubvolID]
		if path == "" {
			continue // stale qgroup — subvolume deleted
		}

		isSnapshot := snapSet[qg.SubvolID]
		if !c.cfg.ShouldCollectSubvol(path, isSnapshot) {
			continue
		}

		subvolID := fmt.Sprintf("%d", qg.SubvolID)
		subvolLabels := []string{fs.UUID, fs.Mountpoint, path, subvolID}
		ch <- prometheus.MustNewConstMetric(c.subvolReferenced, prometheus.GaugeValue, float64(qg.Referenced), subvolLabels...)
		ch <- prometheus.MustNewConstMetric(c.subvolExclusive, prometheus.GaugeValue, float64(qg.Exclusive), subvolLabels...)
		ch <- prometheus.MustNewConstMetric(c.subvolDiskUsage, prometheus.GaugeValue, float64(qg.Exclusive), subvolLabels...)
		emitted++
	}
	log.Printf("[%s] collectQgroups: %d qgroups read, %d emitted", fs.Mountpoint, len(qgroups), emitted)
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
// Uses native /proc scanning to map transaction PIDs to filesystems.
func (c *BtrfsCollector) collectCommitRunning(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	running := 0.0

	pid := c.findTransactionPID(fs.UUID)
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

// threadMap caches PID→UUID mapping, rebuilt when PIDs change
var threadMap map[string]string // uuid → pid
var threadMapPIDs string        // cached sorted PID list for staleness check

// findTransactionPID finds the btrfs-transaction kernel thread PID for a given UUID.
// Scans /proc for btrfs-transaction threads, reads their starttimes, correlates
// with btrfs mount times from dmesg or mount order.
func (c *BtrfsCollector) findTransactionPID(uuid string) string {
	// Get current transaction PIDs
	currentPIDs := findBtrfsTransactionPIDs()
	pidsKey := strings.Join(currentPIDs, ",")

	// Rebuild map if PIDs changed
	if pidsKey != threadMapPIDs {
		threadMap = buildThreadMap(currentPIDs)
		threadMapPIDs = pidsKey
	}

	return threadMap[uuid]
}

// findBtrfsTransactionPIDs scans /proc for btrfs-transaction kernel threads
func findBtrfsTransactionPIDs() []string {
	var pids []string
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return pids
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == "btrfs-transaction" {
			pids = append(pids, e.Name())
		}
	}
	return pids
}

// pidStartTime reads the start time (in clock ticks) of a process from /proc/<pid>/stat
func pidStartTime(pid string) float64 {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return 0
	}
	// Field 22 (0-indexed: 21) is starttime, but we need to skip the comm field
	// which can contain spaces/parens. Find closing paren first.
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(s[idx+2:]) // skip ") " then split
	if len(fields) < 20 {
		return 0
	}
	// starttime is field 20 after the closing paren (0-indexed)
	ticks, _ := strconv.ParseFloat(fields[19], 64)
	clkTck := float64(100) // sysconf(_SC_CLK_TCK), almost always 100 on Linux
	return ticks / clkTck
}

// mountTimeFromDmesg extracts "first mount" timestamps from kernel log for each UUID
func mountTimeFromDmesg() map[string]float64 {
	result := map[string]float64{}
	// Read kernel log from syslog files (no subprocess)
	var data []byte
	for _, path := range []string{"/var/log/kern.log", "/var/log/syslog", "/var/log/dmesg"} {
		var err error
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if data == nil {
		return result
	}

	uuidRe := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	tsRe := regexp.MustCompile(`^\[\s*([0-9.]+)\]`)

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "first mount") {
			continue
		}
		tsMatch := tsRe.FindStringSubmatch(line)
		uuidMatch := uuidRe.FindString(line)
		if len(tsMatch) > 1 && uuidMatch != "" {
			ts, _ := strconv.ParseFloat(tsMatch[1], 64)
			if _, exists := result[uuidMatch]; !exists {
				result[uuidMatch] = ts
			}
		}
	}
	return result
}

// buildThreadMap correlates btrfs-transaction PIDs with filesystem UUIDs
func buildThreadMap(pids []string) map[string]string {
	result := map[string]string{} // uuid → pid

	if len(pids) == 0 {
		return result
	}

	// Collect all mounted btrfs UUIDs
	filesystems := discoverFilesystems()
	uuids := make([]string, 0, len(filesystems))
	for _, fs := range filesystems {
		uuids = append(uuids, fs.UUID)
	}

	// Easy case: same count, try correlating
	if len(pids) == 1 && len(uuids) == 1 {
		result[uuids[0]] = pids[0]
		return result
	}

	// Strategy 1: dmesg timestamp correlation
	mountTimes := mountTimeFromDmesg()
	pidStarts := map[string]float64{}
	for _, pid := range pids {
		pidStarts[pid] = pidStartTime(pid)
	}

	mapped := map[string]bool{} // pid → mapped
	for _, uuid := range uuids {
		mt, ok := mountTimes[uuid]
		if !ok {
			continue
		}
		bestPID := ""
		bestDiff := 999999.0
		for _, pid := range pids {
			if mapped[pid] {
				continue
			}
			diff := pidStarts[pid] - mt
			if diff < 0 {
				diff = -diff
			}
			if diff < bestDiff {
				bestDiff = diff
				bestPID = pid
			}
		}
		if bestPID != "" && bestDiff < 5.0 { // within 5 seconds
			result[uuid] = bestPID
			mapped[bestPID] = true
		}
	}

	// Strategy 2: root heuristic (oldest PID → root filesystem)
	if len(result) < len(uuids) {
		var rootUUID string
		for _, fs := range filesystems {
			if fs.Mountpoint == "/" {
				rootUUID = fs.UUID
				break
			}
		}
		if rootUUID != "" && result[rootUUID] == "" {
			oldestPID := ""
			oldestStart := 999999999.0
			for _, pid := range pids {
				if mapped[pid] {
					continue
				}
				if pidStarts[pid] < oldestStart {
					oldestStart = pidStarts[pid]
					oldestPID = pid
				}
			}
			if oldestPID != "" {
				result[rootUUID] = oldestPID
				mapped[oldestPID] = true
			}
		}
	}

	// Strategy 3: single unmapped PID + single unmapped UUID
	unmappedPIDs := []string{}
	unmappedUUIDs := []string{}
	for _, pid := range pids {
		if !mapped[pid] {
			unmappedPIDs = append(unmappedPIDs, pid)
		}
	}
	for _, uuid := range uuids {
		if result[uuid] == "" {
			unmappedUUIDs = append(unmappedUUIDs, uuid)
		}
	}
	if len(unmappedPIDs) == 1 && len(unmappedUUIDs) == 1 {
		result[unmappedUUIDs[0]] = unmappedPIDs[0]
		mapped[unmappedPIDs[0]] = true
	}

	// Strategy 4: mount-order correlation for remaining
	if len(unmappedPIDs) > 1 && len(unmappedPIDs) == len(unmappedUUIDs) {
		// Sort PIDs by starttime
		sortedPIDs := make([]string, len(unmappedPIDs))
		copy(sortedPIDs, unmappedPIDs)
		for i := 0; i < len(sortedPIDs); i++ {
			for j := i + 1; j < len(sortedPIDs); j++ {
				if pidStarts[sortedPIDs[i]] > pidStarts[sortedPIDs[j]] {
					sortedPIDs[i], sortedPIDs[j] = sortedPIDs[j], sortedPIDs[i]
				}
			}
		}
		// UUIDs are already in mount order from discoverFilesystems
		for i, uuid := range unmappedUUIDs {
			if i < len(sortedPIDs) {
				result[uuid] = sortedPIDs[i]
			}
		}
	}

	return result
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

// collectQuotaRescan uses BTRFS_IOC_QUOTA_RESCAN_STATUS (no subprocess)
func (c *BtrfsCollector) collectQuotaRescan(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	status, err := GetQuotaRescanStatus(fs.Mountpoint, c.cfg.IoctlTimeout)
	if err != nil || status == nil {
		ch <- prometheus.MustNewConstMetric(c.quotaRescanRunning, prometheus.GaugeValue, 0, labels...)
		ch <- prometheus.MustNewConstMetric(c.quotaRescanKey, prometheus.CounterValue, 0, labels...)
		return
	}
	running := 0.0
	if status.Running {
		running = 1.0
	}
	ch <- prometheus.MustNewConstMetric(c.quotaRescanRunning, prometheus.GaugeValue, running, labels...)
	ch <- prometheus.MustNewConstMetric(c.quotaRescanKey, prometheus.CounterValue, float64(status.Progress), labels...)
}

// collectReplace uses BTRFS_IOC_DEV_REPLACE for status (no subprocess)
func (c *BtrfsCollector) collectReplace(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	status, err := GetReplaceStatus(fs.Mountpoint, c.cfg.IoctlTimeout)
	if err != nil || status == nil || !status.Running {
		return
	}

	progress := status.Progress
	writeErrs := float64(status.WriteErrs)
	readErrs := float64(status.ReadErrs)

	// Resolve device names from sysfs devinfo
	newDev := ""
	oldDev := "MISSING"
	devinfoBase := fmt.Sprintf("/sys/fs/btrfs/%s/devinfo", fs.UUID)
	devinfos, _ := os.ReadDir(devinfoBase)
	devMap := DevIDMap(fs.Mountpoint, fs.UUID)
	log.Printf("[%s] DevIDMap: %v", fs.Mountpoint, devMap)
	for _, di := range devinfos {
		rtData, _ := os.ReadFile(filepath.Join(devinfoBase, di.Name(), "replace_target"))
		if strings.TrimSpace(string(rtData)) == "1" {
			if name, ok := devMap[di.Name()]; ok && name != "missing" {
				newDev = c.resolveDeviceName(name)
			} else {
				newDev = "dev-" + di.Name()
			}
		}
		missingData, _ := os.ReadFile(filepath.Join(devinfoBase, di.Name(), "missing"))
		if strings.TrimSpace(string(missingData)) == "1" {
			oldDev = "dev-" + di.Name()
		}
	}
	if newDev == "" {
		newDev = "unknown"
	}

	replaceLabels := append(labels, oldDev, newDev) // old_device, new_device
	ch <- prometheus.MustNewConstMetric(c.replaceProgress, prometheus.GaugeValue, progress, replaceLabels...)
	ch <- prometheus.MustNewConstMetric(c.replaceWriteErrs, prometheus.CounterValue, writeErrs, replaceLabels...)
	ch <- prometheus.MustNewConstMetric(c.replaceReadErrs, prometheus.CounterValue, readErrs, replaceLabels...)
}


// collectBalance uses BTRFS_IOC_BALANCE_PROGRESS (no subprocess)
func (c *BtrfsCollector) collectBalance(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	status, err := GetBalanceStatus(fs.Mountpoint, c.cfg.IoctlTimeout)
	if err != nil || status == nil || !status.Running {
		return
	}

	total := float64(status.Expected)
	done := float64(status.Completed)
	considered := float64(status.Considered)
	progress := 0.0
	if total > 0 {
		progress = done / total * 100
	}

	ch <- prometheus.MustNewConstMetric(c.balanceChunksDone, prometheus.GaugeValue, done, labels...)
	ch <- prometheus.MustNewConstMetric(c.balanceChunksTotal, prometheus.GaugeValue, total, labels...)
	ch <- prometheus.MustNewConstMetric(c.balanceChunksConsidered, prometheus.GaugeValue, considered, labels...)
	ch <- prometheus.MustNewConstMetric(c.balanceProgressPercent, prometheus.GaugeValue, progress, labels...)
	ch <- prometheus.MustNewConstMetric(c.balanceStatus, prometheus.GaugeValue, 1, append(labels, status.State)...)
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
		desc := prometheus.NewDesc("bees_"+parts[0], "Bees counter: "+parts[0], []string{"uuid", "mountpoint"}, nil)
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, val, labels...)
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

// collectOrphans counts orphan (deleted but not yet cleaned) subvolumes via `btrfs subvolume list -d`
// Tracks max value in a state file to show peak orphan count
func (c *BtrfsCollector) collectOrphans(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	count, err := CountOrphans(fs.Mountpoint, c.cfg.IoctlTimeout)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.cleanOrphansLeft, prometheus.GaugeValue, 0, labels...)
		ch <- prometheus.MustNewConstMetric(c.cleanOrphansMax, prometheus.GaugeValue, 0, labels...)
		return
	}

	// Track max via state file
	stateDir := "/tmp"
	statePath := filepath.Join(stateDir, fs.UUID+".max")
	storedMax := 0
	if data, err := os.ReadFile(statePath); err == nil {
		storedMax, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}

	newMax := storedMax
	if count == 0 {
		newMax = 0
	} else if count > storedMax {
		newMax = count
	}
	if newMax != storedMax {
		os.WriteFile(statePath, []byte(strconv.Itoa(newMax)), 0644)
	}

	ch <- prometheus.MustNewConstMetric(c.cleanOrphansLeft, prometheus.GaugeValue, float64(count), labels...)
	ch <- prometheus.MustNewConstMetric(c.cleanOrphansMax, prometheus.GaugeValue, float64(newMax), labels...)
}

// resolveDeviceName resolves dm-X to /dev/mapper/* if ResolveDeviceMapper is enabled
func (c *BtrfsCollector) resolveDeviceName(dmName string) string {
	if !c.cfg.ResolveDeviceMapper {
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
			return e.Name()
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
