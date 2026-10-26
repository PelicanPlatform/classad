package collections

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The paged read narrows each segment to its index candidates, asks the columns before rebuilding a
// record, and builds a projection from the columns where it can. None of that may change a page.
// These tests hold it to the walk it replaced -- every record of every segment, reassembled and
// evaluated in full -- page by page, under writes, compaction, reindexing and columnarization
// between pages.

// refQueryRawFromSeq is the reference: the sequence walk as it was before it read through the
// indexes and columns. It visits every record visible at the page's snapshot, rebuilds it, and
// evaluates the query as written. The one departure is that a record is read at the page's
// snapshot (wireAt) rather than at the newest version, which is what a delta record needs to
// read as the version the snapshot sees.
func refQueryRawFromSeq(c *Collection, q *vm.Query, projection []string, after SeqCursor, limit int) ([]string, SeqPage) {
	var rows []string
	page := refQueryRawFromSeqYield(c, q, projection, after, limit, func(ra RawAd) bool {
		rows = append(rows, rawSig(ra))
		return true
	})
	return rows, page
}

// refQueryRawFromSeqYield is refQueryRawFromSeq handing each row to yield.
func refQueryRawFromSeqYield(c *Collection, q *vm.Query, projection []string, after SeqCursor, limit int, yield func(RawAd) bool) SeqPage {
	page := SeqPage{Next: after}
	var qp queryPlan
	if q != nil {
		qp = c.newQueryPlan(q, nil, nil)
	} else {
		s := &wireScope{ctx: c}
		qp.ws, qp.resolver = s, s.resolve
	}
	emit := c.yieldRawProjected(yield, c.newRawProjector(projection, false, false))
	var dbuf []byte
	emitted := 0
	for si := int(after.Shard); si < len(c.shards); si++ {
		sh := c.shards[si]
		cursor := SeqCursor{Shard: uint32(si)}
		if si == int(after.Shard) {
			cursor = after
		}
		if cursor.Snapshot == 0 {
			sh.mu.RLock()
			cursor.Snapshot = sh.commitSeq
			sh.mu.RUnlock()
		}
		s0 := cursor.Snapshot
		wins := sh.snapshotAt(s0)
		var runs []*seqRun
		for _, w := range wins {
			run := &seqRun{w: w, dict: w.dict()}
			run.advance(s0, cursor.Seq)
			runs = append(runs, run)
		}
		done := func() bool {
			defer releaseWindows(wins)
			var group []groupRec
			for {
				group = nextGroup(runs, s0, cursor.Seq, group[:0])
				if len(group) == 0 {
					return false
				}
				gseq := group[0].seq
				for gi := range group {
					g := &group[gi]
					if gseq == cursor.Seq && cursor.Key != "" && bytes.Compare(g.key, []byte(cursor.Key)) <= 0 {
						continue
					}
					if limit > 0 && emitted == limit {
						page.More = true
						return true
					}
					if isSystemKeyBytes(g.key) {
						continue
					}
					w, err := c.wireAt(g.ref, s0, dbuf)
					if err != nil {
						continue
					}
					dbuf = w
					qp.ws.dict = g.ref.dict
					if q != nil && !matchWire(w, qp) {
						continue
					}
					page.Next = SeqCursor{Shard: uint32(si), Snapshot: s0, Seq: gseq, Key: string(g.key)}
					emitted++
					emit(w, g.ref.dict)
				}
			}
		}()
		if done {
			return page
		}
		page.Next = SeqCursor{Shard: uint32(si) + 1}
	}
	return page
}

// rawSig renders a raw ad for an exact comparison. The entries are sorted: an ad's attribute order
// is not part of its value, and an ad built from columns lists the record's own entries before the
// columns' rather than in the order the whole record holds them.
func rawSig(ra RawAd) string {
	exprs := make([]string, len(ra.Exprs))
	for i, e := range ra.Exprs {
		exprs[i] = string(e)
	}
	sort.Strings(exprs)
	return ra.MyType + "|" + ra.TargetType + "|" + strings.Join(exprs, "; ")
}

