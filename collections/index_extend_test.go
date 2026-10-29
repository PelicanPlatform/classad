package collections

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// recordEnds returns the offset just past each record of seg's first used bytes, in order: the
// points an index may stop at.
func recordEnds(seg *segment, used int) []int {
	var ends []int
	for off := 0; off < used; {
		total := int(recTotalLen(seg.data, uint32(off)))
		if total == 0 {
			break
		}
		off += total
		ends = append(ends, off)
	}
	return ends
}

func bitmapsEqual(a, b *roaring.Bitmap) bool {
	if a == nil || b == nil {
		return (a == nil || a.IsEmpty()) && (b == nil || b.IsEmpty())
	}
	return a.Equals(b)
}

func postingsEqual[K comparable](t *testing.T, what string, got, want map[K]*roaring.Bitmap) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d keys, want %d", what, len(got), len(want))
	}
	for k, bm := range want {
		if !bitmapsEqual(got[k], bm) {
			t.Errorf("%s[%v]: %v, want %v", what, k, got[k], bm)
		}
	}
}

// segIndexesEqual fails t unless got and want post the same records under the same keys and carry
// the same summary stats -- everything a query or a sealed sidecar reads from an index.
func segIndexesEqual(t *testing.T, got, want *segIndex) {
	t.Helper()
	if got.upto != want.upto || got.specGen != want.specGen {
		t.Fatalf("upto/specGen %d/%d, want %d/%d", got.upto, got.specGen, want.upto, want.specGen)
	}
	if !bitmapsEqual(got.all, want.all) {
		t.Errorf("all: %v, want %v", got.all, want.all)
	}
	for id, w := range want.cat {
		g := got.cat[id]
		postingsEqual(t, fmt.Sprintf("cat %d post", id), g.post, w.post)
		postingsEqual(t, fmt.Sprintf("cat %d exact", id), g.exact, w.exact)
		if !reflect.DeepEqual(g.exactCase, w.exactCase) {
			t.Errorf("cat %d exactCase: %v, want %v", id, g.exactCase, w.exactCase)
		}
		if !bitmapsEqual(g.exc, w.exc) || !bitmapsEqual(g.posted, w.posted) {
			t.Errorf("cat %d exc/posted differ", id)
		}
		if !reflect.DeepEqual(g.stats, w.stats) {
			t.Errorf("cat %d stats:\n got  %+v\n want %+v", id, g.stats, w.stats)
		}
	}
	for id, w := range want.val {
		g := got.val[id]
		postingsEqual(t, fmt.Sprintf("val %d post", id), g.post, w.post)
		if !slices.Equal(g.sortedKeys, w.sortedKeys) {
			t.Errorf("val %d sortedKeys: %v, want %v", id, g.sortedKeys, w.sortedKeys)
		}
		if !bitmapsEqual(g.exc, w.exc) || !bitmapsEqual(g.posted, w.posted) {
			t.Errorf("val %d exc/posted differ", id)
		}
		if !reflect.DeepEqual(g.stats, w.stats) {
			t.Errorf("val %d stats:\n got  %+v\n want %+v", id, g.stats, w.stats)
		}
	}
	if len(got.cat) != len(want.cat) || len(got.val) != len(want.val) {
		t.Errorf("attribute count %d/%d, want %d/%d", len(got.cat), len(got.val), len(want.cat), len(want.val))
	}
}

