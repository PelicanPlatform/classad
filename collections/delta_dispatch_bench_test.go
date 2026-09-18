package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// WHAT DOES DELTA MODE COST A READER?
//
// A delta record holds only what one write changed, so a reader has to find out whether the
// record in front of it is one. The first implementation answered that from the record's
// PAYLOAD, which meant decompressing it -- every record, on every scan, whether or not any
// record in the store was a delta. The cost showed up as a ~12% read regression on the ingest
// benchmark, with no explanation attached to it.
//
// These benchmarks attach one. They hold the store, the data and the process fixed and vary
// only the dispatch (deltaHdrDispatch), because that is the single thing the change alters --
// comparing two builds would compare two machine states as well, which on a loaded machine is
// most of the measurement.
//
// Three stores, because the interesting cases are not just "deltas on" and "deltas off":
//
//	plain    -- DeltaMax 0: no delta machinery at all. The floor.
//	sparse   -- delta mode on, but only ONE key ever updated: 4999 of 5000 records are whole.
//	            This is the case that mattered and the one nobody was measuring. A reader pays
//	            the classification on every record and almost no record benefits -- and it is
//	            not a contrived shape, it is a mirror of a queue whose rows are mostly idle.
//	chains   -- every key updated, so every current record is a delta. Classification here is
//	            work that has to happen; the question is only what it costs.
//
// The store must be PERSISTENT. Delta records need inline-name encoding, so an in-memory
// collection with DeltaMax set quietly stores whole ads -- the first version of this benchmark
// did exactly that and measured two identical configurations to four significant figures.

// deltaBenchStore builds a persistent collection of n wide ads, then applies `rounds` patch
// updates to the first `hot` of them. rounds == 0 leaves every record whole.
func deltaBenchStore(b *testing.B, deltaMax, n, hot, rounds int) *Collection {
	b.Helper()
	c, err := Open(Options{Dir: b.TempDir(), Shards: 4, SegmentSize: 1 << 20, DeltaMax: deltaMax})
	if err != nil {
		b.Fatal(err)
	}
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("%d.0", i))
	}
	tx := c.Begin()
	for i, k := range keys {
		tx.Put(k, jobAd(i, map[string]int64{"LastJobLeaseRenewal": 100}))
	}
	if r := tx.Commit(); r.Conflicted() {
		b.Fatal("seed conflicted")
	}
	for r := 1; r <= rounds; r++ {
		for _, k := range keys[:hot] {
			patch := classad.New()
			patch.InsertAttr("LastJobLeaseRenewal", int64(100+r))
			tx := c.Begin()
			tx.PatchAttrs(k, patch, nil)
			if res := tx.Commit(); res.Conflicted() {
				b.Fatal("patch conflicted")
			}
		}
	}
	// A benchmark of delta dispatch that stored no deltas would compare a configuration with
	// itself, which is what the first version of this did. Assert the store is the shape the
	// name claims before timing anything.
	deltas, fulls := c.DeltaStats()
	if deltaMax > 0 && rounds > 0 && deltas == 0 {
		b.Fatalf("no delta records were stored (%d full): the benchmark is measuring nothing", fulls)
	}
	if deltaMax > 0 && rounds > 0 && !c.deltaRead {
		b.Fatal("delta replay is off: the read path under test is not reached")
	}
	b.Logf("store: %d delta records, %d whole, deltaRead=%v", deltas, fulls, c.deltaRead)
	return c
}

// benchDispatch runs one store through BOTH dispatches as sub-benchmarks, so the pair differs
// only in the dispatch. Giving each dispatch its own benchmark function gave each its own store
// -- its own TempDir, mmap layout and segment count -- and that variance was large enough to
// invert one of the four comparisons.
func benchDispatch(b *testing.B, deltaMax, hot, rounds int) {
	c := deltaBenchStore(b, deltaMax, 5000, hot, rounds)
	defer c.Close()
	keys := make([][]byte, 5000)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("%d.0", i))
	}
	for _, mode := range []struct {
		name string
		hdr  bool
	}{{"hdr", true}, {"payload", false}} {
		b.Run("scan/"+mode.name, func(b *testing.B) {
			restore := setDispatch(mode.hdr)
			defer restore()
			want := checkScan(b, c)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got := 0
				for ad := range c.Scan() {
					if ad != nil {
						got++
					}
				}
				if got != want {
					b.Fatalf("scan saw %d ads, want %d", got, want)
				}
			}
		})
		b.Run("get/"+mode.name, func(b *testing.B) {
			restore := setDispatch(mode.hdr)
			defer restore()
			checkGet(b, c, keys[7])
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := c.Get(keys[i%len(keys)]); !ok {
					b.Fatal("missing")
				}
			}
		})
	}
}

