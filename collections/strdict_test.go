package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// strDictFixture builds a collection whose string values REPEAT within a block, which is what a submit burst
// looks like: a cluster of jobs shares an owner, a command and a working directory.
//
// The ordinary scope fixture does not exercise dictionaries at all -- its Owner is user(i%512) over
// consecutive i, so a 372-record block holds 372 distinct owners and dictWorthwhile correctly declines. A
// correctness test written against that fixture would pass without ever building a dictionary, so the
// dictionary tests use this one and assert the encoding actually happened.
func strDictFixture(tb testing.TB, n, runLen int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd})
	for i := 0; i < n; i++ {
		cluster := i / runLen
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nOwner = \"user%d\"\nCmd = \"/home/user%d/run%d.sh\"\n"+
			"WantCheckpoint = %t\nIwd = \"/scratch/user%d\"",
			cluster, i%runLen, 1+i%5, 1024+(i%32)*512, 1+i%8,
			cluster%8, cluster%8, cluster%4, i%3 == 0, cluster%8)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"ProcId >= 0", "JobStatus >= 0", "RequestMemory >= 0", "RequestCpus >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			tb.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

// dictStats counts how many blocks carry a dictionary for the named attribute.
func dictStats(t *testing.T, c *Collection, attr string) (withDict, blocks int) {
	t.Helper()
	id, ok := c.intern.LookupID(attr)
	if !ok {
		t.Fatalf("no intern id for %s", attr)
	}
	for _, sh := range c.shards {
		_, ws := sh.snapshot()
		for _, w := range ws {
			seg := w.seg.colblk.Load()
			if seg == nil || seg.schema() == nil {
				continue
			}
			idx, ok := seg.schema().byID[id]
			if !ok {
				continue
			}
			for _, blk := range seg.blocks {
				blocks++
				if _, ok := blk.strDict[idx]; ok {
					withDict++
				}
			}
		}
		releaseWindows(ws)
	}
	return withDict, blocks
}

// TestStrDictIsBuiltWhenValuesRepeat pins that the encoding decision goes the way each fixture implies --
// otherwise every other dictionary test could pass without a dictionary existing.
func TestStrDictIsBuiltWhenValuesRepeat(t *testing.T) {
	rep := strDictFixture(t, 20000, 200)
	defer rep.Close()
	got, blocks := dictStats(t, rep, "Owner")
	if blocks == 0 {
		t.Fatal("no blocks")
	}
	if got == 0 {
		t.Errorf("repeating fixture: %d/%d blocks have an Owner dictionary, want most", got, blocks)
	}
	t.Logf("repeating: %d/%d blocks carry an Owner dictionary", got, blocks)

	uniq := scopeFixtureCodec(t, 20000)
	defer uniq.Close()
	gotU, blocksU := dictStats(t, uniq, "Cmd")
	t.Logf("all-distinct: %d/%d blocks carry a Cmd dictionary", gotU, blocksU)
	if gotU > 0 {
		t.Errorf("all-distinct fixture: %d blocks built a Cmd dictionary; dictWorthwhile should decline "+
			"when nothing repeats, since the positional region is still written", gotU)
	}
}

// TestStrDictAgreesWithRowPath runs the string corpus against a fixture that DOES dictionary-encode, so the
// dictionary read path is what answers, and compares against the ordinary row path.
func TestStrDictAgreesWithRowPath(t *testing.T) {
	c := strDictFixture(t, 20000, 200)
	defer c.Close()
	if got, _ := dictStats(t, c, "Owner"); got == 0 {
		t.Fatal("no Owner dictionary; this test would not exercise the dictionary path")
	}
	for _, expr := range []string{
		`Owner == "user3"`,
		`Owner == "USER3"`, // == folds case
		`Owner != "user3"`,
		`Owner =?= "user3"`,
		`Owner =!= "USER3"`, // =?= / =!= do not fold
		`Owner < "user5"`,
		`Owner <= "user3"`,
		`Owner > "user5"`,
		`Owner >= "USER3"`,
		`Owner == "nobody"`, // absent from every dictionary
		`Owner == "user3" || Owner == "user5"`,
		`Owner == "user3" && RequestCpus > 2`,
		`Cmd == "/home/user3/run1.sh"`,
		`Iwd == "/scratch/user3"`,
		`Owner == "user3" || RequestMemory > 8192`,
		`Owner =?= Iwd`, // two string columns against each other
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		got, ok := c.VectorEvalCount(q)
		if !ok {
			t.Fatalf("%q: declined", expr)
		}
		want := rowCount(t, c, expr)
		if got != want {
			t.Errorf("%q: dictionary path %d != row path %d", expr, got, want)
		}
		if split := lastVecSplit.Load(); split.vecBlocks == 0 && split.prunedBlocks == 0 {
			t.Errorf("%q: nothing vectorized or pruned; the comparison is vacuous", expr)
		}
	}
}

// BenchmarkStrDict measures the dictionary against the positional walk on IDENTICAL data, by building the
// same fixture twice with the encoding switched off for one of them.
//
// The values must repeat for a dictionary to be built at all, which is the case this is for: a submit burst
// where a cluster of jobs shares an owner and a command. On all-distinct strings the encoding declines and
// there is nothing to measure.
func BenchmarkStrDict(b *testing.B) {
	build := func(dict bool) *Collection {
		old := strDictEnabled
		strDictEnabled = dict
		defer func() { strDictEnabled = old }()
		return strDictFixture(b, 60000, 200)
	}
	withDict, without := build(true), build(false)
	defer withDict.Close()
	defer without.Close()

	// Assert the two fixtures really do differ in encoding, or this measures nothing.
	id, _ := withDict.intern.LookupID("Owner")
	count := func(c *Collection) int {
		n := 0
		for _, sh := range c.shards {
			_, ws := sh.snapshot()
			for _, w := range ws {
				seg := w.seg.colblk.Load()
				if seg == nil || seg.schema() == nil {
					continue
				}
				if idx, ok := seg.schema().byID[id]; ok {
					for _, blk := range seg.blocks {
						if _, ok := blk.strDict[idx]; ok {
							n++
						}
					}
				}
			}
			releaseWindows(ws)
		}
		return n
	}
	if count(withDict) == 0 {
		b.Fatal("dictionary fixture built no dictionaries")
	}
	if count(without) != 0 {
		b.Fatal("no-dictionary fixture built dictionaries")
	}
	b.Logf("dictionary blocks: with=%d without=%d", count(withDict), count(without))

	for _, expr := range []string{
		`Owner == "user3"`,
		`Owner == "nobody"`, // absent from every dictionary: should prune
		`Owner < "user5"`,
		`Owner == "user3" && RequestCpus > 2`,
		`Cmd == "/home/user3/run1.sh"`,
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		a, _ := withDict.VectorEvalCount(q)
		c, _ := without.VectorEvalCount(q)
		if a != c {
			b.Fatalf("%q: dictionary %d != positional %d", expr, a, c)
		}
		b.Run("dict/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				withDict.VectorEvalCount(q)
			}
		})
		b.Run("positional/"+expr, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				without.VectorEvalCount(q)
			}
		})
	}
}
