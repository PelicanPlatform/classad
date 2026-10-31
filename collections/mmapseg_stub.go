//go:build !unix

package collections

import "os"

// mmapSupported reports whether mmap-backed (persistent) segments are available
// on this platform. Persistence is currently unix-only.
const mmapSupported = false

func newMmapSegment(id uint32, size int, codec Codec, path string) (*segment, error) {
	return nil, errNoMmap
}

func openMmapSegment(id uint32, codec Codec, f *os.File, size int) (*segment, error) {
	return nil, errNoMmap
}

func (s *segment) flush() error {
	if !s.persistent {
		return nil
	}
	return errNoMmap
}

func (s *segment) msyncRange(from, to int) error {
	if !s.persistent {
		return nil
	}
	return errNoMmap
}

// isDurabilityFailure is unused on non-unix (no persistence, so msync never runs), but must exist
// for the shared commit path to compile.
func isDurabilityFailure(error) bool { return false }

func (s *segment) reap() error {
	if !s.persistent {
		return nil
	}
	return errNoMmap
}

func (s *segment) unmap() error {
	if !s.persistent {
		return nil
	}
	return errNoMmap
}

// adviseSequential and adviseNormal are readahead hints for the recovery walk; there is
// no mapping to advise without persistence.
func (s *segment) adviseSequential() {}
func (s *segment) adviseNormal()     {}
