package collector

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// BTRFS ioctl magic
const btrfsMagic = 0x94

// ioctl direction bits
const (
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}
func iowr(typ, nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, typ, nr, size) }
func ior(typ, nr, size uintptr) uintptr  { return ioc(iocRead, typ, nr, size) }

// ioctl numbers
var (
	iocTreeSearch        = iowr(btrfsMagic, 17, 4096)
	iocDevInfo           = iowr(btrfsMagic, 30, unsafe.Sizeof(devInfoArgs{}))
	iocBalanceProgress   = ior(btrfsMagic, 34, unsafe.Sizeof(balanceArgs{}))
	iocQuotaRescanStatus = ior(btrfsMagic, 45, unsafe.Sizeof(quotaRescanArgs{}))
	iocDevReplace        = iowr(btrfsMagic, 53, 2600) // sizeof(struct btrfs_ioctl_dev_replace_args)
)

// Key types
const (
	orphanItemKey  = 48
	rootItemKey    = 132
	rootBackrefKey = 144
	qgroupInfoKey  = 242
)

// Object IDs
const (
	rootTreeObjectID    = 1
	quotaTreeObjectID   = 8
	orphanObjectID      = uint64(0xFFFFFFFFFFFFFFFB) // -5 as uint64
	fsTreeObjectID      = 5
	firstFreeObjectID   = 256
	lastFreeObjectID    = uint64(0xFFFFFFFFFFFFFF00) // -256 as uint64
)

// --- Timeout wrapper with goroutine leak protection ---
// Wraps blocking ioctl calls in a goroutine with configurable timeout.
// Uses a semaphore per call-key to prevent goroutine accumulation:
// if a previous call is still blocked, immediately returns an error
// instead of spawning another blocked goroutine.

var (
	pendingMu   sync.Mutex
	pendingCall = map[string]bool{} // key → true if a goroutine is still running
)

func withTimeout[T any](key string, timeout time.Duration, fn func() (T, error)) (T, error) {
	pendingMu.Lock()
	if pendingCall[key] {
		pendingMu.Unlock()
		var zero T
		return zero, fmt.Errorf("skipped: previous call still blocked for %s", key)
	}
	pendingCall[key] = true
	pendingMu.Unlock()

	type result struct {
		val T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, e := fn()
		pendingMu.Lock()
		delete(pendingCall, key)
		pendingMu.Unlock()
		ch <- result{v, e}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case r := <-ch:
		return r.val, r.err
	case <-ctx.Done():
		// Goroutine stays running but pendingCall[key] remains true
		// → next call will skip immediately instead of spawning another
		var zero T
		return zero, fmt.Errorf("ioctl timeout after %v for %s", timeout, key)
	}
}

// --- Tree Search ---

type searchKey struct {
	TreeID      uint64
	MinObjectID uint64
	MaxObjectID uint64
	MinOffset   uint64
	MaxOffset   uint64
	MinTransID  uint64
	MaxTransID  uint64
	MinType     uint32
	MaxType     uint32
	NrItems     uint32
	_unused     [36]byte
}

type searchHeader struct {
	TransID  uint64
	ObjectID uint64
	Offset   uint64
	Type     uint32
	Len      uint32
}

type searchArgs struct {
	Key searchKey
	Buf [3992]byte
}

func doTreeSearch(fd uintptr, args *searchArgs) (uint32, error) {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, iocTreeSearch, uintptr(unsafe.Pointer(args)))
	if errno != 0 {
		return 0, errno
	}
	return args.Key.NrItems, nil
}

// SubvolInfo contains subvolume metadata
type SubvolInfo struct {
	ID         uint64
	Generation uint64
	ParentUUID [16]byte
	Path       string
}

// ListSubvolumes lists all subvolumes using BTRFS_IOC_TREE_SEARCH
func ListSubvolumes(fd int, mountpoint string, timeout time.Duration) ([]SubvolInfo, error) {
	return withTimeout(mountpoint+":subvols", timeout, func() ([]SubvolInfo, error) {
		return listSubvolumesImpl(fd, mountpoint)
	})
}

