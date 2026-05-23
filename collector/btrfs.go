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
	"sync"
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

	// Device
	deviceSizeBytes   *prometheus.Desc
	deviceUnusedBytes *prometheus.Desc
	deviceErrorsTotal *prometheus.Desc

	// Bees
	beesCounter       *prometheus.Desc
	beesTasksProgress *prometheus.Desc
	beesTasksQueued   *prometheus.Desc
	beesWorkers       *prometheus.Desc

	// Once-per-scraper tracking for last_commit_ms
	commitMu       sync.Mutex
	commitTracker  map[string]*commitState // UUID → state
	currentScraper string
}

type commitState struct {
	lastSeenCommits float64
	reportedTo      map[string]bool
}

func New(cfg Config) *BtrfsCollector {
	labels := []string{"uuid", "mountpoint"}
	subvolLabels := []string{"uuid", "mountpoint", "subvolume", "subvolume_id"}
	deviceLabels := []string{"uuid", "mountpoint", "device"}

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

		deviceSizeBytes:   prometheus.NewDesc("btrfs_device_size_bytes", "Size of a device in the filesystem", append(deviceLabels, "btrfs_dev_uuid"), nil),
		deviceUnusedBytes: prometheus.NewDesc("btrfs_device_unused_bytes", "Unused bytes on a device in the filesystem", append(deviceLabels, "btrfs_dev_uuid"), nil),
		deviceErrorsTotal: prometheus.NewDesc("btrfs_device_errors_total", "Device errors by type", append(deviceLabels, "btrfs_dev_uuid", "type"), nil),

		commitTracker: map[string]*commitState{},
	}
}

func (c *BtrfsCollector) debugf(format string, args ...interface{}) {
	if c.cfg.Debug {
		log.Printf(format, args...)
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
	ch <- c.deviceSizeBytes
	ch <- c.deviceUnusedBytes
	ch <- c.deviceErrorsTotal
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
	fd         int // shared file descriptor for ioctls
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

	// Open a single fd for all ioctls on this filesystem
	fd, err := syscall.Open(fs.Mountpoint, syscall.O_RDONLY, 0)
	if err != nil {
		log.Printf("[%s] open: %v", fs.Mountpoint, err)
		return
	}
	defer syscall.Close(fd)
	fs.fd = fd

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
		subvols, err = ListSubvolumes(fs.fd, fs.Mountpoint, c.cfg.IoctlTimeout)
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
	if c.cfg.CollectScrub {
		c.collectScrub(ch, fs, labels)
	}
	// Device metrics are always collected (like filesystem-level metrics)
	c.collectDevices(ch, fs, labels)
}

// collectDevices reads per-device size (from ioctl) and error stats (from sysfs)
func (c *BtrfsCollector) collectDevices(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	devMap := DevIDMap(fs.fd, fs.Mountpoint, fs.UUID)
	devinfoBase := fmt.Sprintf("/sys/fs/btrfs/%s/devinfo", fs.UUID)
	devinfos, err := os.ReadDir(devinfoBase)
	if err != nil {
		return
	}

	for _, di := range devinfos {
		devID := di.Name()
		deviceName := devMap[devID]
		if deviceName == "" {
			deviceName = "dev-" + devID
		}
		if c.cfg.ResolveDeviceMapper && deviceName != "missing" {
			deviceName = c.resolveDeviceName(deviceName)
		}

		// Device info from ioctl (size + UUID)
		var args devInfoArgs
		did, _ := strconv.ParseUint(devID, 10, 64)
		args.DevID = did
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fs.fd), iocDevInfo, uintptr(unsafe.Pointer(&args)))
		if errno != 0 {
			continue
		}

		// Format btrfs_dev_uuid from ioctl response (no byte-swapping, matches node_exporter)
		u := args.UUID
		devUUID := fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
			u[0], u[1], u[2], u[3], u[4], u[5], u[6], u[7],
			u[8], u[9], u[10], u[11], u[12], u[13], u[14], u[15])

		devLabels := append(labels, deviceName, devUUID)

		if args.TotalBytes > 0 {
			ch <- prometheus.MustNewConstMetric(c.deviceSizeBytes, prometheus.GaugeValue, float64(args.TotalBytes), devLabels...)
			unused := args.TotalBytes - args.BytesUsed
			ch <- prometheus.MustNewConstMetric(c.deviceUnusedBytes, prometheus.GaugeValue, float64(unused), devLabels...)
		}

		// Error stats from sysfs
		errData, err := os.ReadFile(filepath.Join(devinfoBase, devID, "error_stats"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(errData), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			val, _ := strconv.ParseFloat(fields[1], 64)
			errType := strings.TrimSuffix(fields[0], "_errs")
			ch <- prometheus.MustNewConstMetric(c.deviceErrorsTotal, prometheus.CounterValue, val, append(devLabels, errType)...)
		}
	}
}

