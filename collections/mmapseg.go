//go:build unix

package collections

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// mmapSupported reports whether mmap-backed (persistent) segments are available
// on this platform.
const mmapSupported = true

// newMmapSegment creates a fresh mmap-backed segment: a file of the given size at
// path, mapped MAP_SHARED for read/write. The returned segment.data aliases the
// mapping (page-aligned, so the 8-byte MVCC atomics are aligned); record framing
// and append are identical to a RAM segment.
func newMmapSegment(id uint32, size int, codec Codec, path string) (*segment, error) {
	size = recAlign(size)
	if size < recHeaderSize+8 {
		size = recAlign(recHeaderSize + 8)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	// Reserve the segment's blocks up front (fallocate on Linux) rather than a sparse Truncate, so a
	// disk-full surfaces here as a clean allocation error instead of later at mmap writeback (where
	// it can silently lose data or SIGBUS). See segreserve_*.go.
	if err := reserve(f, int64(size)); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	// The mapping keeps the file alive on its own (mmap adds a reference the fd's
	// close does not drop), and nothing after this needs the fd: writes go to the
	// mapping, durability is msync (address-based), and retirement munmaps + unlinks
	// by path. So release the fd now -- a large store otherwise holds one fd per
	// live segment (thousands past RAM). See segment.persistent.
	f.Close()
	return &segment{id: id, data: data, codec: codec, persistent: true, path: path}, nil
}

// openMmapSegment maps an existing segment file (recovery). used is set by the
// caller after scanning the durable region. The fd is released once mapped (the
// caller reads through seg.data, not the file); see newMmapSegment.
func openMmapSegment(id uint32, codec Codec, f *os.File, size int) (*segment, error) {
	path := f.Name()
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap fd: %w", err)
	}
	f.Close()
	return &segment{id: id, data: data, codec: codec, persistent: true, path: path}, nil
}

// pageSize is the system page size, which msync's start address must be aligned to. It is
// not always 4 KiB: Apple silicon uses 16 KiB pages and some arm64 Linux kernels 64 KiB,
// and a start aligned only to 4 KiB fails there with EINVAL.
var pageSize = os.Getpagesize()

// flush msyncs the not-yet-durable bytes [synced, used) to disk and advances the
// durable length. Page-aligned start is required by msync. No-op for RAM segments.
func (s *segment) flush() error {
	if !s.persistent || s.used <= s.synced {
		return nil
	}
	start := s.synced &^ (pageSize - 1)
	if err := unix.Msync(s.data[start:s.used], unix.MS_SYNC); err != nil {
		return err
	}
	if h := msyncDoneHook.Load(); h != nil {
		(*h)(s.path, s.used)
	}
	if h := msyncCopyHook.Load(); h != nil {
		(*h)(s, start, s.data[start:s.used])
	}
	s.synced = s.used
	return nil
}

// msyncDoneHook is a test seam: when set, it is called after every successful msync of a
// segment with the segment's path and the end of the range now on disk, so a test can
// build the image a machine crash would leave. Nil in production.
var msyncDoneHook atomic.Pointer[func(path string, to int)]

// msyncCopyHook is a test seam like msyncDoneHook that also hands over the bytes an msync just made
// durable (b, starting at offset start of the segment's file), so a test can keep the image a
// machine crash would leave of a segment whose pages are rewritten in place. The segment, not its
// path, names the file: a rewrite renames its output after syncing it. Nil in production.
var msyncCopyHook atomic.Pointer[func(seg *segment, start int, b []byte)]

// msyncFailHook is a test seam: when non-nil and it returns a non-nil error, msyncRange fails with
// that error instead of calling the syscall, simulating a durability-sync failure (e.g. EIO). Nil
// in production.
var msyncFailHook func() error

// isDurabilityFailure reports whether an msync error means data did not reach disk (so a commit
// must be treated as non-durable and fail-stopped) versus a benign/platform error like EINVAL
// (which some platforms return for MS_SYNC ranges and which does not indicate data loss). Only the
// former is latched into syncErr; latching EINVAL would wrongly wedge the store on such platforms.
// EINVAL also answers a start address not aligned to the page size, with nothing synced, which is
// why every msync here aligns its start to pageSize.
func isDurabilityFailure(err error) bool {
	return errors.Is(err, unix.EIO) || errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)
}

// msyncRange flushes [from, to) of an mmap segment to disk (page-aligned start).
// Used by group-commit durability, which advances synced under the shard lock and
// then calls this lock-free. No-op for RAM segments.
func (s *segment) msyncRange(from, to int) error {
	if !s.persistent || to <= from {
		return nil
	}
	if msyncFailHook != nil {
		if err := msyncFailHook(); err != nil {
			return err
		}
	}
	start := from &^ (pageSize - 1)
	if err := unix.Msync(s.data[start:to], unix.MS_SYNC); err != nil {
		return err
	}
	if h := msyncDoneHook.Load(); h != nil {
		(*h)(s.path, to)
	}
	if h := msyncCopyHook.Load(); h != nil {
		(*h)(s, start, s.data[start:to])
	}
	return nil
}

// reap unmaps (without a final flush — the file is being discarded) and unlinks a
// retired mmap segment. Called at most once, after the pin count drains to zero.
// No-op for a RAM segment.
func (s *segment) reap() error {
	if !s.persistent {
		return nil
	}
	err := unix.Munmap(s.data)
	if s.file != nil { // normally already released after mmap; guard for safety
		if e := s.file.Close(); err == nil {
			err = e
		}
		s.file = nil
	}
	if s.path != "" {
		if e := os.Remove(s.path); err == nil {
			err = e
		}
		os.Remove(s.path + ".idx") // best-effort: drop the sidecar container with the segment
	}
	return err
}

// unmap flushes, unmaps, and closes an mmap segment's file. No-op for RAM.
func (s *segment) unmap() error {
	if !s.persistent {
		return nil
	}
	err := s.flush()
	if e := unix.Munmap(s.data); err == nil {
		err = e
	}
	if s.file != nil { // normally already released after mmap; guard for safety
		if e := s.file.Close(); err == nil {
			err = e
		}
		s.file = nil
	}
	return err
}

// adviseSequential tells the kernel this mapping is about to be read front to back,
// so it should read ahead aggressively and not retain the pages behind the cursor.
//
// Recovery is the caller. A record walk reads only header fields -- totalLen, seq, the
// key -- and skips each ad body, but faults are page-granular: at a kilobyte per record
// every 4 KiB page holds several headers, so walking headers faults in the WHOLE store.
// On a 10 GB table that is 10 GB of read I/O and a matching resident spike, which in a
// memory-limited container is the difference between a slow start and being killed. The
// access pattern is strictly forward, so the kernel can do much better than its default
// guess; it just has to be told.
//
// Best effort: an error means the hint was not applied, which costs speed and nothing
// else, so it is dropped rather than failing an open.
func (s *segment) adviseSequential() {
	if !s.persistent || len(s.data) == 0 {
		return
	}
	_ = unix.Madvise(s.data, unix.MADV_SEQUENTIAL)
}

// adviseNormal undoes adviseSequential. Steady-state access is by key and therefore
// random, and MADV_SEQUENTIAL also asks the kernel to drop pages the cursor has passed --
// both wrong once recovery is over, so the hint is reset rather than left behind.
func (s *segment) adviseNormal() {
	if !s.persistent || len(s.data) == 0 {
		return
	}
	_ = unix.Madvise(s.data, unix.MADV_NORMAL)
}