func listSubvolumesImpl(fd int, mountpoint string) ([]SubvolInfo, error) {
	

	var result []SubvolInfo
	backrefs := map[uint64]struct{ parentID uint64; name string }{}

	// Search both ROOT_ITEM and ROOT_BACKREF in one pass (like btrfs-progs)
	var args searchArgs
	args.Key.TreeID = rootTreeObjectID
	args.Key.MinObjectID = fsTreeObjectID
	args.Key.MaxObjectID = lastFreeObjectID
	args.Key.MinType = rootItemKey
	args.Key.MaxType = rootBackrefKey
	args.Key.MinOffset = 0
	args.Key.MaxOffset = ^uint64(0)
	args.Key.MinTransID = 0
	args.Key.MaxTransID = ^uint64(0)

	for {
		args.Key.NrItems = 4096
		nr, err := doTreeSearch(uintptr(fd), &args)
		if err != nil {
			return result, err
		}
		if nr == 0 {
			break
		}

		off := 0
		var sh searchHeader
		for i := uint32(0); i < nr; i++ {
			if off+int(unsafe.Sizeof(sh)) > len(args.Buf) {
				break
			}
			sh = *(*searchHeader)(unsafe.Pointer(&args.Buf[off]))
			off += int(unsafe.Sizeof(sh))

			if sh.Type == rootBackrefKey && sh.Len > 18 {
				itemData := args.Buf[off : off+int(sh.Len)]
				// btrfs_root_ref: dirid(8) + sequence(8) + name_len(2) + name
				nameLen := binary.LittleEndian.Uint16(itemData[16:18])
				if int(18+nameLen) <= len(itemData) {
					name := string(itemData[18 : 18+nameLen])
					backrefs[sh.ObjectID] = struct{ parentID uint64; name string }{sh.Offset, name}
				}
			} else if sh.Type == rootItemKey &&
				(sh.ObjectID >= firstFreeObjectID || sh.ObjectID == fsTreeObjectID) {
				itemData := args.Buf[off : off+int(sh.Len)]
				// generation is at offset 0x80 (128) in btrfs_root_item (after inode_item)
				// Actually: struct btrfs_inode_item (160 bytes) + generation(8)
				gen := uint64(0)
				if len(itemData) >= 168 {
					gen = binary.LittleEndian.Uint64(itemData[160:168])
				}
				var parentUUID [16]byte
				// parent_uuid is at different offsets depending on root_item version
				// Full root_item (>= 439 bytes): parent_uuid at offset 0x67*2+...
				// Use the standard offset: after uuid (at 0xB0=176), parent_uuid at 0xC0=192
				if int(sh.Len) >= 208 {
					copy(parentUUID[:], itemData[192:208])
				}
				result = append(result, SubvolInfo{
					ID:         sh.ObjectID,
					Generation: gen,
					ParentUUID: parentUUID,
				})
			}
			off += int(sh.Len)
		}

		// Pagination exactly like btrfs-progs
		args.Key.MinObjectID = sh.ObjectID
		args.Key.MinType = sh.Type
		args.Key.MinOffset = sh.Offset + 1
		if args.Key.MinOffset == 0 {
			args.Key.MinType++
		}
		if args.Key.MinType > rootBackrefKey {
			args.Key.MinType = rootItemKey
			args.Key.MinObjectID++
		}
		if args.Key.MinObjectID > args.Key.MaxObjectID {
			break
		}
	}


	// Build paths from backrefs
	for i := range result {
		result[i].Path = buildPath(result[i].ID, backrefs)
	}

	return result, nil
}