// seqPage runs one page of the paged read and returns its rows.
func seqPage(c *Collection, q *vm.Query, projection []string, after SeqCursor, limit int) ([]string, SeqPage) {
	seq, page := c.QueryRawFromSeq(q, projection, after, limit)
	var rows []string
	for ra := range seq {
		rows = append(rows, rawSig(ra))
	}
	return rows, *page
}

// pageBoth pages the read and the reference in lockstep, calling mutate between pages, and fails
// on the first difference.
//
// Page boundaries are the same for both -- a page holds the next `limit` matches in the same order
// -- so each page must agree. The cursors need not: a full page whose shard has no further
// candidate leaves the read's cursor at the start of the next shard and the reference's at its last
// match, which resume to the same records. Nor need More: the read sets it on the next candidate
// after a full page, the reference on the next record, so the reference can still report More after
// the read has finished. Its remaining pages must then be empty. Writes stop once either
// side has finished, because a later page of the reference opens later shards at a fresh snapshot,
// and a write the read never saw would land in it.
func pageBoth(t *testing.T, c *Collection, q *vm.Query, projection []string, limit int, mutate func()) []string {
	t.Helper()
	var (
		cn, cr       SeqCursor
		doneN, doneR bool
		all          []string
	)
	for pages := 0; !doneN || !doneR; pages++ {
		if pages > 100000 {
			t.Fatal("pagination did not terminate")
		}
		var gotN, gotR []string
		var pn, pr SeqPage
		if !doneN {
			gotN, pn = seqPage(c, q, projection, cn, limit)
			if pn.Err != nil {
				t.Fatalf("page %d (cursor %+v): %v", pages, cn, pn.Err)
			}
		}
		if !doneR {
			gotR, pr = refQueryRawFromSeq(c, q, projection, cr, limit)
		}
		switch {
		case !doneN && !doneR:
			if !slices.Equal(gotN, gotR) {
				t.Fatalf("page %d (cursor %+v) differs\n read:      %q\n reference: %q", pages, cn, gotN, gotR)
			}
			all = append(all, gotN...)
			if pn.More && !pr.More {
				t.Fatalf("page %d: the read reports More, the reference walked every record and does not", pages)
			}
		case doneN:
			if len(gotR) != 0 {
				t.Fatalf("page %d: the read finished but the reference's next page holds %q", pages, gotR)
			}
		}
		doneN = doneN || !pn.More
		doneR = doneR || !pr.More
		cn, cr = pn.Next, pr.Next
		if !doneN && !doneR && mutate != nil {
			mutate()
		}
	}
	return all
}

// seqFixtureAd is a job ad in the shapes a paged read meets: schema'd literals the columns hold,
// an attribute only some ads carry, values stored as expressions -- including ones that read
// CurrentTime and one that DEFINES it -- and a MyType the projection lifts.
func seqFixtureAd(i int, now int64, rng *rand.Rand) *classad.ClassAd {
	var b strings.Builder
	fmt.Fprintf(&b, `[ MyType="Job"; ClusterId=%d; ProcId=%d; Cmd="/bin/sleep"; QDate=%d; RequestMemory=%d`,
		i, i%10, now-int64(rng.Intn(100000)), (i%16)*1024)
	switch rng.Intn(9) {
	case 0:
		b.WriteString(`; Owner=strcat("user", "1")`)
	default:
		fmt.Fprintf(&b, `; Owner="user%d"`, i%7)
	}
	switch rng.Intn(10) {
	case 0:
		b.WriteString(`; JobStatus=1+1`)
	default:
		fmt.Fprintf(&b, `; JobStatus=%d`, 1+i%5)
	}
	switch rng.Intn(8) {
	case 0:
		b.WriteString(`; EnteredCurrentStatus=CurrentTime-60`)
	default:
		fmt.Fprintf(&b, `; EnteredCurrentStatus=%d`, now-int64(rng.Intn(5000)))
	}
	if rng.Intn(40) == 0 {
		b.WriteString(`; CurrentTime=5`)
	}
	if i%5 == 0 {
		fmt.Fprintf(&b, `; Rare%d=%d`, i%3, i)
	}
	if i%5 == 1 {
		fmt.Fprintf(&b, `; Wide=%d`, int64(i)*1000003)
	}
	b.WriteString(" ]")
	ad, err := classad.Parse(b.String())
	if err != nil {
		panic(err)
	}
	return ad
}

