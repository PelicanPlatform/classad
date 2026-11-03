package dbrpc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// WHAT AN N-WAY JOIN IN THE STORE COULD BUY, PRICED BEFORE BUILDING ONE.
//
// The store's join opcode carries two tables, so a client joining three or more pairs them
// itself: it fetches each table in turn, narrowing every table after the first to the keys
// the rows so far hold, and folds one table in per step. The question this prices is whether
// moving that whole chain into the store is worth a new opcode.
//
// The two arms share ONE left-deep implementation and differ only in where the rows come
// from, which is the same control the two-table wire benchmark uses: `wire` reads each side
// through a real server over a pipe and rebuilds every row; `inproc` reads the same
// attributes straight from the catalog. `inproc` is therefore a CEILING on the store arm,
// not an estimate of it -- a store-side n-way still has to ship its output rows, and this
// subtracts nothing for that. The bytes each arm puts on the wire are counted alongside, so
// the ceiling can be read against what it would have to pay back.
//
// The shape is the one a three-table chain is actually wanted for: completed jobs joined to
// their run attempts joined to a per-job metrics table. The metrics table's density is the
// knob -- one row per job EXPANDS the chain, one row per fiftieth job makes it FILTER -- and
// that knob is the whole answer.

const (
	nwayHistWidth   = 80
	nwayEpochWidth  = 25
	nwayMetricWidth = 20
	nwayEpochs      = 4
	// nwayKeyBatch is the client's cap on how many key values it folds into one
	// membership constraint, matching the client join's own.
	nwayKeyBatch = 512
)

