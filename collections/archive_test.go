package collections

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// buildArchive creates an Archive with a small segment size (to force several seals)
// and appends n job-like ads. It returns the archive and the source ads keyed by ID.
func buildArchive(t *testing.T, dir string, n int, opts ArchiveOptions) (*Archive, map[int]*classad.ClassAd) {
	t.Helper()
	opts.Dir = dir
	if opts.SegmentSize == 0 {
		opts.SegmentSize = 8 << 10 // 8 KiB -> many small segments
	}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	src := map[int]*classad.ClassAd{}
	owners := []string{"alice", "bob", "carol", "dave", "eve"}
	states := []string{"Completed", "Removed", "Held"}
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ID=%d; Owner="%s"; JobStatus="%s"; ClusterId=%d; Memory=%d; CompletionDate=%d ]`,
			i, owners[i%len(owners)], states[i%len(states)], i/10, ((i%16)+1)*512, 1_700_000_000+i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	return a, src
}

func archiveQueryIDs(t *testing.T, a *Archive, qs string) []int {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatalf("parse %q: %v", qs, err)
	}
	var ids []int
	for ad := range a.Query(q) {
		ids = append(ids, idOf(t, ad))
	}
	sort.Ints(ids)
	return ids
}

func TestArchiveRoundTripAndQuery(t *testing.T) {
	t.Parallel()
	a, src := buildArchive(t, t.TempDir(), 500, ArchiveOptions{
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"Memory", "ClusterId"},
	})
	defer a.Close()
	if err := a.Flush(); err != nil { // seal the tail so everything is indexed
		t.Fatal(err)
	}
	// Multiple segments must have been created (seal-on-roll-over worked).
	if len(a.segs) < 2 {
		t.Fatalf("expected several segments, got %d", len(a.segs))
	}

	queries := []string{
		`Owner == "alice"`,
		`JobStatus == "Completed" || JobStatus == "Held"`,
		`Owner == "bob" && Memory > 4096`,
		`Memory > 4096`,
		`ClusterId >= 10 && ClusterId < 20`,
		`Owner == "nobody"`, // no matches
		`Owner != "alice"`,  // negation
		`Memory > 1000000`,  // none
	}
	for _, qs := range queries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		got := archiveQueryIDs(t, a, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("query %q: got %d, want %d\n got=%v\nwant=%v", qs, len(got), len(want), got, want)
		}
	}
}

func TestArchiveRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"Memory"},
		ZoneAttrs:        []string{"CompletionDate"},
	}
	a, src := buildArchive(t, dir, 300, opts)
	if err := a.Close(); err != nil { // seals tail, unmaps
		t.Fatal(err)
	}

	// Reopen: results must be identical, from the catalog + sidecar indexes only.
	opts.Dir = dir
	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Includes a != query so the mmap-backed (FromBuffer) all/AndNot path is exercised.
	for _, qs := range []string{`Owner == "carol"`, `Memory > 4096`, `Owner != "carol"`, `Owner == "dave" && Memory <= 2048`} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("after reopen %q: got %v want %v", qs, got, want)
		}
	}
}

func TestArchiveCrashRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}}
	// Append without Close/Flush: the active (tail) segment is left un-sealed and
	// absent from the catalog, simulating a crash.
	a, src := buildArchive(t, dir, 250, opts)
	if a.active == nil || a.active.seg.used == 0 {
		t.Fatal("expected an un-sealed active segment to recover")
	}

	opts.Dir = dir
	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// All appended ads — including those only in the un-sealed active segment — must
	// be recovered via the CRC scan.
	q, _ := vm.Parse(`Owner == "alice" || Owner != "alice"`) // matches every ad with Owner
	got := archiveQueryIDs(t, b, `Owner == "alice" || Owner != "alice"`)
	want := bruteIDs(src, q)
	if !equalInts(got, want) {
		t.Errorf("crash recovery: recovered %d ads, want %d", len(got), len(want))
	}
}

func TestArchiveZonePruning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// CompletionDate increases monotonically with ID, so successive segments hold
	// disjoint, increasing time ranges — ideal for zone pruning.
	a, src := buildArchive(t, dir, 400, ArchiveOptions{
		ValueAttrs: []string{"CompletionDate"},
		ZoneAttrs:  []string{"CompletionDate"},
	})
	defer a.Close()
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	total := len(a.segs)
	if total < 4 {
		t.Fatalf("need several segments for a pruning test, got %d", total)
	}

	var scanned int64
	a.scanned = func(uint32) { atomic.AddInt64(&scanned, 1) }

	// Only the newest ads (highest CompletionDate) match; most segments prune.
	qs := `CompletionDate > 1700000390`
	q, _ := vm.Parse(qs)
	got := archiveQueryIDs(t, a, qs)
	want := bruteIDs(src, q)
	if !equalInts(got, want) {
		t.Fatalf("pruned query wrong: got %v want %v", got, want)
	}
	if n := atomic.LoadInt64(&scanned); int(n) >= total {
		t.Errorf("no pruning: scanned %d of %d segments", n, total)
	} else {
		t.Logf("zone pruning scanned %d of %d segments", n, total)
	}
}

func TestArchiveQueryLimitNewestFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 400, ArchiveOptions{CategoricalAttrs: []string{"Owner"}})
	defer a.Close()
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	q, _ := vm.Parse(`Owner == "alice"`) // alice == every 5th id: 0,5,10,...,395
	const k = 7
	var gotIDs []int
	for ad := range a.QueryLimit(q, k) {
		gotIDs = append(gotIDs, idOf(t, ad))
	}
	if len(gotIDs) != k {
		t.Fatalf("limit not honored: got %d, want %d", len(gotIDs), k)
	}
	// Newest first: the highest alice ids are 395,390,385,...
	want := []int{395, 390, 385, 380, 375, 370, 365}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("newest-first order wrong at %d: got %v want %v", i, gotIDs, want)
			break
		}
	}
}

func TestArchiveRotation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 400, ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
		Retention:        Retention{MaxSegments: 2},
	})
	defer a.Close()
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	before := len(a.segs)
	if before <= 2 {
		t.Fatalf("need >2 segments to rotate, got %d", before)
	}
	oldestID := a.segs[0].seg.id

	dropped, err := a.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != before-2 {
		t.Errorf("dropped %d, want %d", dropped, before-2)
	}
	if len(a.segs) != 2 {
		t.Errorf("kept %d segments, want 2", len(a.segs))
	}
	// The oldest segment's files must be gone, remaining ones still queryable.
	if _, err := os.Stat(a.idxPath(oldestID)); err == nil {
		t.Errorf("dropped segment's sidecar index still present")
	}
	got := archiveQueryIDs(t, a, `Owner == "alice"`)
	if len(got) == 0 {
		t.Errorf("no results after rotation")
	}

	// Reopen: rotation persisted (dropped stay dropped).
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenArchive(ArchiveOptions{Dir: dir, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(b.segs) != 2 {
		t.Errorf("after reopen kept %d segments, want 2", len(b.segs))
	}
}

// TestArchiveConcurrentQueryRotate hammers a static archive with concurrent queries
// and rotation. Run under -race. Every query result must be a valid subset of the
// still-present ads (never a crash / torn read of a reaped segment).
func TestArchiveConcurrentQueryRotate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 600, ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
		Retention:        Retention{MaxSegments: 3},
	})
	defer a.Close()
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	q, _ := vm.Parse(`Owner == "alice"`)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				n := 0
				for ad := range a.Query(q) {
					if own, ok := ad.EvaluateAttrString("Owner"); !ok || own != "alice" {
						t.Errorf("non-matching ad in result: %v", own)
						return
					}
					n++
				}
			}
		}()
	}
	// Rotate repeatedly while queries run; each Rotate drops to MaxSegments.
	for i := 0; i < 50; i++ {
		if _, err := a.Rotate(0); err != nil {
			t.Errorf("rotate: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

// TestArchiveWireNativeFallback exercises the shared wire-native matcher, including
// the fallback to a decode when a queried attribute is a non-literal expression.
func TestArchiveWireNativeFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{
		Dir: dir, SegmentSize: 8 << 10, CategoricalAttrs: []string{"Owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 200; i++ {
		// Rank is an expression (references Base), so a query on it cannot be resolved
		// wire-native and must fall back to a partial/full decode inside matchWire.
		ad, err := classad.Parse(fmt.Sprintf(`[ ID=%d; Owner="u%d"; Base=%d; Rank=Base*2 ]`, i, i%4, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, qs := range []string{
		`Owner == "u1"`,               // pure wire-native (literal attrs)
		`Rank > 100`,                  // expression attr -> fallback decode
		`Rank > 100 && Owner == "u2"`, // index-narrowed + fallback re-verify
	} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, a, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("query %q: got %v want %v", qs, got, want)
		}
	}
}

// TestArchiveLazyIndexLoad verifies sidecar indexes are not loaded at Open and that
// a zone-pruned segment never pages its index in (H5).
func TestArchiveLazyIndexLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{ValueAttrs: []string{"CompletionDate"}, ZoneAttrs: []string{"CompletionDate"}}
	a, src := buildArchive(t, dir, 400, opts)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	opts.Dir = dir
	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	total := len(b.segs)
	if total < 4 {
		t.Fatalf("need several segments, got %d", total)
	}
	// Lazy: nothing loaded immediately after Open (restart is O(segments)).
	for _, as := range b.segs {
		if as.idx.Load() != nil {
			t.Fatalf("segment %d index loaded eagerly at Open", as.seg.id)
		}
	}

	// A zone-selective query touches only the newest segment(s); pruned segments must
	// stay unloaded.
	qs := `CompletionDate > 1700000390`
	q, _ := vm.Parse(qs)
	got := archiveQueryIDs(t, b, qs)
	if want := bruteIDs(src, q); !equalInts(got, want) {
		t.Fatalf("zone query wrong: got %v want %v", got, want)
	}
	loaded := 0
	for _, as := range b.segs {
		if as.idx.Load() != nil {
			loaded++
		}
	}
	if loaded == 0 || loaded >= total {
		t.Errorf("lazy load: %d of %d indexes paged in (want some but not all)", loaded, total)
	}

	// A non-selective query pages in the rest.
	for ad := range b.Query(mustParseQuery(t, `CompletionDate > 0`)) {
		_ = ad
	}
	loadedAll := 0
	for _, as := range b.segs {
		if as.idx.Load() != nil {
			loadedAll++
		}
	}
	if loadedAll != total {
		t.Errorf("after full query, %d of %d indexes loaded", loadedAll, total)
	}
}

func mustParseQuery(t *testing.T, qs string) *vm.Query {
	t.Helper()
	q, err := vm.Parse(qs)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// TestArchiveHighCardinalityIndex round-trips a value index whose keys are nearly
// unique per record (the case the v2 sorted-run sidecar keeps pageable rather than
// resident) and checks equality + range queries still match brute force after a
// reopen goes through the mmap view.
func TestArchiveHighCardinalityIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{ValueAttrs: []string{"Seq"}, CategoricalAttrs: []string{"GJID"}}
	opts.Dir = dir
	opts.SegmentSize = 8 << 10
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 600; i++ {
		// Seq and GJID are unique per record -> one posting per value.
		ad, err := classad.Parse(fmt.Sprintf(`[ ID=%d; Seq=%d; GJID="job.%d.0" ]`, i, i*7, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, qs := range []string{
		`Seq == 700`,               // value equality (binary search)
		`Seq >= 700 && Seq < 1400`, // value range (boundary scan)
		`Seq != 700`,               // value negation
		`GJID == "job.42.0"`,       // categorical equality on unique key
		`GJID == "JOB.42.0"`,       // case-insensitive fold
	} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("query %q: got %v want %v", qs, got, want)
		}
	}
}

func TestZoneMayMatch(t *testing.T) {
	t.Parallel()
	z := zoneRange{Min: 10, Max: 20}
	cases := []struct {
		op   string
		vals []float64
		want bool
	}{
		{"==", []float64{15}, true},
		{"==", []float64{5}, false},
		{"in", []float64{1, 2, 15}, true},
		{"in", []float64{1, 2, 3}, false},
		{"<", []float64{10}, false}, // min not < 10
		{"<", []float64{11}, true},
		{"<=", []float64{10}, true},
		{">", []float64{20}, false},
		{">", []float64{19}, true},
		{">=", []float64{20}, true},
		{"!=", []float64{15}, true}, // never prunable
	}
	for _, c := range cases {
		if got := zoneMayMatch(z, c.op, c.vals); got != c.want {
			t.Errorf("zoneMayMatch(%v, %q, %v) = %v, want %v", z, c.op, c.vals, got, c.want)
		}
	}
}