// seqWriter applies a random write to c: a new key, a patch (a delta record where delta mode is
// on), a whole rewrite, or a delete. It returns the next key number.
func seqWriter(t testing.TB, c *Collection, rng *rand.Rand, next *int, now int64) {
	t.Helper()
	key := func() []byte { return []byte(fmt.Sprintf("%d.0", rng.Intn(*next))) }
	switch r := rng.Intn(10); {
	case r < 2 || c.appendOnly(): // an append-only store only ever gains keys
		if err := c.Put([]byte(fmt.Sprintf("%d.0", *next)), seqFixtureAd(*next, now, rng)); err != nil {
			t.Fatal(err)
		}
		*next++
	case r < 7:
		patch := classad.New()
		switch rng.Intn(3) {
		case 0:
			patch.InsertAttr("JobStatus", int64(1+rng.Intn(5)))
		case 1:
			patch.InsertAttr("RemoteUserCpu", int64(rng.Intn(1000)))
		default:
			patch.InsertAttr("RequestMemory", int64(rng.Intn(16))*1024)
		}
		tx := c.Begin()
		tx.PatchAttrs(key(), patch, nil)
		tx.Commit()
	case r < 9:
		k := key()
		var id int
		fmt.Sscanf(string(k), "%d.0", &id)
		if err := c.Put(k, seqFixtureAd(id, now, rng)); err != nil {
			t.Fatal(err)
		}
	default:
		c.Delete(key())
	}
}

// seqEquivConfigs are the store shapes the read has to agree across. The persistent ones are the
// shape of a production jobs table: mutable, indexed, delta records, columnarized, and one of them
// encrypted at rest.
func seqEquivConfigs(t *testing.T) map[string]Options {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	idx := func(o Options) Options {
		o.ValueAttrs = []string{"ClusterId", "JobStatus", "QDate", "EnteredCurrentStatus"}
		o.CategoricalAttrs = []string{"Owner"}
		return o
	}
	return map[string]Options{
		"ram":                {Shards: 2, SegmentSize: 1 << 13},
		"ram-indexed":        idx(Options{Shards: 3, SegmentSize: 1 << 13}),
		"persistent-columns": idx(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1}),
		"persistent-encrypted": idx(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14, DeltaMax: 8, GroupSchemaCount: -1,
			DataKey: key, EncryptedAttrs: []string{"Cmd"}}),
		// Zone maps are kept only by an append-only collection, so this is the shape that
		// exercises zone pruning.
		"append-only-zoned": idx(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 13, AppendOnly: true, GroupSchemaCount: -1,
			ZoneAttrs: []string{"ClusterId", "QDate", "RequestMemory", "EnteredCurrentStatus"}}),
	}
}

var seqEquivConstraints = []string{
	"",
	`ClusterId == 17`,
	`ClusterId >= 100 && ClusterId < 400`,
	`ClusterId == 3 && ClusterId == 4`,
	`JobStatus == 2`,
	`JobStatus != 2 && ProcId < 5`,
	`Owner == "user3"`,
	`Owner =?= "user1"`,
	`Owner != "user2" && ClusterId < 300`,
	`RequestMemory > 4096`,
	`Cmd == "/bin/sleep" && ProcId == 3`,
	`ClusterId == 5 || Owner == "user1"`,
	`Rare1 is undefined && ProcId == 1`,
	`Wide isnt undefined`,
	`QDate > CurrentTime - 50000`,
	`QDate > CurrentTime + 1000`, // none at the pinned time; every ad that defines CurrentTime=5
	`EnteredCurrentStatus < CurrentTime - 1000`,
	`NoSuchAttr == 1`,
}