// collectScrub reads scrub status from /var/lib/btrfs/scrub.status.<uuid> (no subprocess)
func (c *BtrfsCollector) collectScrub(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	statusFile := fmt.Sprintf("/var/lib/btrfs/scrub.status.%s", fs.UUID)
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return // no scrub status
	}

	// Parse pipe-separated records: uuid:diskid|key:val|key:val|...
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "scrub") {
			continue
		}

		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}

		// First field: uuid:diskid
		idParts := strings.SplitN(fields[0], ":", 2)
		if len(idParts) < 2 {
			continue
		}
		diskID := idParts[1]
		if diskID == "" {
			continue
		}

		// Parse key:value pairs
		vals := map[string]string{}
		for _, f := range fields[1:] {
			kv := strings.SplitN(f, ":", 2)
			if len(kv) == 2 {
				vals[kv[0]] = kv[1]
			}
		}

		// Determine status
		status := "idle"
		if vals["finished"] == "1" {
			status = "finished"
		} else if vals["canceled"] == "1" {
			status = "canceled"
		} else if vals["t_start"] != "" && vals["t_start"] != "0" && vals["finished"] != "1" && vals["canceled"] != "1" {
			status = "running"
		}

		deviceLabels := []string{fs.UUID, fs.Mountpoint, diskID}

		// Status per device
		for _, st := range []string{"running", "finished", "canceled", "idle"} {
			val := 0.0
			if st == status {
				val = 1.0
			}
			ch <- prometheus.MustNewConstMetric(c.scrubStatus, prometheus.GaugeValue, val,
				append(deviceLabels, st)...)
		}

		// Error counters per device
		errorTypes := []string{"read_errors", "csum_errors", "verify_errors", "super_errors",
			"uncorrectable_errors", "corrected_errors", "last_physical"}
		for _, et := range errorTypes {
			if v, ok := vals[et]; ok {
				val, _ := strconv.ParseFloat(v, 64)
				ch <- prometheus.MustNewConstMetric(c.scrubErrors, prometheus.CounterValue, val,
					append(deviceLabels, et)...)
			}
		}
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
	qgroups, err := ListQgroups(fs.fd, fs.Mountpoint, c.cfg.IoctlTimeout)
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
	c.debugf("[%s] collectQgroups: %d qgroups read, %d emitted", fs.Mountpoint, len(qgroups), emitted)
}

// SetCurrentScraper records the scraper IP for once-per-scraper tracking.
// Called from the HTTP handler before promhttp serves the request.
func (c *BtrfsCollector) SetCurrentScraper(remoteAddr string) {
	// Strip port — same Prometheus instance may connect from different source ports
	host := remoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx >= 0 {
		host = remoteAddr[:idx]
	}
	c.commitMu.Lock()
	c.currentScraper = host
	c.commitMu.Unlock()
}

// collectCommitStats reads /sys/fs/btrfs/<uuid>/commit_stats (no subprocess).
// last_commit_ms is only emitted once per scraper per new commit to avoid
// stale values appearing as current in dashboards.
func (c *BtrfsCollector) collectCommitStats(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	path := fmt.Sprintf("/sys/fs/btrfs/%s/commit_stats", fs.UUID)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Parse all values from sysfs (no lock needed)
	vals := map[string]float64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			val, _ := strconv.ParseFloat(fields[1], 64)
			vals[fields[0]] = val
		}
	}

	// Emit all metrics except last_commit_ms unconditionally
	always := map[string]*prometheus.Desc{
		"commits":         c.commitCommits,
		"cur_commit_ms":   c.commitCurMs,
		"max_commit_ms":   c.commitMaxMs,
		"total_commit_ms": c.commitTotalMs,
	}
	for key, desc := range always {
		if val, ok := vals[key]; ok {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, val, labels...)
		}
	}

	// last_commit_ms: emit only once per scraper per new commit
	lastMs, hasLast := vals["last_commit_ms"]
	commits := vals["commits"]
	if !hasLast {
		return
	}

	c.commitMu.Lock()
	scraper := c.currentScraper

	state, exists := c.commitTracker[fs.UUID]
	if !exists {
		state = &commitState{reportedTo: map[string]bool{}}
		c.commitTracker[fs.UUID] = state
	}

	if commits != state.lastSeenCommits {
		state.lastSeenCommits = commits
		state.reportedTo = map[string]bool{}
	}

	shouldEmit := !state.reportedTo[scraper]
	if shouldEmit {
		state.reportedTo[scraper] = true
	}
	c.commitMu.Unlock()

	if shouldEmit {
		ch <- prometheus.MustNewConstMetric(c.commitLastMs, prometheus.CounterValue, lastMs, labels...)
	}
}

// collectCommitRunning derives commit-in-progress from cur_commit_ms in sysfs.
// cur_commit_ms > 0 means a commit is currently running on this filesystem.
func (c *BtrfsCollector) collectCommitRunning(ch chan<- prometheus.Metric, fs btrfsFS, labels []string) {
	running := 0.0
	path := fmt.Sprintf("/sys/fs/btrfs/%s/commit_stats", fs.UUID)
	data, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "cur_commit_ms" {
				val, _ := strconv.ParseFloat(fields[1], 64)
				if val > 0 {
					running = 1.0
				}
				break
			}
		}
	}
	ch <- prometheus.MustNewConstMetric(c.commitRunning, prometheus.GaugeValue, running, labels...)
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
	status, err := GetQuotaRescanStatus(fs.fd, fs.Mountpoint, c.cfg.IoctlTimeout)
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
	status, err := GetReplaceStatus(fs.fd, fs.Mountpoint, c.cfg.IoctlTimeout)
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
	devMap := DevIDMap(fs.fd, fs.Mountpoint, fs.UUID)
	c.debugf("[%s] DevIDMap: %v", fs.Mountpoint, devMap)
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
	status, err := GetBalanceStatus(fs.fd, fs.Mountpoint, c.cfg.IoctlTimeout)
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
	count, err := CountOrphans(fs.fd, fs.Mountpoint, c.cfg.IoctlTimeout)
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
