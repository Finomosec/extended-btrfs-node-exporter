package collector

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	IncludeUUIDs  []string
	ExcludeUUIDs  []string
	BeesStatusDir string

	// Subvolume name filters
	IncludeSubvolumes []string // empty = all
	ExcludeSubvolumes []string

	// Device name display: false = dm-X (raw), true = /dev/mapper/* name (resolved)
	ResolveDeviceMapper bool

	// Timeout for ioctl calls (protects against kernel lock contention)
	IoctlTimeout time.Duration

	// Log level: "info" (default) or "debug"
	Debug bool

	// Collector modules (all true by default)
	CollectSubvolumes bool // non-snapshot subvolumes
	CollectSnapshots  bool // snapshot subvolumes
	CollectQgroups    bool
	CollectCommit     bool
	CollectReplace    bool
	CollectBalance    bool
	CollectScrub      bool
	CollectDefrag     bool
	CollectBees       bool
	CollectOrphans    bool
}

func LoadConfig() Config {
	cfg := Config{
		BeesStatusDir:     "/run/bees",
		ResolveDeviceMapper: envBool("RESOLVE_DEVICE_MAPPER", false),
		IoctlTimeout:       envDuration("IOCTL_TIMEOUT_SECS", 30),
		Debug:              envBool("DEBUG", false),
		CollectSubvolumes:  envBool("COLLECT_SUBVOLUMES", true),
		CollectSnapshots:  envBool("COLLECT_SNAPSHOTS", true),
		CollectQgroups:    envBool("COLLECT_QGROUPS", true),
		CollectCommit:     envBool("COLLECT_COMMIT", true),
		CollectReplace:    envBool("COLLECT_REPLACE", true),
		CollectBalance:    envBool("COLLECT_BALANCE", true),
		CollectScrub:      envBool("COLLECT_SCRUB", true),
		CollectDefrag:     envBool("COLLECT_DEFRAG", true),
		CollectBees:       envBool("COLLECT_BEES", true),
		CollectOrphans:    envBool("COLLECT_ORPHANS", true),
	}

	if v := os.Getenv("BTRFS_INCLUDE_UUIDS"); v != "" {
		cfg.IncludeUUIDs = splitTrim(v)
	}
	if v := os.Getenv("BTRFS_EXCLUDE_UUIDS"); v != "" {
		cfg.ExcludeUUIDs = splitTrim(v)
	}
	if v := os.Getenv("BTRFS_INCLUDE_SUBVOLUMES"); v != "" {
		cfg.IncludeSubvolumes = splitTrim(v)
	}
	if v := os.Getenv("BTRFS_EXCLUDE_SUBVOLUMES"); v != "" {
		cfg.ExcludeSubvolumes = splitTrim(v)
	}
	if v := os.Getenv("BEES_STATUS_DIR"); v != "" {
		cfg.BeesStatusDir = v
	}

	return cfg
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			result = append(result, t)
		}
	}
	return result
}

func envDuration(key string, defaultSecs int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(defaultSecs) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(defaultSecs) * time.Second
	}
	return time.Duration(n) * time.Second
}

func envBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	return v == "true" || v == "1" || v == "yes"
}

func (c Config) ShouldCollectFS(uuid string) bool {
	if len(c.IncludeUUIDs) > 0 {
		for _, u := range c.IncludeUUIDs {
			if u == uuid {
				return true
			}
		}
		return false
	}
	for _, u := range c.ExcludeUUIDs {
		if u == uuid {
			return false
		}
	}
	return true
}

// ShouldCollectSubvol checks if a subvolume should be collected
// based on snapshot status and name include/exclude filters
func (c Config) ShouldCollectSubvol(path string, isSnapshot bool) bool {
	if isSnapshot && !c.CollectSnapshots {
		return false
	}
	if !isSnapshot && !c.CollectSubvolumes {
		return false
	}

	if len(c.IncludeSubvolumes) > 0 {
		for _, pattern := range c.IncludeSubvolumes {
			if strings.Contains(path, pattern) {
				return true
			}
		}
		return false
	}
	for _, pattern := range c.ExcludeSubvolumes {
		if strings.Contains(path, pattern) {
			return false
		}
	}
	return true
}
