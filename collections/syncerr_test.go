//go:build unix

package collections

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMsyncErrorSurfaced checks that a durability-sync (msync) failure is surfaced to the caller
// via writeError, not silently swallowed -- so a commit whose pages did not reach disk is reported
// as failed. The error is sticky (fail-stop) and cleared by Truncate.
func TestMsyncErrorSurfaced(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if putErr := c.Put([]byte("k0"), mustAd(t, `[Id=0]`)); putErr != nil {
		t.Fatalf("baseline Put failed: %v", putErr)
	}

	eio := unix.EIO // a real "data did not reach disk" errno (EINVAL etc. are benign, not latched)
	msyncFailHook = func() error { return eio }
	defer func() { msyncFailHook = nil }()

	// A write now triggers a durability sync that fails; the error must be surfaced. The sync may be
	// coalesced with a later commit, so a second write is allowed to be where it lands.
	err = c.Put([]byte("k1"), mustAd(t, `[Id=1]`))
	if err == nil {
		err = c.Put([]byte("k2"), mustAd(t, `[Id=2]`))
	}
	if !errors.Is(err, eio) {
		t.Fatalf("msync failure was not surfaced; Put returned %v", err)
	}
	// Sticky: subsequent writes keep reporting it (fail-stop).
	if err := c.Put([]byte("k3"), mustAd(t, `[Id=3]`)); !errors.Is(err, eio) {
		t.Errorf("sync error should be sticky; Put returned %v", err)
	}

	// Truncate clears the latched error; with the fault gone, writes resume.
	msyncFailHook = nil
	c.Truncate()
	if err := c.Put([]byte("k4"), mustAd(t, `[Id=4]`)); err != nil {
		t.Errorf("Truncate should clear the sync error; Put still fails: %v", err)
	}
}
