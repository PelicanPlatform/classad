package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// scanMatches is the reference answer an indexed query must reproduce: every ad the plain scan
// yields that the query matches.
func scanMatches(t *testing.T, c *Collection, q *vm.Query) []string {
	t.Helper()
	var out []string
	for ad := range c.Scan() {
		if q.Matches(ad) {
			out = append(out, adSig(ad))
		}
	}
	return out
}

// TestIndexedQueryFindsDeltaRecords: a delta record posts only the attributes its write changed,
// so the index has no posting for what it inherits. An indexed read has to visit it anyway, or a
// key whose current version is a delta drops out of every indexed answer.
func TestIndexedQueryFindsDeltaRecords(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 14, DeltaMax: 16,
		ValueAttrs: []string{"ClusterId", "N"}, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 200
	for i := 0; i < n; i++ {
		ad := jobAd(i, map[string]int64{"N": 0})
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%4))
		if err := c.Put([]byte(fmt.Sprintf("k%04d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for round := 1; round <= 3; round++ {
		for i := 0; i < n; i += 1 + round%2 {
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("k%04d.0", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
		}
		c.Reindex() // index each round's segments, so deltas sit in both sealed and active prefixes
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	for _, expr := range []string{
		`ClusterId >= 0`,
		`ClusterId == 7`,
		`ClusterId < 50 && ClusterId > 10`,
		`Owner == "user1"`,
		`Owner =?= "user2"`,
		`Owner != "user3"`,
		`N == 3`,
		`ClusterId == 3 || Owner == "user1"`, // the disjunctive candidate path
		`ClusterId > 100000`,                 // a prefix the index skips outright
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.planIndex(q.Probes())) == 0 && len(q.ProbePlan()) < 2 {
			t.Fatalf("%s: not index-planned; the test would not reach the candidate walk", expr)
		}
		sigsEqual(t, "Query("+expr+")", collectQuery(t, c, expr), scanMatches(t, c, q))
	}
}

// TestIndexedIsProbeAdmitsExpressions: `=?=` against a categorical index has to visit the records
// whose value is an expression, not a string literal, since one can still evaluate to the probe's
// string.
func TestIndexedIsProbeAdmitsExpressions(t *testing.T) {
	t.Parallel()
	for _, persistent := range []bool{false, true} {
		opts := Options{Shards: 1, SegmentSize: 1 << 12, CategoricalAttrs: []string{"Owner"}}
		if persistent {
			opts.Dir = t.TempDir()
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100; i++ {
			src := fmt.Sprintf(`[Id=%d; Owner = strcat("al", "ice")]`, i)
			if i%2 == 0 {
				src = fmt.Sprintf(`[Id=%d; Owner = "alice"]`, i)
			}
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, src)); err != nil {
				t.Fatal(err)
			}
		}
		c.Reindex()
		for _, expr := range []string{`Owner =?= "alice"`, `Owner =?= "Alice"`, `Owner =!= "alice"`} {
			q, err := vm.Parse(expr)
			if err != nil {
				t.Fatal(err)
			}
			sigsEqual(t, fmt.Sprintf("persistent=%v Query(%s)", persistent, expr), collectQuery(t, c, expr), scanMatches(t, c, q))
		}
		c.Close()
	}
}

// TestIndexedQueryDeltaBaseUnindexed: a delta that does not carry the probed attribute inherits it
// from an earlier record of its key, and is kept as a candidate when that record is posted as a
// match. With the earlier record in a segment no index covers there is no posting to find, so the
// delta has to be admitted outright.
func TestIndexedQueryDeltaBaseUnindexed(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, DeltaMax: 8,
		ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 200
	for i := 0; i < n; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("k%04d.0", i)), jobAd(i, nil)); perr != nil {
			t.Fatal(perr)
		}
	}
	// Patch a few keys whose whole records sealed into older segments: the deltas land in the
	// active segment, their bases do not.
	sh := c.shards[0]
	sh.mu.RLock()
	if len(sh.segs) < 3 {
		sh.mu.RUnlock()
		t.Fatal("the records did not span segments")
	}
	sh.mu.RUnlock()
	for i := 0; i < 5; i++ {
		patch := classad.New()
		patch.InsertAttr("N", int64(i))
		w := c.Begin()
		w.PatchAttrs([]byte(fmt.Sprintf("k%04d.0", i)), patch, nil)
		if r := w.Commit(); r.Conflicted() {
			t.Fatal("patch conflicted")
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written")
	}
	// Index the active segment only, so its deltas are served by an index while their bases are
	// in sealed segments that have none.
	sh.mu.RLock()
	act, used := sh.act, sh.act.used
	sh.mu.RUnlock()
	act.idx.Store(buildSegIndex(c, act, used, c.spec.Load()))
	for i := 0; i < 5; i++ {
		expr := fmt.Sprintf(`ClusterId == %d`, i)
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		sigsEqual(t, "Query("+expr+")", collectQuery(t, c, expr), scanMatches(t, c, q))
		seq, _ := c.QueryRawFromSeq(q, nil, SeqCursor{}, 0)
		rows := 0
		for range seq {
			rows++
		}
		if rows != 1 {
			t.Errorf("QueryRawFromSeq(%s) returned %d rows, want 1", expr, rows)
		}
	}
}