var seqEquivProjections = [][]string{nil, {"ClusterId", "Owner"}, {"ClusterId", "Rare0", "Wide", "Cmd", "MyType"}, {"JobStatus"}}

// TestSeqReadMatchesFullWalk pages every constraint, projection and page size through the read
// and the reference in lockstep, with writes and maintenance between pages.
func TestSeqReadMatchesFullWalk(t *testing.T) {
	t.Parallel()
	seed := time.Now().UnixNano()
	if v := os.Getenv("SEQ_SEED"); v != "" {
		seed, _ = strconv.ParseInt(v, 10, 64)
	}
	t.Logf("seed %d", seed)
	for name, opts := range seqEquivConfigs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(seed + int64(len(name))))
			c, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { c.Close() }() // c is replaced by the reopen below
			now := time.Now().Unix()
			next := 0
			for ; next < 600; next++ {
				if perr := c.Put([]byte(fmt.Sprintf("%d.0", next)), seqFixtureAd(next, now, rng)); perr != nil {
					t.Fatal(perr)
				}
			}
			// Churn, which an append-only store has none of: there every write is a new key, and
			// each one syncs.
			churn := 400
			if opts.AppendOnly {
				churn = 0
			}
			for range churn {
				seqWriter(t, c, rng, &next, now)
			}
			c.Reindex()
			if opts.Dir != "" {
				if !c.BuildAndEnableSchemaScan(4096, 8) {
					t.Fatal("no schema; the columnar paths would go untested")
				}
				c.ColumnarizeSealed()
				if columnarSegmentsForTest(c) == 0 {
					t.Fatal("nothing columnarized; the columnar paths would go untested")
				}
				if d, _ := c.DeltaStats(); d == 0 && opts.DeltaMax > 0 {
					t.Fatal("no delta records; the delta paths would go untested")
				}
			}
			for range 100 {
				seqWriter(t, c, rng, &next, now)
			}
			// Every combination once without writes, which also checks the read against the
			// ordinary query path.
			for _, expr := range seqEquivConstraints {
				var q *vm.Query
				if expr != "" {
					if q, err = vm.Parse(expr); err != nil {
						t.Fatal(err)
					}
				}
				for _, proj := range seqEquivProjections {
					for _, limit := range []int{0, 7, 64} {
						got := pageBoth(t, c, q, proj, limit, nil)
						if limit == 0 {
							checkAgainstQueryRaw(t, c, q, proj, got, expr)
						}
					}
				}
			}
			randomQuery := func() (*vm.Query, []string) {
				expr := seqEquivConstraints[rng.Intn(len(seqEquivConstraints))]
				var q *vm.Query
				if expr != "" {
					var perr error
					if q, perr = vm.Parse(expr); perr != nil {
						t.Fatal(perr)
					}
				}
				return q, seqEquivProjections[rng.Intn(len(seqEquivProjections))]
			}
			// maintain writes and runs maintenance, compacting, reindexing or columnarizing
			// about one call in every 3*every.
			maintain := func(every int) {
				writes := rng.Intn(3)
				if opts.AppendOnly && rng.Intn(4) != 0 {
					writes = 0
				}
				for range writes {
					seqWriter(t, c, rng, &next, now)
				}
				switch rng.Intn(3 * every) {
				case 0:
					c.Compact()
				case 1:
					c.Reindex()
				case 2:
					if opts.Dir != "" {
						c.BuildAndEnableSchemaScan(4096, 8)
						c.ColumnarizeSealed()
					}
				}
			}
			// randomPages pages random combinations with writes and maintenance between pages.
			randomPages := func(n int) {
				for range n {
					q, proj := randomQuery()
					// Page size 1 puts a page boundary inside every commit group, but costs a
					// page per match: keep it to the selective constraints.
					limit := []int{3, 17, 50, 1000}[rng.Intn(4)]
					if q != nil && rng.Intn(3) == 0 {
						limit = 1
					}
					pageBoth(t, c, q, proj, limit, func() { maintain(13) })
				}
			}
			randomPages(40)

			if opts.Dir != "" {
				// A clean close and reopen restores each shard from its directory snapshot, which
				// does not walk the records, so no segment knows its sequence bounds and none is
				// skipped whole: the seek alone keeps a page from walking what earlier pages read.
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				if c, err = Open(opts); err != nil {
					t.Fatal(err)
				}
				c.Reindex()
				if !anySegmentWithoutSeqBounds(c) {
					// The snapshot is refused when the segment set it was taken over does not match
					// the one Open loads, which some histories produce; Open then walks the records
					// and every segment learns its bounds. Put the segments in the state the
					// snapshot leaves, so the reopened shape is tested either way.
					clearSegmentSeqBounds(c)
				}
			}
			// Deep cursors: resume directly from cursors throughout the order, with writes and
			// maintenance between, so the seek is built deep in a segment, extended over a grown
			// active segment, and built afresh over the segments compaction and columnarization
			// put in place.
			//
			// The cursors come from the read, which leases each shard's snapshot as a paginating
			// client's would: the reference takes none, and maintenance between pages would be
			// free to reclaim what an unleased snapshot sees.
			var cursors []SeqCursor
			for cur := (SeqCursor{}); ; {
				_, page := seqPage(c, nil, nil, cur, 23)
				if page.Err != nil {
					t.Fatal(page.Err)
				}
				if !page.More {
					break
				}
				cursors = append(cursors, page.Next)
				cur = page.Next
			}
			rng.Shuffle(len(cursors), func(i, j int) { cursors[i], cursors[j] = cursors[j], cursors[i] })
			for _, cur := range cursors[:min(len(cursors), 40)] {
				q, proj := randomQuery()
				limit := []int{1, 7, 50}[rng.Intn(3)]
				gotN, pn := seqPage(c, q, proj, cur, limit)
				if pn.Err != nil {
					t.Fatalf("page from cursor %+v: %v", cur, pn.Err)
				}
				gotR, pr := refQueryRawFromSeq(c, q, proj, cur, limit)
				if !slices.Equal(gotN, gotR) {
					t.Fatalf("page from cursor %+v differs\n read:      %q\n reference: %q", cur, gotN, gotR)
				}
				if pn.More && !pr.More {
					t.Fatalf("page from cursor %+v: the read reports More, the reference does not", cur)
				}
				maintain(4)
			}
			randomPages(10)
		})
	}
}

