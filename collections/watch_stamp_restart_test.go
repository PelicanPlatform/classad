package collections

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// openStamped opens dir as a mutable collection whose watch epoch (and so its stamp
// high-water mark) is carried across a clean Close.
func openStamped(t *testing.T, dir string) *Collection {
	t.Helper()
	c, err := Open(Options{
		Dir:          dir,
		Shards:       1,
		WatchHistory: 1024,
		WatchRetain:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestWatchStampsDoNotRepeatAfterTheClockMovesBack is the reason the stamp is persisted at
// all. A stamp is read off the wall clock, so a clock that moved backwards while the process
// was down -- an NTP step, a VM resumed from a snapshot, a host that disagrees about now --
// would hand a reconnecting watcher stamps it has already been given. A consumer publishing
// them as a version would then move backwards, which is precisely what a version may not do.
//
// The assertion is on stamps actually ISSUED to a watcher, not on the watermark: with no
// watcher attached a commit takes no stamp at all, and two reads of a frozen clock would
// then compare equal and prove nothing.
func TestWatchStampsDoNotRepeatAfterTheClockMovesBack(t *testing.T) {
	dir := t.TempDir()
	const start = uint64(1_700_000_000_000) // fixed ms, so the test does not depend on now
	const keys = 8
	setClock := fakeClock(t, start)

	write := func(c *Collection, gen int) map[string]uint64 {
		t.Helper()
		seq, err := c.Watch(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		next, stop := drainTo(t, seq)
		defer stop()
		for i := range keys {
			if err := c.Put([]byte(fmt.Sprintf("key-%03d.%d", i, gen)), classad.New()); err != nil {
				t.Fatal(err)
			}
		}
		return liveStamps(t, next, keys)
	}

	c := openStamped(t, dir)
	highest := uint64(0)
	for _, st := range write(c, 0) {
		highest = max(highest, st)
	}
	if highest == 0 {
		t.Fatal("no stamp was issued before the restart, so the comparison below proves nothing")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// The clock loses an hour while the store is down.
	setClock(start - uint64(time.Hour/time.Millisecond))

	c2 := openStamped(t, dir)
	defer c2.Close()
	lowest := ^uint64(0)
	for _, st := range write(c2, 1) {
		lowest = min(lowest, st)
	}
	if lowest <= highest {
		t.Errorf("lowest stamp issued after the restart = %d, want > %d (the highest issued "+
			"before it): the clock went backwards and took the stamps with it, so a watcher "+
			"is handed a version it has already seen", lowest, highest)
	}
}

// TestWatchEpochV1MarkerIsStillAccepted: markers written before stamps existed carry no
// stamp. Refusing them would rotate the epoch on the first open after an upgrade and Reset
// every watcher of every archive -- the exact storm the epoch exists to avoid.
func TestWatchEpochV1MarkerIsStillAccepted(t *testing.T) {
	seqs := []uint64{11, 22, 33}
	b := make([]byte, 0, 64)
	b = binary.LittleEndian.AppendUint32(b, watchEpochMagic)
	b = binary.LittleEndian.AppendUint32(b, watchEpochVersionV1)
	b = binary.LittleEndian.AppendUint64(b, 0xABCDEF)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(seqs)))
	for _, s := range seqs {
		b = binary.LittleEndian.AppendUint64(b, s)
	}
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))

	m, ok := decodeWatchEpoch(b)
	if !ok {
		t.Fatal("a version-1 marker was refused; every archive would Reset its watchers on upgrade")
	}
	if m.epoch != 0xABCDEF {
		t.Errorf("epoch = %#x, want %#x", m.epoch, 0xABCDEF)
	}
	if len(m.seqs) != len(seqs) || m.seqs[2] != 33 {
		t.Errorf("seqs = %v, want %v", m.seqs, seqs)
	}
	if m.stamp != 0 {
		t.Errorf("stamp = %d, want 0: a v1 marker records none", m.stamp)
	}
}

// TestWatchEpochRoundTripsTheStamp pins the v2 encoding itself, so a field added later
// cannot quietly shift the stamp's offset.
func TestWatchEpochRoundTripsTheStamp(t *testing.T) {
	in := watchEpochMark{epoch: 7, seqs: []uint64{1, 2}, stamp: 123456789}
	out, ok := decodeWatchEpoch(encodeWatchEpoch(in))
	if !ok {
		t.Fatal("a marker this build just wrote did not decode")
	}
	if out.stamp != in.stamp || out.epoch != in.epoch || len(out.seqs) != 2 || out.seqs[1] != 2 {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}
