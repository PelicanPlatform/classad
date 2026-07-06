//go:build unix

package collections

import (
	"fmt"
	"os"

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
	if err := f.Truncate(int64(size)); err != nil {
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
	return &segment{id: id, data: data, codec: codec, file: f}, nil
}

// openMmapSegment maps an existing segment file (recovery). used is set by the
// caller after scanning the durable region.
func openMmapSegment(id uint32, codec Codec, f *os.File, size int) (*segment, error) {
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap fd: %w", err)
	}
	return &segment{id: id, data: data, codec: codec, file: f}, nil
}

// flush msyncs the not-yet-durable bytes [synced, used) to disk and advances the
// durable length. Page-aligned start is required by msync. No-op for RAM segments.
func (s *segment) flush() error {
	if s.file == nil || s.used <= s.synced {
		return nil
	}
	const page = 4096
	start := s.synced &^ (page - 1)
	if err := unix.Msync(s.data[start:s.used], unix.MS_SYNC); err != nil {
		return err
	}
	s.synced = s.used
	return nil
}

// unmap flushes, unmaps, and closes an mmap segment's file. No-op for RAM.
func (s *segment) unmap() error {
	if s.file == nil {
		return nil
	}
	err := s.flush()
	if e := unix.Munmap(s.data); err == nil {
		err = e
	}
	if e := s.file.Close(); err == nil {
		err = e
	}
	s.file = nil
	return err
}