// buildPath constructs the full path for a subvolume from backrefs
func buildPath(id uint64, backrefs map[uint64]struct{ parentID uint64; name string }) string {
	if id == fsTreeObjectID {
		return "."
	}
	br, ok := backrefs[id]
	if !ok {
		return fmt.Sprintf("subvol-%d", id)
	}
	path := br.name
	parentID := br.parentID
	for parentID > fsTreeObjectID {
		pbr, ok := backrefs[parentID]
		if !ok {
			break
		}
		path = pbr.name + "/" + path
		parentID = pbr.parentID
	}
	return path
}
// IsNullUUID checks if a UUID is all zeros
func IsNullUUID(uuid [16]byte) bool {
	for _, b := range uuid {
		if b != 0 {
			return false
		}
	}
	return true
}

// --- Qgroup Info ---

type QgroupInfo struct {
	SubvolID   uint64
	Referenced uint64
	Exclusive  uint64
}

// ListQgroups reads qgroup info via BTRFS_IOC_TREE_SEARCH on the quota tree
func ListQgroups(fd int, mountpoint string, timeout time.Duration) ([]QgroupInfo, error) {
	return withTimeout(mountpoint+":qgroups", timeout, func() ([]QgroupInfo, error) {
		return listQgroupsImpl(fd, mountpoint)
	})
}

func listQgroupsImpl(fd int, mountpoint string) ([]QgroupInfo, error) {
	

	var result []QgroupInfo
	var args searchArgs
	args.Key.TreeID = quotaTreeObjectID
	args.Key.MinObjectID = 0
	args.Key.MaxObjectID = ^uint64(0)
	args.Key.MinType = qgroupInfoKey
	args.Key.MaxType = qgroupInfoKey
	args.Key.MinOffset = 0
	args.Key.MaxOffset = ^uint64(0)
	args.Key.MinTransID = 0
	args.Key.MaxTransID = ^uint64(0)
	args.Key.NrItems = 4096

	for iterations := 0; iterations < 10000; iterations++ {
		nr, err := doTreeSearch(uintptr(fd), &args)
		if err != nil {
			return result, err
		}
		if nr == 0 {
			break
		}

		offset := 0
		var sh searchHeader
		for i := uint32(0); i < nr; i++ {
			if offset+int(unsafe.Sizeof(sh)) > len(args.Buf) {
				break
			}
			sh = *(*searchHeader)(unsafe.Pointer(&args.Buf[offset]))
			offset += int(unsafe.Sizeof(sh))

			if sh.Type == qgroupInfoKey && sh.Len >= 40 {
				itemData := args.Buf[offset : offset+int(sh.Len)]
				rfer := binary.LittleEndian.Uint64(itemData[8:16])
				excl := binary.LittleEndian.Uint64(itemData[24:32])
				subvolID := sh.Offset & 0xFFFFFFFFFFFF

				result = append(result, QgroupInfo{
					SubvolID:   subvolID,
					Referenced: rfer,
					Exclusive:  excl,
				})
			}
			offset += int(sh.Len)

			// Track last key for pagination
			args.Key.MinObjectID = sh.ObjectID
			args.Key.MinType = sh.Type
			args.Key.MinOffset = sh.Offset
		}

		// Pagination like btrfs-progs __qgroups_search
		args.Key.MinOffset++
		if args.Key.MinOffset == 0 {
			break // overflow
		}
		args.Key.NrItems = 4096
	}

	return result, nil
}

// --- Orphan Items ---

// CountOrphans counts orphan subvolume items via BTRFS_IOC_TREE_SEARCH
func CountOrphans(fd int, mountpoint string, timeout time.Duration) (int, error) {
	return withTimeout(mountpoint+":orphans", timeout, func() (int, error) {
		return countOrphansImpl(fd, mountpoint)
	})
}

