//go:build unix

package collections

import (
	"os"

	"golang.org/x/sys/unix"
)

// mapFile memory-maps path read-only and returns the bytes plus a closer that
// unmaps them. The file descriptor is closed immediately (the mapping holds the
// pages); the returned bytes are demand-paged by the OS and evictable under memory
// pressure, so a sidecar index costs resident memory only for the postings a query
// actually touches. Used for Archive sidecar indexes (H5).
func mapFile(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := int(info.Size())
	if size == 0 {
		return nil, func() error { return nil }, nil
	}
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return unix.Munmap(data) }, nil
}

// mapAnon copies b into a fresh anonymous (file-less) mapping and returns it plus an unmap
// closer. Used for an in-memory collection's sealed-segment sidecar: the index bytes live
// OFF the Go heap, so they neither add to GC mark work (a mapping is not scanned) nor to the
// GOGC-paced heap, and the kernel can MADV_FREE them under pressure -- while the query path
// reads them exactly as a file-backed sidecar. On munmap the pages are gone (no writeback:
// an in-memory index is never persisted).
func mapAnon(b []byte) ([]byte, func() error, error) {
	if len(b) == 0 {
		return nil, func() error { return nil }, nil
	}
	data, err := unix.Mmap(-1, 0, len(b), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, nil, err
	}
	copy(data, b)
	return data, func() error { return unix.Munmap(data) }, nil
}