// nwayCatalog builds the three tables. metricEvery = 1 gives every job a metrics row (the
// chain expands); 50 gives one job in fifty a row (the chain filters).
func nwayCatalog(tb testing.TB, jobs, metricEvery int) (*db.Catalog, func()) {
	tb.Helper()
	cat, err := db.OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	mk := func(name string) *db.DB {
		t, terr := cat.CreateTable(name)
		if terr != nil {
			tb.Fatal(terr)
		}
		return t
	}
	hist, epoch, metric := mk("history"), mk("epoch_history"), mk("job_metrics")
	pad := func(b *strings.Builder, prefix string, n int) {
		for k := 0; k < n; k++ {
			fmt.Fprintf(b, `; %s%02d = "a padding attribute value"`, prefix, k)
		}
	}
	put := func(t *db.DB, key, text string) {
		ad, perr := classad.Parse(text)
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := t.Put(key, ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	for i := 0; i < jobs; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ ClusterId = %d; ProcId = 0; Owner = "user%02d"; JobStatus = 4; RequestMemory = %d`,
			i, i%32, 1024+(i%8)*512)
		pad(&b, "HAttr", nwayHistWidth-5)
		b.WriteString(" ]")
		put(hist, fmt.Sprintf("h%d", i), b.String())

		for e := 0; e < nwayEpochs; e++ {
			var eb strings.Builder
			fmt.Fprintf(&eb, `[ ClusterId = %d; NumShadowStarts = %d; RequestMemory = %d; MemoryUsage = %d`,
				i, e+1, 1024+(i%8)*512+e*256, 900+e*200)
			pad(&eb, "EAttr", nwayEpochWidth-4)
			eb.WriteString(" ]")
			put(epoch, fmt.Sprintf("e%d.%d", i, e), eb.String())
		}

		if i%metricEvery == 0 {
			var mb strings.Builder
			fmt.Fprintf(&mb, `[ ClusterId = %d; CpuEfficiency = %d; PeakRss = %d`, i, 50+i%50, 2048+i%1024)
			pad(&mb, "MAttr", nwayMetricWidth-3)
			mb.WriteString(" ]")
			put(metric, fmt.Sprintf("m%d", i), mb.String())
		}
	}
	return cat, func() { cat.Close() }
}

// nwaySide is one table of the chain, under the alias its expressions reference.
type nwaySide struct {
	table, alias string
	proj         []string
}

// nwayChain is the plan both arms run: history h, then epoch_history e, then job_metrics m,
// every step an equi join on ClusterId.
func nwayChain() []nwaySide {
	return []nwaySide{
		{table: "history", alias: "h", proj: []string{"ClusterId", "Owner", "RequestMemory"}},
		{table: "epoch_history", alias: "e", proj: []string{"ClusterId", "NumShadowStarts", "MemoryUsage"}},
		{table: "job_metrics", alias: "m", proj: []string{"ClusterId", "CpuEfficiency", "PeakRss"}},
	}
}

// nwayFetch reads one side: the attributes named, narrowed by the constraint. It reports the
// payload bytes that crossing cost, which is 0 for a reader that did not cross anything.
type nwayFetch func(table, constraint string, proj []string) (ads []*classad.ClassAd, bytes int, err error)

// nwayRow is one accumulated row of the left-deep chain: the members joined so far, and the
// join key the next step will probe with, kept beside the ad so the chain does not re-evaluate
// a select to find it.
type nwayRow struct {
	ad  *classad.ClassAd
	key int64
}

// runNway is the left-deep chain, shared by both arms. sides[0] is the FROM table and each
// later side is folded in against the keys the rows so far hold, exactly as the client's own
// n-way join does. It returns the joined rows and the payload bytes it read.
func runNway(fetch nwayFetch, sides []nwaySide) ([]nwayRow, int, error) {
	total := 0
	base, n, err := fetch(sides[0].table, "true", sides[0].proj)
	if err != nil {
		return nil, 0, err
	}
	total += n
	rows := make([]nwayRow, 0, len(base))
	for _, ad := range base {
		k, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			continue
		}
		row := classad.New()
		row.InsertAttrClassAd(sides[0].alias, ad)
		rows = append(rows, nwayRow{ad: row, key: k})
	}

	for _, s := range sides[1:] {
		next, n, ferr := fetch(s.table, nwayKeyConstraint(rows), s.proj)
		if ferr != nil {
			return nil, 0, ferr
		}
		total += n
		buckets := map[int64][]*classad.ClassAd{}
		for _, ad := range next {
			if k, ok := ad.EvaluateAttrInt("ClusterId"); ok {
				buckets[k] = append(buckets[k], ad)
			}
		}
		out := make([]nwayRow, 0, len(rows))
		for _, row := range rows {
			for _, ad := range buckets[row.key] {
				merged := classad.New()
				for _, prev := range sides {
					if prev.alias == s.alias {
						break
					}
					if member, ok := row.ad.Lookup(prev.alias); ok {
						merged.InsertExpr(prev.alias, member)
					}
				}
				merged.InsertAttrClassAd(s.alias, ad)
				out = append(out, nwayRow{ad: merged, key: row.key})
			}
		}
		rows = out
	}
	return rows, total, nil
}

// nwayKeyConstraint folds the keys the rows so far hold into one membership constraint, or
// gives up and reads the table whole past the cap -- which is the client's rule, cap and all.
func nwayKeyConstraint(rows []nwayRow) string {
	seen := map[int64]bool{}
	for _, r := range rows {
		seen[r.key] = true
		if len(seen) > nwayKeyBatch {
			return "true"
		}
	}
	keys := make([]int64, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	terms := make([]string, 0, len(keys))
	for _, k := range keys {
		terms = append(terms, fmt.Sprintf("ClusterId == %d", k))
	}
	return "(" + strings.Join(terms, " || ") + ")"
}

func nwayWireFetch(c *Client) nwayFetch {
	ctx := context.Background()
	return func(table, constraint string, proj []string) ([]*classad.ClassAd, int, error) {
		rows, err := c.QueryRawProject(ctx, table, constraint, proj, 0)
		if err != nil {
			return nil, 0, err
		}
		bytes := 0
		ads := make([]*classad.ClassAd, 0, len(rows))
		for _, r := range rows {
			bytes += len(r)
			ad, perr := classad.ParseOld(r)
			if perr != nil {
				return nil, 0, perr
			}
			ads = append(ads, ad)
		}
		return ads, bytes, nil
	}
}

// runStoreNway is the left-deep chain run INSIDE the store, as an n-ary opcode would run it.
// The first step is the store's own two-table join -- projected reads, the index probe, the
// real pair loop -- and the second folds the third table against the composites that join
// produced, which is the step the opcode cannot express today. Nothing crosses a wire.
//
// It is an emulation, and in one way an optimistic one: the third side is read whole rather
// than projected, which costs the store arm rather than flattering it.
func runStoreNway(cat *db.Catalog, sides []nwaySide) ([]nwayRow, error) {
	var stats db.JoinStats
	seq, err := cat.Join(db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: sides[0].table, Alias: sides[0].alias, Project: sides[0].proj},
		Right: db.JoinSide{Table: sides[1].table, Alias: sides[1].alias, Project: sides[1].proj},
		Cond:  []string{sides[0].alias + ".ClusterId == " + sides[1].alias + ".ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}, db.JoinLimits{}, &stats)
	if err != nil {
		return nil, err
	}
	keyExpr, err := classad.ParseExpr(sides[0].alias + ".ClusterId")
	if err != nil {
		return nil, err
	}
	var rows []nwayRow
	for ad := range seq {
		n, ierr := keyExpr.Eval(ad).IntValue()
		if ierr != nil {
			continue
		}
		rows = append(rows, nwayRow{ad: ad, key: n})
	}
	if stats.Err != nil {
		return nil, stats.Err
	}

	if len(sides) == 2 {
		return rows, nil
	}
	s := sides[2]
	d, ok := cat.Table(s.table)
	if !ok {
		return nil, fmt.Errorf("no such table: %s", s.table)
	}
	ads, err := d.Query(nwayKeyConstraint(rows))
	if err != nil {
		return nil, err
	}
	buckets := map[int64][]*classad.ClassAd{}
	for ad := range ads {
		if k, ok := ad.EvaluateAttrInt("ClusterId"); ok {
			buckets[k] = append(buckets[k], nwayProject(ad, s.proj))
		}
	}
	out := make([]nwayRow, 0, len(rows))
	for _, row := range rows {
		for _, ad := range buckets[row.key] {
			merged := classad.New()
			for _, prev := range sides[:2] {
				if member, ok := row.ad.Lookup(prev.alias); ok {
					merged.InsertExpr(prev.alias, member)
				}
			}
			merged.InsertAttrClassAd(s.alias, ad)
			out = append(out, nwayRow{ad: merged, key: row.key})
		}
	}
	return out, nil
}

// nwayProject keeps only the attributes a side contributes to the joined row.
func nwayProject(ad *classad.ClassAd, proj []string) *classad.ClassAd {
	out := classad.New()
	for _, a := range proj {
		if e, ok := ad.Lookup(a); ok {
			out.InsertExpr(a, e)
		}
	}
	return out
}

// nwayOutputBytes is what a store-side n-way would have to ship: its joined rows, rendered as
// the join opcode renders them. The client arm never pays this, because its rows are formed
// where they are consumed.
func nwayOutputBytes(rows []nwayRow) int {
	n := 0
	for _, r := range rows {
		n += len(r.ad.String())
	}
	return n
}

// nwayVolume is what one plan puts on the wire, and how many rows it produces.
type nwayVolume struct {
	rows  int
	bytes int
}

// TestNwayWireVolumeGrowsWithTheChain states what moving a whole chain into the store does to
// the wire, which is the quantity that decides whether it is worth an opcode.
//
// A client joining n tables ships each table ONCE, narrowed to the keys the rows so far hold;
// its intermediate rows never cross anything, because they are formed where they are consumed.
// A store-side n-way ships only its output -- but every output row carries every side, so a
// chain that EXPANDS repeats earlier sides once per output row and ships more than the inputs
// were. The two directions are not symmetric guesses: each is arithmetic, and this measures
// both on one fixture so the ratio can be read against the chain's length rather than against
// another fixture's.
//
// The third plan is the one that needs no new opcode: push the first PAIR to the two-table
// join that already ships, and fold the last table client-side. It is here because a new
// capability has to beat the capability already present, not only the client.
func TestNwayWireVolumeGrowsWithTheChain(t *testing.T) {
	const jobs = 2048
	for _, tc := range []struct {
		metricEvery int
		shape       string
		storeWins   bool
	}{
		{1, "expands: a metrics row per job", false},
		{50, "filters: a metrics row per fiftieth job", true},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			cat, cleanup := nwayCatalog(t, jobs, tc.metricEvery)
			defer cleanup()
			srv := NewServerCatalog(cat)
			cconn, sconn := netPipe()
			go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
			c := NewClient(cconn, WithPeerSessionAttrs(srv.SessionAttrs()))
			defer func() { c.Close(); srv.Close() }()

			chain := nwayChain()
			client := func(sides []nwaySide) nwayVolume {
				rows, bytes, err := runNway(nwayWireFetch(c), sides)
				if err != nil {
					t.Fatalf("client-side chain: %v", err)
				}
				return nwayVolume{rows: len(rows), bytes: bytes}
			}
			store := func(sides []nwaySide) nwayVolume {
				rows, err := runStoreNway(cat, sides)
				if err != nil {
					t.Fatalf("store-side chain: %v", err)
				}
				return nwayVolume{rows: len(rows), bytes: nwayOutputBytes(rows)}
			}

			c2, s2 := client(chain[:2]), store(chain[:2])
			c3, s3 := client(chain), store(chain)
			if c2.rows != s2.rows || c3.rows != s3.rows {
				t.Fatalf("the plans disagree on the answer: 2-table %d vs %d, 3-table %d vs %d",
					c2.rows, s2.rows, c3.rows, s3.rows)
			}
			// The prefix push: the first pair joined in the store with the opcode that
			// already exists, the last table read and folded by the client.
			//
			// The tail is read unnarrowed, where a real prefix push would narrow it to the
			// keys the composite rows hold. That costs this arm nothing on either shape
			// here -- the expanding chain holds every key, so the narrowed read is the same
			// read, and the filtering chain's third table is 41 rows whole -- and the term
			// that decides the arm is the prefix's own output, which is measured exactly.
			_, tailBytes, err := runNway(nwayWireFetch(c), chain[2:])
			if err != nil {
				t.Fatalf("tail read: %v", err)
			}
			prefix := s2.bytes + tailBytes

			r2 := float64(s2.bytes) / float64(c2.bytes)
			r3 := float64(s3.bytes) / float64(c3.bytes)
			t.Logf("2 tables: %d rows; client ships %d bytes, the store would ship %d (%.2fx)",
				c2.rows, c2.bytes, s2.bytes, r2)
			t.Logf("3 tables: %d rows; client ships %d bytes, the store would ship %d (%.2fx)",
				c3.rows, c3.bytes, s3.bytes, r3)
			t.Logf("3 tables, pushing only the first PAIR to the opcode that exists: %d bytes (%.2fx)",
				prefix, float64(prefix)/float64(c3.bytes))

			// The other thing a planner can do without any new opcode: start the chain at
			// the most selective table, so every later read is narrowed to the keys that
			// survived. Join ORDER and join LOCATION are different levers, and this prices
			// them against each other on the same fixture.
			reordered := []nwaySide{chain[2], chain[0], chain[1]}
			cr, sr := client(reordered), store(reordered)
			if cr.rows != c3.rows {
				t.Fatalf("reordering changed the answer: %d rows, want %d", cr.rows, c3.rows)
			}
			t.Logf("3 tables, most selective FIRST, all client-side: %d bytes (%.2fx of the unordered client)",
				cr.bytes, float64(cr.bytes)/float64(c3.bytes))
			t.Logf("3 tables, most selective first and in the store: %d bytes", sr.bytes)

			if tc.storeWins {
				if s3.bytes >= c3.bytes {
					t.Errorf("a filtering chain should ship LESS from the store: %d vs the client's %d",
						s3.bytes, c3.bytes)
				}
				// The direction is not the point on its own; the chain must sharpen it,
				// or length is not an argument for an n-ary opcode.
				if r3 >= r2 {
					t.Errorf("the third table did not sharpen the saving: 2-table %.2fx, 3-table %.2fx", r2, r3)
				}
				// THE ONE THAT DECIDES IT. The chain the store beats is the chain the
				// client ran in the wrong order. Started at the selective table, every
				// later read is narrowed to the keys that survived and the client ships
				// only surviving rows, while the store still ships composites carrying
				// every side. If this ever stops holding, the case for an n-ary opcode
				// is open again and this is where to read why.
				if cr.bytes >= s3.bytes {
					t.Errorf("ordering the chain no longer beats moving it: reordered client "+
						"ships %d, the store-side n-way %d", cr.bytes, s3.bytes)
				}
				return
			}
			if s3.bytes <= c3.bytes {
				t.Errorf("an expanding chain should ship MORE from the store: %d vs the client's %d",
					s3.bytes, c3.bytes)
			}
			// The cost of being wrong about the shape grows with the chain. That is the
			// whole reason an n-ary opcode is not a free generalization of a two-table one.
			if r3 <= r2 {
				t.Errorf("the third table did not worsen the overshipping: 2-table %.2fx, 3-table %.2fx", r2, r3)
			}
		})
	}
}