// TestExtendSegIndexMatchesBuild: an index extended over a segment's newer records, in any number
// of steps, is the index a fresh build over the same prefix gives. The values are the ones the
// postings treat specially -- strings in several casings (a bucket turns mixed only once a later
// record spells it differently), booleans, NaN, -0 and +0, infinities, expressions, wrong-typed
// values, absent attributes -- and the records include delta records and delete markers.
func TestExtendSegIndexMatchesBuild(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 8 << 20, DeltaMax: 4,
		CategoricalAttrs: []string{"Owner", "Flag"}, ValueAttrs: []string{"N", "T"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := rand.New(rand.NewSource(7))
	owners := []string{`"alice"`, `"Alice"`, `"bob"`, `"BOB"`, `"carol"`, `strcat("al", "ice")`, `17`, `true`}
	nums := []string{`0`, `-0.0`, `0.0`, `1`, `2.5`, `-3`, `real("NaN")`, `real("INF")`, `-real("INF")`, `"x"`, `undefined`, `T + 1`, `{1, 2}`}
	for i := 0; i < 300; i++ {
		key := []byte(fmt.Sprintf("k%03d", r.Intn(120)))
		switch r.Intn(6) {
		case 0:
			c.Delete(key)
		case 1, 2:
			patch := classad.New()
			patch.InsertAttr("T", int64(r.Intn(50)))
			w := c.Begin()
			w.PatchAttrs(key, patch, nil)
			w.Commit()
		default:
			src := fmt.Sprintf(`[Id = %d; T = %d; Flag = %v`, i, r.Intn(50), r.Intn(2) == 0)
			if r.Intn(5) > 0 {
				src += "; Owner = " + owners[r.Intn(len(owners))]
			}
			if r.Intn(5) > 0 {
				src += "; N = " + nums[r.Intn(len(nums))]
			}
			if err := c.Put(key, mustAd(t, src+"]")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no delta records written; the test would not cover them")
	}
	seg := c.shards[0].act
	spec := c.spec.Load()
	ends := recordEnds(seg, seg.used)
	if len(ends) < 200 {
		t.Fatalf("only %d records in the write target", len(ends))
	}
	for trial := 0; trial < 20; trial++ {
		var idx *segIndex
		if trial%2 == 1 {
			idx = buildSegIndex(c, seg, ends[r.Intn(len(ends)/4)], spec)
		}
		for {
			from := 0
			if idx != nil {
				from = int(idx.upto)
			}
			i := sort.SearchInts(ends, from+1)
			if i == len(ends) {
				break
			}
			to := ends[i+r.Intn(min(len(ends)-i, 40))]
			base := idx
			idx = extendSegIndex(c, seg, base, to, spec)
			segIndexesEqual(t, idx, buildSegIndex(c, seg, to, spec))
			if t.Failed() {
				t.Fatalf("trial %d: extension to %d (from %d) differs from a fresh build", trial, to, from)
			}
			// The base is published and read concurrently: extending it must leave it as it was.
			if base != nil {
				segIndexesEqual(t, base, buildSegIndex(c, seg, from, spec))
				if t.Failed() {
					t.Fatalf("trial %d: extending to %d modified the index over [0, %d)", trial, to, from)
				}
			}
		}
	}
}

// TestIndexedReadCatchesUpWritableSegments: an indexed read over a mutable delta store whose
// writes have run on past its last Reindex visits only the index candidates, not every record
// written since -- and answers exactly what the plain scan does. With small segments the writes
// also fill and rotate the write target, so the shard holds segments no Reindex ever saw.
func TestIndexedReadCatchesUpWritableSegments(t *testing.T) {
	t.Parallel()
	for _, segSize := range []int{0, 8 << 10} {
		t.Run(fmt.Sprintf("segment=%d", segSize), func(t *testing.T) {
			t.Parallel()
			testIndexedReadCatchesUp(t, segSize)
		})
	}
}

func testIndexedReadCatchesUp(t *testing.T, segSize int) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 4, DeltaMax: 16, SegmentSize: segSize,
		ValueAttrs: []string{"ClusterId", "JobStatus", "EnteredCurrentStatus"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 400
	key := func(i int) []byte { return []byte(fmt.Sprintf("%d.0", 1000+i)) }
	for i := 0; i < n; i++ {
		ad := jobAd(1000+i, map[string]int64{"EnteredCurrentStatus": 1000})
		ad.InsertAttr("JobStatus", 1)
		if err := c.Put(key(i), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	// Patch every job a few times past the index: only some change JobStatus, the rest patch an
	// attribute no index covers, so their current versions are deltas inheriting the indexed ones.
	for round := 1; round <= 3; round++ {
		w := c.Begin()
		for i := 0; i < n; i++ {
			patch := classad.New()
			patch.InsertAttr("Pad00", int64(round))
			if i%7 == round {
				patch.InsertAttr("JobStatus", 2)
				patch.InsertAttr("EnteredCurrentStatus", int64(2000+round))
			}
			w.PatchAttrs(key(i), patch, nil)
		}
		if r := w.Commit(); r.Conflicted() {
			t.Fatal("conflict")
		}
	}
	if d, _ := c.DeltaStats(); d < n {
		t.Fatalf("only %d delta records written", d)
	}
	if segSize > 0 {
		unindexed := 0
		for _, sh := range c.shards {
			for _, seg := range sh.segs {
				if seg != nil && seg.used > 0 && seg.readIdx() == nil {
					unindexed++
				}
			}
		}
		if unindexed == 0 {
			t.Fatal("no segment was opened after the Reindex; the rotation case is not exercised")
		}
	}
	for _, tc := range []struct {
		expr       string
		maxVisited int
	}{
		{`ClusterId == -5`, 0},
		{`ClusterId == 1007`, 1},
		{`EnteredCurrentStatus >= 2002 && JobStatus == 2`, 2 * n / 7},
		{`JobStatus == 2`, 3*n/7 + 3},
		{`EnteredCurrentStatus < 1500 && ClusterId > 1390`, 9},
	} {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		var st ScanStats
		for range c.QueryRawWireStats(q, []string{"ClusterId"}, false, &st) {
		}
		sigsEqual(t, tc.expr, collectQuery(t, c, tc.expr), scanMatches(t, c, q))
		if st.RecordsVisited > tc.maxVisited {
			t.Errorf("%s: visited %d records, want at most %d (the index candidates): the writes since the last Reindex were walked",
				tc.expr, st.RecordsVisited, tc.maxVisited)
		}
	}
}

// TestIndexedReadPinsWindowIndex: one indexed read plans its delta candidates and walks its
// postings over the same index of each segment, even when the segment's index is replaced in
// between. Planned over a short prefix holding no delta and walked over a longer one, a delta the
// longer prefix covers is in neither: it posts nothing for the attribute it inherits, so the
// postings miss it, and the tail walk that would have found it now starts past it.
func TestIndexedReadPinsWindowIndex(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, DeltaMax: 16, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 20; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), jobAd(i, nil)); perr != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	patch := classad.New()
	patch.InsertAttr("Pad00", 99)
	w := c.Begin()
	w.PatchAttrs([]byte("7.0"), patch, nil)
	if r := w.Commit(); r.Conflicted() {
		t.Fatal("conflict")
	}
	q, err := vm.Parse(`ClusterId == 7`)
	if err != nil {
		t.Fatal(err)
	}
	usable := c.planIndex(q.Probes())
	if len(usable) != 1 {
		t.Fatalf("ClusterId == 7 not index-planned")
	}
	sh := c.shards[0]
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	src := conjCandidates{usable: usable, ds: c.newDeltaScope(sh, s0, wins, usable, false)}
	// The read's segment index is replaced mid-read by one covering the delta, as a concurrent
	// Reindex or catch-up does.
	seg := sh.act
	seg.idx.Store(extendSegIndex(c, seg, seg.idx.Load(), seg.used, c.spec.Load()))
	var keys []string
	c.forEachCandidateRef(s0, wins, src, false, nil, func(r recRef) bool {
		keys = append(keys, string(r.key()))
		return true
	})
	if !slices.Contains(keys, "7.0") {
		t.Fatalf("candidates %v miss 7.0, whose current version is a delta written after the read's index", keys)
	}
}

// TestDeltaCandidatesFollowIndexGrowth: the per-segment delta set, cached for one covered prefix,
// is carried forward to a longer prefix and cut back for a shorter one, and agrees with a fresh
// walk either way.
func TestDeltaCandidatesFollowIndexGrowth(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, DeltaMax: 16, ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 40; i++ {
		k := []byte(fmt.Sprintf("%d.0", i%10))
		if i < 10 {
			if err := c.Put(k, jobAd(i, nil)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		patch := classad.New()
		patch.InsertAttr("Pad01", int64(i))
		w := c.Begin()
		w.PatchAttrs(k, patch, nil)
		w.Commit()
	}
	sh := c.shards[0]
	_, wins := sh.snapshot()
	defer releaseWindows(wins)
	w := wins[len(wins)-1]
	ends := recordEnds(w.seg, w.used)
	fresh := func(upto uint32) *roaring.Bitmap {
		bm := roaring.New()
		for off := uint32(0); off < upto; off += recTotalLen(w.data, off) {
			if !recIsMarker(w.data, off) && recIsDelta(w.data, off) {
				bm.Add(off)
			}
		}
		return bm
	}
	for _, i := range []int{5, 20, 39, 12, 30, 2} {
		upto := uint32(ends[min(i, len(ends)-1)])
		got := c.deltaCandidates(w, upto)
		if want := fresh(upto); !bitmapsEqual(got, want) {
			t.Fatalf("deltaCandidates(%d) = %v, want %v", upto, got, want)
		}
	}
}

// TestTopEntriesSelectsSortedPrefix: the top-N selection returns exactly the first topNMax entries
// of the whole slice sorted by count descending, then value -- ties included, which is what makes
// a rebuilt segment's stats reproducible.
func TestTopEntriesSelectsSortedPrefix(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(3))
	for trial := 0; trial < 200; trial++ {
		n := r.Intn(60)
		entries := make([]topEntry, n)
		for i := range entries {
			// Few distinct counts, so ties on count are common and the value decides.
			entries[i] = topEntry{fkey: float64(i), count: uint32(r.Intn(5))}
			if trial%2 == 1 {
				entries[i] = topEntry{skey: fmt.Sprintf("v%03d", r.Perm(1000)[i]), count: uint32(r.Intn(5))}
			}
		}
		r.Shuffle(n, func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
		want := slices.Clone(entries)
		sort.Slice(want, func(i, j int) bool {
			if want[i].count != want[j].count {
				return want[i].count > want[j].count
			}
			if want[i].skey != want[j].skey {
				return want[i].skey < want[j].skey
			}
			return want[i].fkey < want[j].fkey
		})
		want = want[:min(n, topNMax)]
		if got := topEntries(entries); !slices.Equal(got, want) {
			t.Fatalf("trial %d: topEntries = %v, want %v", trial, got, want)
		}
	}
}
