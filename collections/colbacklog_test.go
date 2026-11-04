package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSchemaScanInfoReportsTheColumnarizeBacklog pins down the distinction between the two coverage
// numbers, which is the whole reason ColumnarSegments is reported at all.
//
// CoveredSegments counts sealed segments carrying a sidecar columnar block. That block is built on
// the ordinary schema scan for every sealed segment at once, so coverage goes to 100% immediately
// and says nothing about how the records are stored. The columnar-NATIVE rewrite -- the one that
// moves each schema'd attribute out of every record and into the segment's own payload -- is bounded
// by ColumnarSegmentBudget per maintenance pass, so it lags, sometimes by a lot.
//
// Reading CoveredSegments as "this table is columnar now" is therefore wrong, and wrong in the
// direction that hides a problem: a benchmark guarded on CoveredSegments >= SealedSegments ran over
// a fixture whose segments were two thirds row-native and reported it as a columnar measurement.
// The assertions below hold the two numbers apart at the moment they disagree.
func TestSchemaScanInfoReportsTheColumnarizeBacklog(t *testing.T) {
	const budget = 4
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 12,
		ColumnarSegmentBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Small segments and enough ads to fill many of them, so the backlog outlasts one pass.
	const n = 4000
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; `+
				`RequestCpus=%d; QDate=%d; RemoteHost="slot%d@host%d.example" ]`,
			i, i%10, i%37, 1+i%5, (i%16)*1024, 1+i%8, 1600000000+i, i%64, i%128))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// The ordinary maintenance pass: derive and enable on the first call, then columnarize a
	// budget's worth and (re)cover every sealed segment on each one after.
	pass := func() { c.BuildAndEnableSchemaScan(4096, 12) }
	if !c.BuildAndEnableSchemaScan(4096, 12) {
		t.Fatal("could not derive and install a schema; the fixture has no accelerator to report on")
	}

	info := c.SchemaScanInfo()
	// Premise: there has to be more backlog than one pass can clear, or the two numbers would
	// agree for an uninteresting reason and the test would prove nothing.
	if info.SealedSegments <= budget {
		t.Fatalf("fixture holds %d sealed segments, not more than the %d-segment budget: one pass "+
			"would clear the whole backlog and the numbers could not disagree",
			info.SealedSegments, budget)
	}

	passes := 0
	for {
		info = c.SchemaScanInfo()
		if info.ColumnarSegments >= info.SealedSegments {
			break
		}
		// Coverage is complete from the first scan onward, while the native rewrite is still
		// working through the backlog. This is the disagreement the field exists to expose.
		if info.CoveredSegments != info.SealedSegments {
			t.Fatalf("pass %d: covered %d of %d sealed; sidecar coverage is unbounded and should "+
				"already be complete", passes, info.CoveredSegments, info.SealedSegments)
		}
		if info.ColumnarSegments >= info.CoveredSegments {
			t.Fatalf("pass %d: %d columnar-native of %d covered; the loop has not exited, so the "+
				"backlog is non-empty and the two counts must differ",
				passes, info.ColumnarSegments, info.CoveredSegments)
		}
		before := info.ColumnarSegments
		pass()
		if after := c.SchemaScanInfo().ColumnarSegments; after == before {
			t.Fatalf("pass %d: rewrote nothing with %d segments still row-native",
				passes, info.SealedSegments-before)
		}
		passes++
		if passes > n {
			t.Fatal("columnarization never converged")
		}
	}

	// Premise: it took more than one pass, so the backlog was observed across passes rather than
	// in a single before/after snapshot.
	if passes < 2 {
		t.Fatalf("converged in %d pass(es); the budget was meant to spread the rewrite over "+
			"several, so nothing was held apart over time", passes)
	}
	if info.ColumnarSegments != info.SealedSegments {
		t.Fatalf("converged with %d columnar-native of %d sealed",
			info.ColumnarSegments, info.SealedSegments)
	}
	t.Logf("%d sealed segments converged over %d passes at a %d-segment budget",
		info.SealedSegments, passes, budget)
}
