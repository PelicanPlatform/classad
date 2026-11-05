package db

import "testing"

// BenchmarkJoinConstrainedSide measures the shape this cost model is for: a join whose
// hashed side carries a constraint of its own.
//
// The constraint names an attribute no index covers, so the side's read visits every record
// either way and the whole table is the honest price of scanning it. The membership
// constraint the probe builds, by contrast, names the join key, which IS indexed -- so
// narrowing turns that full scan into a handful of index lookups. The decision was there to
// be made and the old gate could not make it: unable to price a constrained side, it
// declined every one of them.
//
// Arms alternate, because a fixed order lets drift between the first and second half of the
// run land entirely on one of them.
func BenchmarkJoinConstrainedSide(b *testing.B) {
	cat := probeFixture(b, 65536, 32, true)
	spec := probeSpec(JoinInner)
	spec.Right.Constraint = "ExitCode == 0"

	run := func(b *testing.B, unpriced bool) {
		forceUnpricedSides = unpriced
		defer func() { forceUnpricedSides = false }()
		var probed, declined int
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var st JoinStats
			seq, err := cat.Join(spec, JoinLimits{}, &st)
			if err != nil {
				b.Fatal(err)
			}
			for range seq {
			}
			if st.ProbeKeys > 0 {
				probed++
			} else {
				declined++
			}
		}
		b.StopTimer()
		// Assert the arm did what its name says. A benchmark of two paths that silently
		// ran the same one is the failure mode that reports a clean 1.00x and means
		// nothing.
		switch {
		case unpriced && probed > 0:
			b.Fatalf("the unpriced arm probed %d of %d joins", probed, b.N)
		case !unpriced && declined > 0:
			b.Fatalf("the priced arm declined %d of %d joins", declined, b.N)
		}
	}

	for _, r := range []string{"r0", "r1"} {
		arms := []string{"priced", "unpriced"}
		if r == "r1" {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, arm := range arms {
			b.Run(r+"/"+arm, func(b *testing.B) { run(b, arm == "unpriced") })
		}
	}
}

// The priced arm has to produce exactly the rows the unpriced one does. An index probe that
// narrowed a side too far would be faster and wrong, and nothing else here would notice.
//
// The disjunctive case is about the parentheses in andConstraints. Conjoined without them,
// `a || b && (keys)` binds the && tighter and admits every row with a, whatever key it
// carries. That does NOT change the answer -- the extra rows land in hash buckets no
// streamed row probes -- which is exactly why it needs its own assertion: it defeats the
// narrowing completely, puts the whole table back in the hash, and reports an index probe
// while doing it. HashRows is what moves, so HashRows is what this checks.
func TestJoinConstrainedSideRowsAreUnchanged(t *testing.T) {
	for _, own := range []string{
		"ExitCode == 0",
		"ExitCode == 0 || ExitCode == 3",
		"ExitCode >= 0", // matches everything: the probe must still not widen the side
	} {
		t.Run(own, func(t *testing.T) { constrainedSideRowsAreUnchanged(t, own) })
	}
}

func constrainedSideRowsAreUnchanged(t *testing.T, own string) {
	cat := probeFixture(t, 4096, 16, true)
	spec := probeSpec(JoinInner)
	spec.Right.Constraint = own

	collect := func(unpriced bool) ([]string, JoinStats) {
		forceUnpricedSides = unpriced
		defer func() { forceUnpricedSides = false }()
		var st JoinStats
		seq, err := cat.Join(spec, JoinLimits{}, &st)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for ad := range seq {
			out = append(out, ad.String())
		}
		return out, st
	}
	priced, pst := collect(false)
	unpriced, ust := collect(true)
	if pst.ProbeKeys == 0 {
		t.Fatalf("the priced arm did not probe: %s", pst.ProbeWhy)
	}
	if ust.ProbeKeys != 0 {
		t.Fatal("the unpriced arm probed")
	}
	if len(priced) != len(unpriced) {
		t.Fatalf("priced returned %d rows, unpriced %d", len(priced), len(unpriced))
	}
	for i := range priced {
		if priced[i] != unpriced[i] {
			t.Fatalf("row %d differs:\n priced: %s\n  plain: %s", i, priced[i], unpriced[i])
		}
	}
	if len(priced) == 0 {
		t.Fatal("the fixture produced no rows, so the two arms agreed on nothing")
	}
	// The streamed side names 16 keys, so a narrowed hash holds at most those 16 rows. A
	// hash much larger than that means the membership constraint did not narrow the side,
	// whatever the probe statistics say.
	if pst.HashRows > 16 {
		t.Errorf("the narrowed hash holds %d rows against 16 keys: the membership "+
			"constraint did not narrow the side", pst.HashRows)
	}
}
