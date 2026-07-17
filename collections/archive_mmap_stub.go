//go:build !unix

package collections

import "os"

// mapFile falls back to reading the whole file into a heap buffer on platforms
// without mmap; the sidecar index is then resident rather than demand-paged, but
// behavior is otherwise identical (roaring FromBuffer references the heap slice,
// which the archiveSeg keeps alive).
func mapFile(path string) ([]byte, func() error, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return b, func() error { return nil }, nil
}

// mapAnon falls back to the heap slice itself on platforms without mmap: the sidecar bytes
// are then GC-cheap (a single pointer-free object, not a map of pointers) but heap-resident
// rather than off-heap/evictable. Behavior is otherwise identical.
func mapAnon(b []byte) ([]byte, func() error, error) {
	return b, func() error { return nil }, nil
}