func setDispatch(hdr bool) func() {
	old := deltaHdrDispatch
	deltaHdrDispatch = hdr
	return func() { deltaHdrDispatch = old }
}

// checkScan walks the store once and fails if any ad came back as a fragment. Correctness is
// not incidental to this measurement: a dispatch that misclassified would be FASTER, because it
// would stop merging, so the benchmark has to establish that it is timing two right answers.
func checkScan(b *testing.B, c *Collection) int {
	b.Helper()
	n := 0
	for ad := range c.Scan() {
		if v, ok := ad.EvaluateAttrInt("Pad39"); !ok || v != 39 {
			b.Fatalf("scan served a fragment: Pad39=%d ok=%v", v, ok)
		}
		n++
	}
	if n != 5000 {
		b.Fatalf("scan saw %d ads, want 5000", n)
	}
	return n
}

func checkGet(b *testing.B, c *Collection, key []byte) {
	b.Helper()
	ad, ok := c.Get(key)
	if !ok {
		b.Fatal("precondition: key missing")
	}
	if v, ok2 := ad.EvaluateAttrInt("Pad39"); !ok2 || v != 39 {
		b.Fatalf("point read served a fragment: Pad39=%d ok=%v", v, ok2)
	}
}

// BenchmarkDeltaPlain is the floor: no delta machinery at all. Its two sub-benchmarks are
// identical by construction (the dispatch is unreachable with deltaRead off), which makes the
// pair a read on this machine's noise for the runs that follow.
func BenchmarkDeltaPlain(b *testing.B)  { benchDispatch(b, 0, 0, 0) }
func BenchmarkDeltaSparse(b *testing.B) { benchDispatch(b, 16, 1, 8) }
func BenchmarkDeltaChains(b *testing.B) { benchDispatch(b, 16, 5000, 8) }

// benchIngest measures the WRITE path, which is where classifying a record without decompressing
// it has a reason to matter that reads do not: every time a segment seals, collapseSealedChains
// walks every live record in it looking for chains still open (liveDeltaKeys). Under the payload
// dispatch that walk decompressed every one of them -- tens of thousands of records per seal, to
// find the handful of keys that need collapsing.
//
// Small segments so the run seals many times; that is the shape being measured, not an accident.
func benchIngest(b *testing.B, hdr bool) {
	restore := setDispatch(hdr)
	defer restore()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c, err := Open(Options{Dir: b.TempDir(), Shards: 4, SegmentSize: 1 << 18, DeltaMax: 16})
		if err != nil {
			b.Fatal(err)
		}
		keys := make([][]byte, 2000)
		for j := range keys {
			keys[j] = []byte(fmt.Sprintf("%d.0", j))
		}
		tx := c.Begin()
		for j, k := range keys {
			tx.Put(k, jobAd(j, map[string]int64{"LastJobLeaseRenewal": 100}))
		}
		if r := tx.Commit(); r.Conflicted() {
			b.Fatal("seed conflicted")
		}
		b.StartTimer()
		for r := 1; r <= 12; r++ {
			for _, k := range keys {
				patch := classad.New()
				patch.InsertAttr("LastJobLeaseRenewal", int64(100+r))
				tx := c.Begin()
				tx.PatchAttrs(k, patch, nil)
				if res := tx.Commit(); res.Conflicted() {
					b.Fatal("patch conflicted")
				}
			}
		}
		b.StopTimer()
		deltas, fulls := c.DeltaStats()
		if deltas == 0 {
			b.Fatalf("no deltas stored (%d full): measuring nothing", fulls)
		}
		c.Close()
		b.StartTimer()
	}
}

func BenchmarkDeltaIngestHdr(b *testing.B)     { benchIngest(b, true) }
func BenchmarkDeltaIngestPayload(b *testing.B) { benchIngest(b, false) }