func countOrphansImpl(fd int, mountpoint string) (int, error) {
	

	count := 0
	var args searchArgs
	args.Key.TreeID = rootTreeObjectID
	args.Key.MinObjectID = orphanObjectID
	args.Key.MaxObjectID = orphanObjectID
	args.Key.MinType = orphanItemKey
	args.Key.MaxType = orphanItemKey
	args.Key.MinOffset = 0
	args.Key.MaxOffset = ^uint64(0)
	args.Key.MinTransID = 0
	args.Key.MaxTransID = ^uint64(0)
	args.Key.NrItems = 4096

	for iterations := 0; iterations < 10000; iterations++ {
		nr, err := doTreeSearch(uintptr(fd), &args)
		if err != nil || nr == 0 {
			break
		}

		offset := 0
		var lastOffset uint64
		for i := uint32(0); i < nr; i++ {
			if offset+int(unsafe.Sizeof(searchHeader{})) > len(args.Buf) {
				break
			}
			hdr := (*searchHeader)(unsafe.Pointer(&args.Buf[offset]))
			offset += int(unsafe.Sizeof(searchHeader{}))
			lastOffset = hdr.Offset
			count++
			offset += int(hdr.Len)
		}

		if lastOffset == ^uint64(0) { break }
		args.Key.MinOffset = lastOffset + 1
		args.Key.NrItems = 4096
	}

	return count, nil
}

// --- Dev Replace Status ---

type devReplaceStatusParams struct {
	ReplaceState uint64
	Progress1000 uint64
	TimeStarted  uint64
	TimeStopped  uint64
	NumWriteErrs uint64
	NumReadErrs  uint64
}

type devReplaceArgs struct {
	Cmd    uint64
	Result uint64
	_      [2584]byte // union(2072) + spare(512) = total 2600 - cmd(8) - result(8) = 2584
}

const devReplaceCmdStatus = 1

type ReplaceStatus struct {
	Running   bool
	Progress  float64
	WriteErrs uint64
	ReadErrs  uint64
}

// GetReplaceStatus queries device replace status via ioctl
func GetReplaceStatus(fd int, mountpoint string, timeout time.Duration) (*ReplaceStatus, error) {
	return withTimeout(mountpoint+":replace", timeout, func() (*ReplaceStatus, error) {
		return getReplaceStatusImpl(fd, mountpoint)
	})
}

func getReplaceStatusImpl(fd int, mountpoint string) (*ReplaceStatus, error) {

	var args devReplaceArgs
	args.Cmd = devReplaceCmdStatus

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocDevReplace, uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return nil, nil
	}

	if args.Result != 0 { // BTRFS_IOCTL_DEV_REPLACE_RESULT_NO_ERROR = 0
		return nil, nil
	}

	// Status params start at offset 16 (after cmd + result)
	status := (*devReplaceStatusParams)(unsafe.Pointer(uintptr(unsafe.Pointer(&args)) + 16))
	if status.ReplaceState == 0 { // NEVER_STARTED
		return nil, nil
	}

	// Note: srcdevid and tgtdev_name are only in start_params (different union member),
	// not populated by STATUS cmd. Device names are resolved via sysfs in collectReplace.
	return &ReplaceStatus{
		Running:   status.ReplaceState == 1,
		Progress:  float64(status.Progress1000) / 10.0,
		WriteErrs: status.NumWriteErrs,
		ReadErrs:  status.NumReadErrs,
	}, nil
}

// --- Balance Progress ---

type balanceProgress struct {
	Expected   uint64
	Considered uint64
	Completed  uint64
}

type balanceFilterArgs struct {
	Profiles   uint64
	Usage      uint64
	Devid      uint64
	Pstart     uint64
	Pend       uint64
	Vstart     uint64
	Vend       uint64
	Target     uint64
	Flags      uint64
	Limit      uint64
	StripesMin uint32
	StripesMax uint32
	Unused     [6]uint64
}

type balanceArgs struct {
	Flags  uint64
	State  uint64
	Data   balanceFilterArgs
	Meta   balanceFilterArgs
	Sys    balanceFilterArgs
	Stat   balanceProgress
	Unused [576]byte
}

const (
	balanceStateRunning   = 1
	balanceStatePauseReq  = 2
	balanceStateCancelReq = 4
)

type BalanceStatus struct {
	Running    bool
	State      string
	Expected   uint64
	Considered uint64
	Completed  uint64
}

