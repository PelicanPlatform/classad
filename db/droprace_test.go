package db

import (
	"fmt"
	"iter"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// dropRaceChildEnv marks the re-exec of this test binary that runs the racy half.
const dropRaceChildEnv = "CLASSAD_DROPRACE_CHILD"

// TestDropTableUnderReaders is the regression test for a DROP TABLE that unmapped a
// table's segments while a query was still reading them.
//
// Its cases run in a CHILD PROCESS because the failure is not a panic: reading a munmapped
// segment is a Go runtime fault (SIGSEGV at an address inside a slice whose bounds are
// still perfectly valid), which no recover can catch and which takes the whole test binary
// down with exit status 2 -- every other test in the package with it. A parent that only
// watches the child's exit status is the one shape that can assert "did not crash", and it
// keeps a regression from blanking the rest of the package's results.
func TestDropTableUnderReaders(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDropTableUnderReadersChild$", "-test.v", "-test.timeout=4m")
	cmd.Env = append(os.Environ(), dropRaceChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a reader did not survive a concurrent DROP TABLE; child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("child did not pass:\n%s", out)
	}
}

// TestDropTableUnderReadersChild holds the cases that fault when the bug is back. It is
// inert unless re-exec'd by TestDropTableUnderReaders.
func TestDropTableUnderReadersChild(t *testing.T) {
	if os.Getenv(dropRaceChildEnv) != "1" {
		t.Skip("runs only as the child of TestDropTableUnderReaders")
	}
	t.Run("StreamingScan", dropUnderScanChild)
	t.Run("ScanStartedAfterDrop", dropBeforeScanStarts)
	t.Run("AbandonedScanReleasesGate", dropAfterAbandonedScan)
	t.Run("ConcurrentReaders", dropWithConcurrentReaders)
}

// dropUnderScanChild streams a 40 000-row table, consumes one row, and drops the table
// while the iterator is parked between shards -- the gap where the per-segment pins are all
// released and nothing keeps the mapping alive. Before the fix this faulted on the first
// iteration, every time.
func dropUnderScanChild(t *testing.T) {
	const rows = 40000
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	d, err := cat.CreateTable("doomed")
	if err != nil {
		t.Fatal(err)
	}
	fillWide(t, d, rows)

	seq, err := d.QueryRawProjected("", []string{"Name", "X"}, false)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := iter.Pull(seq)
	defer stop()
	if _, ok := next(); !ok {
		t.Fatal("no rows from a 40000-row table")
	}

	// Drop while the consumer is parked. The sleep is not what makes this deterministic --
	// the iterator is suspended inside iter.Pull and cannot advance until next() is called
	// again -- it only makes sure the drop has run by then.
	done := make(chan error, 1)
	go func() { done <- cat.DropTable("doomed") }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	got := 1
	for {
		if _, ok := next(); !ok {
			break
		}
		got++
	}
	// A drop must not truncate a scan that was already running: the reader is accounted
	// for, so it reads the table out in full and only then is the memory released.
	if got != rows {
		t.Fatalf("scan yielded %d rows after a concurrent drop, want all %d", got, rows)
	}
}

// dropBeforeScanStarts covers the other half: a handle obtained before the drop, read after
// it. There is nothing left to read, so the reads must come back empty -- the one thing they
// must not do is walk the released mapping.
func dropBeforeScanStarts(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	d, err := cat.CreateTable("doomed")
	if err != nil {
		t.Fatal(err)
	}
	fillWide(t, d, 2000)

	// The sequence is built before the drop but not iterated until after it.
	seq, err := d.QueryRawProjected("", []string{"Name"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.DropTable("doomed"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if n != 0 {
		t.Fatalf("dropped table yielded %d rows", n)
	}
	// The non-iterator reads go through the same emptied shards rather than any gate, so
	// check one of each shape here too.
	if _, ok := d.LookupClassAd("k7"); ok {
		t.Fatal("point lookup found a row in a dropped table")
	}
	if got := d.Len(); got != 0 {
		t.Fatalf("dropped table Len() = %d, want 0", got)
	}
	n = 0
	d.ForEach(func(*classad.ClassAd) bool { n++; return true })
	if n != 0 {
		t.Fatalf("ForEach over a dropped table visited %d rows", n)
	}
}

// TestDropTableDoesNotWaitForReaders pins the deliberate choice not to block: the teardown
// is handed to the last reader out, so a drop returns promptly even with a consumer that
// has stopped consuming and never comes back. Blocking instead would turn one abandoned
// cursor into a stalled catalog -- the catalog lock is held across DropTable, so everything
// else that touches the catalog would stall with it.
func TestDropTableDoesNotWaitForReaders(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	d, err := cat.CreateTable("doomed")
	if err != nil {
		t.Fatal(err)
	}
	fillWide(t, d, 4000)

	seq, err := d.QueryRawProjected("", []string{"Name"}, false)
	if err != nil {
		t.Fatal(err)
	}
	next, stop := iter.Pull(seq)
	if _, ok := next(); !ok {
		t.Fatal("no rows")
	}
	// next() is never called again and stop() is deferred to the end of the test: the
	// reader is parked and holding the gate for the whole of the drop below.
	defer stop()

	done := make(chan error, 1)
	go func() { done <- cat.DropTable("doomed") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("DropTable blocked on a parked reader")
	}
	if _, ok := cat.Table("doomed"); ok {
		t.Fatal("table still in the catalog after DropTable returned")
	}
}

// dropAfterAbandonedScan checks that the gate a scan takes is released when
// the consumer walks away part way -- a break out of a range loop and an iter.Pull the
// caller stops. Both are ordinary client behaviour (a cursor closed after the first page),
// and a gate they leaked would hold the table's mappings for the life of the process.
func dropAfterAbandonedScan(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	d, err := cat.CreateTable("doomed")
	if err != nil {
		t.Fatal(err)
	}
	fillWide(t, d, 4000)

	seq, err := d.QueryRawProjected("", []string{"Name"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for range seq { // abandoned after one row
		break
	}
	next, stop := iter.Pull(seq)
	if _, ok := next(); !ok {
		t.Fatal("no rows")
	}
	stop()

	if err := cat.DropTable("doomed"); err != nil {
		t.Fatal(err)
	}
	// With both gates released the drop tears the table down synchronously, which is what
	// leaves nothing behind to read.
	if n := countSeq(t, d, "Name"); n != 0 {
		t.Fatalf("dropped table yielded %d rows; a gate was leaked by an abandoned scan", n)
	}
}

// dropWithConcurrentReaders runs readers and a drop against each other with no
// orchestration, which is the shape the crash was first seen in: a client with a slow
// consumer and an operator dropping the table. Readers see all the rows or none; what they
// must never do is fault.
func dropWithConcurrentReaders(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	d, err := cat.CreateTable("doomed")
	if err != nil {
		t.Fatal(err)
	}
	fillWide(t, d, 8000)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				n := countSeq(t, d, "Name")
				if n != 0 && n != 8000 {
					t.Errorf("partial scan: %d rows", n)
					return
				}
				d.LookupClassAd("k3")
				d.Len()
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	if err := cat.DropTable("doomed"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// countSeq consumes a projected scan of d and returns the row count.
func countSeq(t *testing.T, d *DB, attr string) int {
	t.Helper()
	seq, err := d.QueryRawProjected("", []string{attr}, false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// fill commits n ads wide enough that the table spans several segments per shard, so a scan
// has somewhere to be when the drop lands.
func fillWide(t *testing.T, d *DB, n int) {
	t.Helper()
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("Name = %q\nX = %d\nPad = %q", fmt.Sprintf("n%d", i), i,
			"0123456789012345678901234567890123456789"))
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
