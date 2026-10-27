package collections

import (
	"fmt"
	"iter"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

const closeGateChildEnv = "CLASSAD_CLOSEGATE_CHILD"

// TestReleaseUnderReaders is the collections-level regression test for releasing a
// collection's mappings while something is reading it.
//
// Like the db-level one it runs in a CHILD PROCESS: reading a munmapped segment is a Go
// runtime fault, not a panic, so the only way to assert "did not crash" is to watch a
// child's exit status -- and a regression would otherwise take the whole package's results
// with it.
func TestReleaseUnderReaders(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestReleaseUnderReadersChild$", "-test.v", "-test.timeout=4m")
	cmd.Env = append(os.Environ(), closeGateChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a reader did not survive the release of the collection; child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("child did not pass:\n%s", out)
	}
}

// TestReleaseUnderReadersChild holds the cases that fault when the bug is back. Inert
// unless re-exec'd by TestReleaseUnderReaders.
func TestReleaseUnderReadersChild(t *testing.T) {
	if os.Getenv(closeGateChildEnv) != "1" {
		t.Skip("runs only as the child of TestReleaseUnderReaders")
	}
	t.Run("DiscardKeepsAReaderWhole", discardKeepsReaderWhole)
	t.Run("CloseTruncatesWithoutFaulting", closeTruncatesWithoutFaulting)
	t.Run("ReadAfterRelease", readAfterRelease)
}

// discardKeepsReaderWhole: a reader that registered through AcquireRead reads the whole
// collection even though Discard ran in the middle of it -- the mappings are released only
// once it leaves.
func discardKeepsReaderWhole(t *testing.T) {
	const n = 20000
	c := gateFixture(t, n)
	if !c.AcquireRead() {
		t.Fatal("AcquireRead refused an open collection")
	}
	next, stop := iter.Pull(c.Scan())
	defer stop()
	if _, ok := next(); !ok {
		t.Fatal("no rows")
	}
	if err := c.Discard(); err != nil {
		t.Fatal(err)
	}
	got := 1
	for {
		if _, ok := next(); !ok {
			break
		}
		got++
	}
	c.ReleaseRead()
	if got != n {
		t.Fatalf("reader saw %d of %d rows across a Discard", got, n)
	}
}

// closeTruncatesWithoutFaulting: an UNREGISTERED reader -- one that never took the gate --
// is still not allowed to fault. Close empties the shards it has released, so a scan parked
// between shards comes back short rather than walking a released mapping. Short is the
// deliberate answer here: Close is synchronous because its caller is owed the write-out.
func closeTruncatesWithoutFaulting(t *testing.T) {
	const n = 20000
	c := gateFixture(t, n)
	next, stop := iter.Pull(c.Scan())
	defer stop()
	if _, ok := next(); !ok {
		t.Fatal("no rows")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	got := 1
	for {
		if _, ok := next(); !ok {
			break
		}
		got++
	}
	if got > n {
		t.Fatalf("reader saw %d rows, more than the %d stored", got, n)
	}
}

// readAfterRelease: every read shape started after the release answers empty.
func readAfterRelease(t *testing.T) {
	c := gateFixture(t, 2000)
	if err := c.Discard(); err != nil {
		t.Fatal(err)
	}
	if c.AcquireRead() {
		t.Fatal("AcquireRead admitted a reader to a released collection")
	}
	n := 0
	for range c.Scan() {
		n++
	}
	if n != 0 {
		t.Fatalf("scan of a released collection yielded %d rows", n)
	}
	if _, ok := c.Get([]byte("k7")); ok {
		t.Fatal("point lookup found a row in a released collection")
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("released collection Len() = %d, want 0", got)
	}
}

// gateFixture is a persistent collection of n ads, wide enough to span several segments per
// shard so a scan has somewhere to be parked when the release lands.
func gateFixture(t *testing.T, n int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for i := range n {
		ad, err := classad.ParseOld(fmt.Sprintf("Name = %q\nX = %d\nPad = %q",
			fmt.Sprintf("n%d", i), i, "0123456789012345678901234567890123456789"))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}