// GetBalanceStatus queries balance progress via ioctl
func GetBalanceStatus(fd int, mountpoint string, timeout time.Duration) (*BalanceStatus, error) {
	return withTimeout(mountpoint+":balance", timeout, func() (*BalanceStatus, error) {
		return getBalanceStatusImpl(fd, mountpoint)
	})
}

func getBalanceStatusImpl(fd int, mountpoint string) (*BalanceStatus, error) {

	var args balanceArgs
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocBalanceProgress, uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		// ENOTCONN = no balance running (normal case)
		return &BalanceStatus{Running: false}, nil
	}

	state := "running"
	if args.State&balanceStatePauseReq != 0 {
		state = "pausing"
	}
	if args.State&balanceStateCancelReq != 0 {
		state = "canceling"
	}

	return &BalanceStatus{
		Running:    args.State&balanceStateRunning != 0,
		State:      state,
		Expected:   args.Stat.Expected,
		Considered: args.Stat.Considered,
		Completed:  args.Stat.Completed,
	}, nil
}

// --- Quota Rescan Status ---

type quotaRescanArgs struct {
	Flags    uint64
	Progress uint64
	Reserved [48]byte
}

type QuotaRescanStatus struct {
	Running  bool
	Progress uint64
}

// GetQuotaRescanStatus queries quota rescan status via ioctl
func GetQuotaRescanStatus(fd int, mountpoint string, timeout time.Duration) (*QuotaRescanStatus, error) {
	return withTimeout(mountpoint+":rescan", timeout, func() (*QuotaRescanStatus, error) {
		return getQuotaRescanStatusImpl(fd, mountpoint)
	})
}

func getQuotaRescanStatusImpl(fd int, mountpoint string) (*QuotaRescanStatus, error) {

	var args quotaRescanArgs
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocQuotaRescanStatus, uintptr(unsafe.Pointer(&args)))
	if errno != 0 {
		return &QuotaRescanStatus{Running: false}, nil
	}

	return &QuotaRescanStatus{
		Running:  args.Flags == 1,
		Progress: args.Progress,
	}, nil
}

// --- Dev Info + DevID Map ---

type devInfoArgs struct {
	DevID      uint64
	UUID       [16]byte
	BytesUsed  uint64
	TotalBytes uint64
	FsID       [16]byte
	Unused     [3016]byte
	Path       [1024]byte
} // 4096 bytes total

// devIDMapCache caches device mappings — they don't change at runtime
var devIDMapCache = map[string]map[string]string{}
var devIDMapCacheMu sync.Mutex

// DevIDMap builds devid→device-name mapping via BTRFS_IOC_DEV_INFO (path at offset 3072)
func DevIDMap(fd int, mountpoint string, uuid string) map[string]string {
	// Check cache — invalidate if device count changed
	devicesDir := fmt.Sprintf("/sys/fs/btrfs/%s/devices", uuid)
	entries, _ := os.ReadDir(devicesDir)
	devIDMapCacheMu.Lock()
	if cached, ok := devIDMapCache[uuid]; ok {
		// Invalidate if number of active devices changed
		activeCount := 0
		for _, v := range cached {
			if v != "missing" {
				activeCount++
			}
		}
		if activeCount == len(entries) {
			devIDMapCacheMu.Unlock()
			return cached
		}
	}
	devIDMapCacheMu.Unlock()

	result := map[string]string{}

	for devid := uint64(0); devid < 256; devid++ {
		var args devInfoArgs
		args.DevID = devid
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocDevInfo, uintptr(unsafe.Pointer(&args)))
		if errno != 0 {
			continue
		}

		key := fmt.Sprintf("%d", devid)
		path := strings.TrimRight(string(args.Path[:]), "\x00")
		path = strings.TrimSpace(path)

		if path == "" {
			result[key] = "missing"
		} else {
			result[key] = filepath.Base(path) // e.g. "dm-8" from "/dev/dm-8"
		}
	}

	if len(result) > 0 {
		devIDMapCacheMu.Lock()
		devIDMapCache[uuid] = result
		devIDMapCacheMu.Unlock()
	}

	return result
}
