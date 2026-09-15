package collector

import (
	"os"
	"path/filepath"
	"strings"
)

const bcacheSysfsRoot = "/sys/fs/bcache"

// bcacheBdev ties a bcache backing device to the names it is known by:
// the cache set index (bdevN, the only identifier the node_exporter emits),
// the block device (bcacheM) and the disk behind it (sdX).
type bcacheBdev struct {
	BackingDevice string
	Bcache        string
	Disk          string
	CacheSet      string
}

// bcacheAvailable reports whether this host runs bcache at all. Without the
// module loaded or without a registered cache set the sysfs root is absent,
// and every bcache lookup is skipped for the rest of the scrape.
func bcacheAvailable() bool {
	st, err := os.Stat(bcacheSysfsRoot)
	return err == nil && st.IsDir()
}

// bcacheIndex maps the bcacheM block device name to its backing device facts.
// bdevN numbering follows the attach order within a cache set and matches
// neither the bcacheM numbering nor the disk order, so it can only be read
// from the cache set directory.
//
// Every failure mode degrades to "no mapping" rather than to an error: a
// missing sysfs root, an unreadable cache set, or a dangling symlink each skip
// only what they affect, so a host without bcache simply emits no such metric.
func bcacheIndex() map[string]bcacheBdev {
	sets, err := os.ReadDir(bcacheSysfsRoot)
	if err != nil {
		return nil
	}

	index := make(map[string]bcacheBdev)
	for _, set := range sets {
		cset := set.Name()
		if !strings.Contains(cset, "-") {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(bcacheSysfsRoot, cset))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "bdev") {
				continue
			}
			base := filepath.Join(bcacheSysfsRoot, cset, name)

			// The bdev symlink points at <disk>/bcache, its dev symlink at bcacheM.
			target, err := filepath.EvalSymlinks(base)
			if err != nil {
				continue
			}
			blockDev, err := filepath.EvalSymlinks(filepath.Join(base, "dev"))
			if err != nil {
				continue
			}
			index[filepath.Base(blockDev)] = bcacheBdev{
				BackingDevice: name,
				Bcache:        filepath.Base(blockDev),
				Disk:          filepath.Base(filepath.Dir(target)),
				CacheSet:      cset,
			}
		}
	}
	return index
}

// bcacheForDevice walks the slave chain below a block device until it hits a
// bcache device. A btrfs device is usually a dm-crypt mapping on top of
// bcacheM, but the depth limit also covers deeper stacks.
func bcacheForDevice(dev string, index map[string]bcacheBdev) (bcacheBdev, bool) {
	if len(index) == 0 {
		return bcacheBdev{}, false
	}

	seen := make(map[string]bool)
	queue := []string{dev}
	for depth := 0; depth < 8 && len(queue) > 0; depth++ {
		var next []string
		for _, cur := range queue {
			if seen[cur] {
				continue
			}
			seen[cur] = true

			if info, ok := index[cur]; ok {
				return info, true
			}

			slaves, err := os.ReadDir(filepath.Join("/sys/block", cur, "slaves"))
			if err != nil {
				continue
			}
			for _, s := range slaves {
				next = append(next, s.Name())
			}
		}
		queue = next
	}
	return bcacheBdev{}, false
}
