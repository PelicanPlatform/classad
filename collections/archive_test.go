package collections

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The Archive is a thin facade over an append-only Collection; these tests lock in the
// Archive API contract (append/query/limit/count/rotate/recovery/zone-pruning/watch). The
// per-segment index structures (bloom/MPH/sidecar) are the Collection's own concern and
// are covered by its tests; here we assert query results, order, and durability.

// buildArchive creates an Archive with a small segment size (to force several seals) and
// appends n job-like ads. It returns the archive and the source ads keyed by ID.
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

// archiveLiveSegs counts the archive's live (non-reclaimed) segments through the backing
// Collection -- the facade has no exported segment count.
func archiveLiveSegs(a *Archive) int {
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	n := 0
	for _, s := range sh.segs {
		if s != nil {
			n++
		}
	}
	return n
}

func TestArchiveRoundTripAndQuery(t *testing.T) {
	t.Parallel()
	a, src := buildArchive(t, t.TempDir(), 500, ArchiveOptions{
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"Memory", "ClusterId"},
	})
	defer a.Close()
	if archiveLiveSegs(a) < 2 {
		t.Fatalf("expected several segments (seal-on-rollover), got %d", archiveLiveSegs(a))
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
		`Owner =?= "alice"`,
		`Owner =!= "alice"`,
		`JobStatus =?= "Completed"`,
		`JobStatus =?= "completed"`, // wrong case -> none
		`JobStatus =!= "Held"`,
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

// TestArchiveExactCaseMatch: =?= distinguishes case, =!= keeps the other variant, matching
// a brute-force scan after seal+reopen.
func TestArchiveExactCaseMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 8 << 10, CategoricalAttrs: []string{"Arch"}})
	if err != nil {
		t.Fatal(err)
	}
	arches := []string{"X86_64", "x86_64", "aarch64"}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 300; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ID=%d; Arch="%s" ]`, i, arches[i%len(arches)]))
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
	b, err := OpenArchive(ArchiveOptions{Dir: dir, CategoricalAttrs: []string{"Arch"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, qs := range []string{
		`Arch =?= "X86_64"`,
		`Arch =?= "x86_64"`,
		`Arch == "x86_64"`, // folded: both
		`Arch =!= "X86_64"`,
		`Arch =!= "aarch64"`,
	} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("archive %q: got %d, want %d", qs, len(got), len(want))
		}
	}
}

// TestArchivePresenceMatch: presence probes (is/isnt undefined, isUndefined()) match a
// brute-force scan after reopen, over a corpus mixing absent/expression/literal values.
func TestArchivePresenceMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 8 << 10,
		CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	if err != nil {
		t.Fatal(err)
	}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 300; i++ {
		var text string
		switch i % 3 {
		case 0:
			text = fmt.Sprintf(`[ ID=%d; Owner="alice"; Memory=%d ]`, i, (i%8+1)*512)
		case 1:
			text = fmt.Sprintf(`[ ID=%d; Memory=%d ]`, i, (i%8+1)*512) // Owner absent
		default:
			text = fmt.Sprintf(`[ ID=%d; Owner=Base; Memory=%d ]`, i, (i%8+1)*512) // Owner undefined
		}
		ad, err := classad.Parse(text)
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
	b, err := OpenArchive(ArchiveOptions{Dir: dir, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, qs := range []string{
		`Owner is undefined`,
		`Owner isnt undefined`,
		`isUndefined(Owner)`,
		`!isUndefined(Owner)`,
		`Memory isnt undefined`,
		`Owner is undefined && Memory > 1024`,
	} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("archive %q: got %d, want %d", qs, len(got), len(want))
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
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	opts.Dir = dir
	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, qs := range []string{`Owner == "carol"`, `Memory > 4096`, `Owner != "carol"`, `Owner == "dave" && Memory <= 2048`} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("after reopen %q: got %v want %v", qs, got, want)
		}
	}
}

// TestArchiveCrashRecovery reopens without a prior Close: records only in the un-sealed
// active segment must still be recovered.
func TestArchiveCrashRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}}
	a, src := buildArchive(t, dir, 250, opts) // no Close: simulates a crash
	if a.Count() != 250 {
		t.Fatalf("pre-crash Count = %d, want 250", a.Count())
	}
	opts.Dir = dir
	b, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	q, _ := vm.Parse(`Owner == "alice" || Owner != "alice"`) // every ad with Owner
	got := archiveQueryIDs(t, b, `Owner == "alice" || Owner != "alice"`)
	want := bruteIDs(src, q)
	if !equalInts(got, want) {
		t.Errorf("crash recovery: recovered %d ads, want %d", len(got), len(want))
	}
}

// TestArchiveZonePruning: a range query on a zone-mapped attribute returns exactly the
// matching records (correctness under whole-segment pruning). That pruning actually skips
// segments is covered by the Collection-level zone-map test.
func TestArchiveZonePruning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, src := buildArchive(t, dir, 400, ArchiveOptions{
		ValueAttrs: []string{"CompletionDate"},
		ZoneAttrs:  []string{"CompletionDate"},
	})
	defer a.Close()
	if archiveLiveSegs(a) < 4 {
		t.Fatalf("need several segments for a pruning test, got %d", archiveLiveSegs(a))
	}
	qs := `CompletionDate > 1700000390`
	q, _ := vm.Parse(qs)
	got := archiveQueryIDs(t, a, qs)
	want := bruteIDs(src, q)
	if !equalInts(got, want) {
		t.Fatalf("pruned query wrong: got %v want %v", got, want)
	}
}

func TestArchiveQueryLimitNewestFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 400, ArchiveOptions{CategoricalAttrs: []string{"Owner"}})
	defer a.Close()
	q, _ := vm.Parse(`Owner == "alice"`) // alice == every 5th id: 0,5,...,395
	const k = 7
	var gotIDs []int
	for ad := range a.QueryLimit(q, k) {
		gotIDs = append(gotIDs, idOf(t, ad))
	}
	if len(gotIDs) != k {
		t.Fatalf("limit not honored: got %d, want %d", len(gotIDs), k)
	}
	want := []int{395, 390, 385, 380, 375, 370, 365} // newest first
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("newest-first order wrong at %d: got %v want %v", i, gotIDs, want)
			break
		}
	}
}

// TestArchiveRotation: retention drops whole oldest segments, reducing Count and dropping
// the oldest records, and the rotation persists across a reopen.
func TestArchiveRotation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 400, ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
		Retention:        Retention{MaxSegments: 2},
	})
	defer a.Close()
	if archiveLiveSegs(a) <= 2 {
		t.Fatalf("need >2 segments to rotate, got %d", archiveLiveSegs(a))
	}
	before := a.Count()
	dropped, err := a.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("rotation dropped nothing")
	}
	if archiveLiveSegs(a) > 2 {
		t.Errorf("kept %d segments, want <= 2", archiveLiveSegs(a))
	}
	if a.Count() >= before {
		t.Errorf("Count did not drop after rotation: before %d, after %d", before, a.Count())
	}
	// Newest records survive; oldest are gone.
	minID, maxID := 1<<30, -1
	for ad := range a.Query(mustParseQuery(t, `ID >= 0`)) {
		id := idOf(t, ad)
		if id < minID {
			minID = id
		}
		if id > maxID {
			maxID = id
		}
	}
	if maxID != 399 {
		t.Errorf("newest ID = %d, want 399 (newest never dropped)", maxID)
	}
	if minID == 0 {
		t.Errorf("oldest ID 0 survived; rotation should have dropped it")
	}

	// Reopen: rotation persisted.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenArchive(ArchiveOptions{Dir: dir, CategoricalAttrs: []string{"Owner"}, Retention: Retention{MaxSegments: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Count() >= before {
		t.Errorf("after reopen Count = %d, want < %d (rotation persisted)", b.Count(), before)
	}
}

// TestArchiveConcurrentQueryRotate hammers the archive with concurrent queries and
// rotation. Run under -race: every result is a valid match, never a torn read of a
// reaped segment.
func TestArchiveConcurrentQueryRotate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := buildArchive(t, dir, 600, ArchiveOptions{
		CategoricalAttrs: []string{"Owner"},
		Retention:        Retention{MaxSegments: 3},
	})
	defer a.Close()
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
				for ad := range a.Query(q) {
					if own, ok := ad.EvaluateAttrString("Owner"); !ok || own != "alice" {
						t.Errorf("non-matching ad in result: %v", own)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if _, err := a.Rotate(0); err != nil {
			t.Errorf("rotate: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

// TestArchiveWireNativeFallback: the shared wire-native matcher, including the fallback to
// a decode when a queried attribute is a non-literal expression.
func TestArchiveWireNativeFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 8 << 10, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 200; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ID=%d; Owner="u%d"; Base=%d; Rank=Base*2 ]`, i, i%4, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
		src[i] = ad
	}
	for _, qs := range []string{
		`Owner == "u1"`,
		`Rank > 100`,                  // expression attr -> fallback decode
		`Rank > 100 && Owner == "u2"`, // narrowed + fallback re-verify
	} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, a, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("query %q: got %v want %v", qs, got, want)
		}
	}
}