// checkAgainstQueryRaw compares an unpaged read to the ordinary projected query, as sets: that path
// has an order of its own.
func checkAgainstQueryRaw(t *testing.T, c *Collection, q *vm.Query, proj, got []string, expr string) {
	t.Helper()
	if q == nil {
		q, _ = vm.Parse("true")
	}
	var want []string
	for ra := range c.QueryRawProjected(q, proj, false, false) {
		want = append(want, rawSig(ra))
	}
	got = slices.Clone(got)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%q proj %v: the paged read returned %d rows, QueryRawProjected %d", expr, proj, len(got), len(want))
	}
}

// columnarSegmentsForTest counts the segments holding a columnar payload.
func columnarSegmentsForTest(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.colNative.Load() != nil {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// clearSegmentSeqBounds zeroes every segment's sequence bounds and dead-byte counters, as a reopen
// from the directory snapshot leaves them.
func clearSegmentSeqBounds(c *Collection) {
	for _, sh := range c.shards {
		sh.mu.Lock()
		for _, seg := range sh.segs {
			if seg != nil {
				seg.minSeq, seg.maxSeq, seg.maxSup, seg.dead = 0, 0, 0, 0
			}
		}
		sh.mu.Unlock()
	}
}

// anySegmentWithoutSeqBounds reports whether some segment of c does not know its sequence bounds,
// as a segment restored without walking its records does not.
func anySegmentWithoutSeqBounds(c *Collection) bool {
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.used > 0 && seg.maxSeq == 0 {
				sh.mu.RUnlock()
				return true
			}
		}
		sh.mu.RUnlock()
	}
	return false
}
