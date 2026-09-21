package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// stopFixture builds a collection with a columnarize BACKLOG: the segment budget is set low so
// that enabling the schema scan cannot consume every sealed segment, leaving work for the pass
// under test. Without that the pass has nothing to do and both arms trivially return zero.
func stopFixture(t *testing.T, n int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
		GroupSchemaCount: -1, ColumnarSegmentBudget: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, i%7, i%6, (i%16)*1024))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// A maintenance pass must abandon its remaining work once StopMaintenance is called, at a
// segment boundary. Without this a caller closing the server waits out the whole in-flight
// pass -- bounded at 64 segment rewrites, which is tens of seconds on an archive at history
// scale, and shows up as a process sitting there after its logs say it stopped.
//
// Both arms run the same pass on the same fixture shape, so the only difference is the flag.
// The unstopped arm asserting a non-zero rewrite count is what keeps this from passing
// vacuously: if the fixture stopped producing a backlog, the test fails rather than agreeing
// with itself that zero equals zero.
func TestColumnarizeSealedStopsWhenMaintenanceIsStopped(t *testing.T) {
	pass := func(stop bool) int {
		c := stopFixture(t, 6000)
		defer c.Close()
		if !c.BuildAndEnableSchemaScan(2000, 32) {
			t.Skip("schema scan did not enable; nothing to columnarize")
		}
		if stop {
			c.StopMaintenance()
		}
		return c.ColumnarizeSealed()
	}

	if got := pass(false); got == 0 {
		t.Fatalf("running arm rewrote %d segments: the fixture leaves no backlog, so the stopped arm proves nothing", got)
	}
	if got := pass(true); got != 0 {
		t.Errorf("stopped arm rewrote %d segments, want 0: the pass ran on past StopMaintenance", got)
	}
}

// StopMaintenance is a shutdown signal, not a pause: a collection that has been told to stop
// must stay stopped, or a later maintenance tick would start a fresh pass during the close it
// was meant to get out of the way of.
func TestStopMaintenanceIsSticky(t *testing.T) {
	c := stopFixture(t, 200)
	defer c.Close()
	if c.maintStopping() {
		t.Fatal("a fresh collection reports maintenance stopped")
	}
	c.StopMaintenance()
	if !c.maintStopping() {
		t.Fatal("StopMaintenance did not take effect")
	}
	c.StopMaintenance()
	if !c.maintStopping() {
		t.Error("a second StopMaintenance cleared the flag")
	}
}

// The guard that matters is the one INSIDE the pass. A stop set before the pass starts is
// caught by the check at the top of the shard loop, so a test that only does that leaves the
// per-segment check unproven -- deleting it still passes. Here the stop arrives mid-pass, via
// the commit seam, which is exactly how it arrives in production: a SIGTERM lands while a
// bounded-but-long rewrite is already underway.
//
// With the per-segment check, the pass finishes the segment it is committing (it must -- the
// close that follows unmaps what a rewrite is reading) and then stops, so exactly one segment
// is rewritten. Without it, the pass runs out its whole budget.
func TestColumnarizeSealedStopsMidPass(t *testing.T) {
	const budget = 4

	run := func(stopOnFirst bool) int {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
			GroupSchemaCount: -1, ColumnarSegmentBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for i := range 12000 {
			ad, perr := classad.Parse(fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
				i, i%10, i%7, i%6, (i%16)*1024))
			if perr != nil {
				t.Fatal(perr)
			}
			if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); perr != nil {
				t.Fatal(perr)
			}
		}
		if !c.BuildAndEnableSchemaScan(2000, 32) {
			t.Skip("schema scan did not enable")
		}
		if stopOnFirst {
			n := 0
			colCommitStallHook = func() {
				if n++; n == 1 {
					c.StopMaintenance()
				}
			}
			defer func() { colCommitStallHook = nil }()
		}
		return c.ColumnarizeSealed()
	}

	full := run(false)
	if full < 2 {
		t.Fatalf("uninterrupted pass rewrote %d segments; need at least 2 for a mid-pass stop to be distinguishable", full)
	}
	if got := run(true); got != 1 {
		t.Errorf("stopped mid-pass rewrote %d segments, want 1 (the in-flight one, then stop); uninterrupted rewrites %d", got, full)
	}
}

// recolumnarizeStaleGroups is the loop the production hang was actually in: the stack from a
// SIGQUIT during shutdown ran StartMaintenance -> maintainArchives -> BuildAndEnableSchemaScan
// -> refreshGroupSchemas -> recolumnarizeStaleGroups -> columnarizeSealedSegment. It is a
// second copy of ColumnarizeSealed's loop shape, so its guards need their own coverage -- the
// tests above pass with this function's checks deleted.
//
// The segments here carry no group set (GroupSchemaCount is off), so any non-empty set is
// stale for all of them and every columnarized segment becomes a candidate.
func TestRecolumnarizeStaleGroupsStopsMidPass(t *testing.T) {
	const budget = 4

	run := func(stopOnFirst bool) int {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
			GroupSchemaCount: -1, ColumnarSegmentBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for i := range 12000 {
			ad, perr := classad.Parse(fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
				i, i%10, i%7, i%6, (i%16)*1024))
			if perr != nil {
				t.Fatal(perr)
			}
			if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); perr != nil {
				t.Fatal(perr)
			}
		}
		if !c.BuildAndEnableSchemaScan(2000, 32) {
			t.Skip("schema scan did not enable")
		}
		// Build up a population of columnarized segments for the rewrite to find.
		for range 4 {
			c.ColumnarizeSealed()
		}
		st := c.schemaScan.Load()
		if st == nil || st.schema == nil {
			t.Skip("no schema published")
		}
		if stopOnFirst {
			n := 0
			colCommitStallHook = func() {
				if n++; n == 1 {
					c.StopMaintenance()
				}
			}
			defer func() { colCommitStallHook = nil }()
		}
		return c.recolumnarizeStaleGroups([]*colGroup{{schema: st.schema, ids: []uint32{0, 1}}})
	}

	full := run(false)
	if full < 2 {
		t.Fatalf("uninterrupted rewrite touched %d segments; need at least 2 to tell a mid-pass stop apart", full)
	}
	if got := run(true); got != 1 {
		t.Errorf("stopped mid-pass rewrote %d segments, want 1; uninterrupted rewrites %d", got, full)
	}
}