// TestArchiveHighCardinalityIndex round-trips value/categorical queries whose keys are
// nearly unique per record, through the mmap view after a reopen.
func TestArchiveHighCardinalityIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := ArchiveOptions{ValueAttrs: []string{"Seq"}, CategoricalAttrs: []string{"GJID"}, Dir: dir, SegmentSize: 8 << 10}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < 600; i++ {
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
		`Seq == 700`,
		`Seq >= 700 && Seq < 1400`,
		`Seq != 700`,
		`GJID == "job.42.0"`,
		`GJID == "JOB.42.0"`, // case-insensitive fold
	} {
		q, _ := vm.Parse(qs)
		got := archiveQueryIDs(t, b, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("query %q: got %v want %v", qs, got, want)
		}
	}
}

// TestArchiveCreateExisting: CreateArchive refuses an already-initialized directory
// (OpenArchive is the reopen path).
func TestArchiveCreateExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Append(mustAd(t, `[ ID=1 ]`)); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := CreateArchive(ArchiveOptions{Dir: dir}); err == nil {
		t.Error("CreateArchive on an existing archive should error")
	}
	if _, err := OpenArchive(ArchiveOptions{Dir: dir}); err != nil {
		t.Errorf("OpenArchive on an existing archive failed: %v", err)
	}
}