// TestSealWalkWork reports how many records the seal-time collapse walk examines versus how many
// of them are open chains. It is the measurement that decides whether classifying a record from
// its header rather than its payload is worth anything on the WRITE path: under the payload
// dispatch, every record examined cost a decompression, and only the open chains needed one.
//
// A wall-clock A/B could not settle this -- the ingest benchmark's two arms overlapped by more
// than their difference on this machine -- so count the work instead of timing it.
func TestSealWalkWork(t *testing.T) {
	sealWalkRecords.Store(0)
	sealWalkDeltas.Store(0)
	c, err := Open(Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 18, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	keys := make([][]byte, 2000)
	for j := range keys {
		keys[j] = []byte(fmt.Sprintf("%d.0", j))
	}
	tx := c.Begin()
	for j, k := range keys {
		tx.Put(k, jobAd(j, map[string]int64{"LastJobLeaseRenewal": 100}))
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	for r := 1; r <= 12; r++ {
		for _, k := range keys {
			patch := classad.New()
			patch.InsertAttr("LastJobLeaseRenewal", int64(100+r))
			tx := c.Begin()
			tx.PatchAttrs(k, patch, nil)
			if res := tx.Commit(); res.Conflicted() {
				t.Fatal("patch conflicted")
			}
		}
	}
	walked, open := sealWalkRecords.Load(), sealWalkDeltas.Load()
	deltas, fulls := c.DeltaStats()
	if deltas == 0 {
		t.Fatalf("no deltas stored (%d full): measuring nothing", fulls)
	}
	if walked == 0 {
		t.Fatal("the seal walk never ran: no segment sealed, so this test measures nothing")
	}
	t.Logf("writes: %d deltas, %d whole", deltas, fulls)
	t.Logf("seal walk: examined %d live records, %d were open chains (%.1f%%)",
		walked, open, 100*float64(open)/float64(walked))
	t.Logf("payload dispatch would decompress %d records to find %d; header dispatch decompresses 0",
		walked, open)
}

// TestDispatchesAgree is the reason the payload dispatch is still compiled. Every record in a
// delta store carries the fact that it is a delta twice -- once in the record header, once in the
// compressed payload -- and production reads only the first. This walks a store that mixes whole
// records, short chains and re-materialized chains under BOTH dispatches and requires the same
// answer for every key, on both the point-read and the scan path.
//
// An assertion that the two flags agree (delta_test.go's deltaClassify) checks the encodings; this
// checks the consequence, which is what a copy path that loses a flag actually breaks.
func TestDispatchesAgree(t *testing.T) {
	c, _ := openDelta(t, 4)
	defer c.Close()
	keys := make([][]byte, 300)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("%d.0", i))
	}
	tx := c.Begin()
	for i, k := range keys {
		tx.Put(k, jobAd(i, map[string]int64{"LastJobLeaseRenewal": 100}))
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	// Uneven update counts, so the store holds whole records, chains of every depth up to the
	// bound, and chains that have crossed it and been re-materialized.
	for i, k := range keys {
		for r := 1; r <= i%11; r++ {
			patch := classad.New()
			patch.InsertAttr("LastJobLeaseRenewal", int64(100+r))
			tx := c.Begin()
			tx.PatchAttrs(k, patch, nil)
			if res := tx.Commit(); res.Conflicted() {
				t.Fatal("patch conflicted")
			}
		}
	}
	if deltas, fulls := c.DeltaStats(); deltas == 0 {
		t.Fatalf("no deltas stored (%d full): the comparison is vacuous", fulls)
	}

	// render collapses a store to a comparable string: every key, every attribute, sorted.
	render := func(hdr bool) (points, scans string) {
		restore := setDispatch(hdr)
		defer restore()
		var pb strings.Builder
		for _, k := range keys {
			ad, ok := c.Get(k)
			if !ok {
				pb.WriteString(string(k) + ":MISSING\n")
				continue
			}
			pb.WriteString(string(k) + ":" + adDigest(ad) + "\n")
		}
		var lines []string
		for ad := range c.Scan() {
			lines = append(lines, adDigest(ad))
		}
		sort.Strings(lines)
		return pb.String(), strings.Join(lines, "\n")
	}
	hp, hs := render(true)
	pp, ps := render(false)
	if hp != pp {
		t.Error("point reads disagree between the header and payload dispatches")
		for i, line := range strings.Split(hp, "\n") {
			other := strings.Split(pp, "\n")
			if i < len(other) && line != other[i] {
				t.Fatalf("first difference:\n  header:  %s\n  payload: %s", line, other[i])
			}
		}
	}
	if hs != ps {
		t.Fatal("scans disagree between the header and payload dispatches")
	}
	if len(hs) == 0 {
		t.Fatal("the scan produced nothing: the comparison is vacuous")
	}
}

// adDigest renders an ad as its sorted old-ClassAd text, so two ads compare equal only if they
// hold the same attributes with the same unparsed values. Sorted because the comparison is about
// content, and attribute order is not content.
func adDigest(ad *classad.ClassAd) string {
	lines := strings.Split(strings.TrimSpace(ad.MarshalOld()), "\n")
	sort.Strings(lines)
	return strings.Join(lines, "|")
}
