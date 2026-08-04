package collections

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// earlyOutCorpus is a shape where the early-out matters: a selective categorical attribute
// alongside a unique-per-record timestamp, so an `Owner == x && CompletionDate > t` query
// leaves a few hundred candidates after the first probe and would merge most of the segment
// for the second.
func earlyOutCorpus(tb testing.TB, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 4, CategoricalAttrs: []string{"Owner"},
		ValueAttrs: []string{"Memory", "CompletionDate"}})
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(tb, fmt.Sprintf(
			`[ ID=%d; Owner="u%d"; Memory=%d; CompletionDate=%d ]`,
			i, i%500, (i%64+1)*128, 1700000000+i))); err != nil {
			tb.Fatal(err)
		}
	}
	c.Reindex()
	return c
}

// TestWorthProbing pins the cost bound itself.
func TestWorthProbing(t *testing.T) {
	t.Parallel()
	// A probe cheaper than the verify work it could save is always applied.
	if !worthProbing(10, 100) {
		t.Error("a cheap probe against a large accumulator must be applied")
	}
	// A probe that would merge more than the accumulator could ever cost to verify is
	// dropped -- it cannot pay off even if it eliminated every candidate.
	if worthProbing(100000, 10) {
		t.Error("a probe costlier than the whole remaining verify must be dropped")
	}
	// Exactly at the bound: not worth it (strict less-than).
	if worthProbing(float64(50*offsetsPerVerify), 50) {
		t.Error("at the break-even point the probe must not be applied")
	}
	// An empty accumulator can never justify any probe, but the caller never asks: the
	// intersection returns as soon as acc is empty.
	if worthProbing(1, 0) {
		t.Error("nothing is worth probing against an empty accumulator")
	}
}

// TestEarlyOutMatchesFullIntersection is the correctness anchor: dropping probes only widens
// a superset the store re-verifies, so results must be identical to a full scan's.
func TestEarlyOutMatchesFullIntersection(t *testing.T) {
	t.Parallel()
	c := earlyOutCorpus(t, 40000)
	for _, qs := range []string{
		`Owner == "u7" && CompletionDate > 1700000000`,
		`Owner == "u7" && Memory > 256 && CompletionDate > 1700000000`,
		`Owner == "u7" && Memory > 8000`,
		`Owner == "u7" && CompletionDate > 1700000000 && CompletionDate < 1700039999`,
		`Owner == "nobody" && CompletionDate > 1700000000`,
		`Memory > 8000 && CompletionDate > 1700039000`,
	} {
		q := mustQuery(t, qs)
		indexed := 0
		for range c.Query(q) {
			indexed++
		}
		brute := 0
		for ad := range c.Scan() {
			if b, err := q.Eval(ad).BoolValue(); err == nil && b {
				brute++
			}
		}
		if indexed != brute {
			t.Errorf("%s: indexed returned %d ads, full scan %d", qs, indexed, brute)
		}
	}
}

// TestEarlyOutTierParity checks that both index tiers make the SAME skip decisions. They
// price a probe from segStats, which the sidecar carries verbatim, so they must -- if they
// ever diverged, a sealed segment would answer with a different candidate set than the same
// data in RAM.
func TestEarlyOutTierParity(t *testing.T) {
	t.Parallel()
	c := earlyOutCorpus(t, 20000)
	queries := []string{
		`Owner == "u7" && CompletionDate > 1700000000`,
		`Owner == "u7" && Memory > 256 && CompletionDate > 1700000000`,
		`Memory > 8000 && CompletionDate > 1700019000`,
	}
	segs := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			live := seg.idx.Load()
			if live == nil {
				continue
			}
			path := filepath.Join(t.TempDir(), fmt.Sprintf("seg-%d.idx", seg.id))
			if err := writeSidecarIndex(path, live); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
			data, closer, err := mapFile(path)
			if err != nil {
				t.Fatalf("map sidecar: %v", err)
			}
			mm, err := parseMmapSidecar(data)
			if err != nil {
				t.Fatalf("parse sidecar: %v", err)
			}
			for _, qs := range queries {
				q, err := vm.Parse(qs)
				if err != nil {
					t.Fatalf("parse %q: %v", qs, err)
				}
				u := c.planIndex(q.Probes())
				if !bmEqual(live.candidateOffsets(u), mm.candidateOffsets(u)) {
					t.Errorf("seg %d %q: tiers disagree on the candidate set", seg.id, qs)
				}
			}
			_ = closer()
			segs++
		}
	}
	if segs == 0 {
		t.Fatal("no segments compared")
	}
}

// BenchmarkIntersectEarlyOut measures the shape the early-out targets: a selective probe
// followed by one that would merge most of the segment to remove a handful of candidates.
func BenchmarkIntersectEarlyOut(b *testing.B) {
	c := earlyOutCorpus(b, 200000)
	for _, qs := range []string{
		`Owner == "u7" && CompletionDate > 1700000000`,
		`Owner == "u7" && Memory > 256 && CompletionDate > 1700000000`,
	} {
		q := mustQuery(b, qs)
		b.Run(qs, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for range c.Query(q) {
				}
			}
		})
	}
}