// TestArchiveWatch exercises the facade Watch: full replay (oldest first) then a live
// append, and a Reset when a cursor falls below the retained floor after rotation.
func TestArchiveWatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 8 << 10, Retention: Retention{MaxSegments: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 20; i++ {
		if err := a.Append(mustAd(t, fmt.Sprintf(`[ ID=%d ]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Full replay: Reset, every retained record oldest-first, Synced with a cursor.
	seq, err := a.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var cursor []byte
	sawReset := false
	for ev := range seq {
		switch ev.Kind {
		case WatchReset:
			sawReset = true
		case WatchUpsert:
			v, _ := ev.Ad.EvaluateAttrInt("ID")
			ids = append(ids, v)
		case WatchSynced:
			cursor = ev.Cursor
		}
		if ev.Kind == WatchSynced {
			break
		}
	}
	if !sawReset {
		t.Error("full replay should begin with Reset")
	}
	if len(ids) != 20 {
		t.Fatalf("full replay yielded %d records, want 20", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("replay not oldest-first at %d: %v", i, ids)
		}
	}
	if cursor == nil {
		t.Fatal("Synced carried no cursor")
	}

	// A cursor below the retained floor after rotation triggers a Reset.
	for i := 20; i < 400; i++ {
		if err := a.Append(mustAd(t, fmt.Sprintf(`[ ID=%d ]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if dropped, _ := a.Rotate(0); dropped == 0 {
		t.Fatal("expected rotation to drop segments")
	}
	seq2, err := a.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	reset2 := false
	for ev := range seq2 {
		if ev.Kind == WatchReset {
			reset2 = true
		}
		if ev.Kind == WatchSynced {
			break
		}
	}
	if !reset2 {
		t.Error("a cursor below the retained floor should Reset")
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

// TestArchiveWatchLive verifies a live append reaches an already-synced watcher.
func TestArchiveWatchLive(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Append(mustAd(t, `[ ID=0 ]`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan int64, 4)
	go func() {
		seq, err := a.Watch(ctx, nil)
		if err != nil {
			return
		}
		synced := false
		for ev := range seq {
			switch ev.Kind {
			case WatchSynced:
				synced = true
			case WatchUpsert:
				if synced {
					v, _ := ev.Ad.EvaluateAttrInt("ID")
					got <- v
				}
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	a.Append(mustAd(t, `[ ID=777 ]`))
	select {
	case v := <-got:
		if v != 777 {
			t.Fatalf("live event ID=%d, want 777", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live append not delivered")
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
		{"<", []float64{10}, false},
		{"<", []float64{11}, true},
		{"<=", []float64{10}, true},
		{">", []float64{20}, false},
		{">", []float64{19}, true},
		{">=", []float64{20}, true},
		{"!=", []float64{15}, true},
	}
	for _, c := range cases {
		if got := zoneMayMatch(z, c.op, c.vals); got != c.want {
			t.Errorf("zoneMayMatch(%v, %q, %v) = %v, want %v", z, c.op, c.vals, got, c.want)
		}
	}
}
