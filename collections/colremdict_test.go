package collections

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// remDictAd is one record of the remnant-dictionary fixture. Its non-literal values cover every
// repetition shape a block can hold:
//
//	MemoryUsage   the same expression in every record
//	Requirements  shared by a cluster's ten procs, different across clusters
//	Stats, Lst    a nested record (holding a record of its own) and a list, in a third of the records
//	Uniq          different in every record: never an entry
//	Rare          one record in 211: at most one per block, never an entry
//	Secret        sealed in an encrypted store; the same plaintext in every record
//
// Every expression references other attributes of the ad, so a reference resolved in the wrong id
// space reads a different attribute and evaluates to a different number.
func remDictAd(i int) string {
	cl := i / 10
	var b strings.Builder
	fmt.Fprintf(&b, `[ ClusterId=%d; ProcId=%d; Owner="user%d"; W=%d; X=%d; Y=%d; Z=%d; `,
		cl, i%10, cl%5, 1000+i%37, 2000+i%41, 3000+i*7, 4000+i%13)
	b.WriteString(`MemoryUsage=((Y + 1023) / 1024); `)
	fmt.Fprintf(&b, `Requirements=(TARGET.Memory >= W) && (X > %d) && (TARGET.Arch == "X86_64"); `, cl%6)
	if i%3 == 0 {
		b.WriteString(`Stats=[ A = W + 1; Inner = [ B = X * 2; C = "files" ]; D = { Y, 3 } ]; Lst={ W, "x", 7 }; `)
	}
	fmt.Fprintf(&b, `Uniq=W + %d; `, i)
	if i%211 == 7 {
		b.WriteString(`Rare=Z * 2; `)
	}
	b.WriteString(`Secret=strcat("tok-", "shared"); Last=1 ]`)
	return b.String()
}

// remDictCheckValues asserts ad i reads back as written: every attribute's expression text, and the
// values of the expressions whose references must resolve to the right attributes.
func remDictCheckValues(t *testing.T, gen func(int) string, i int, ad *classad.ClassAd, when string) int {
	t.Helper()
	want := mustAd(t, gen(i))
	if got, w := canonAd(ad), canonAd(want); got != w {
		t.Errorf("%s: record %d reads as\n%s\nwant\n%s", when, i, got, w)
		return 1
	}
	bad := 0
	for _, name := range []string{"MemoryUsage", "Uniq", "Rare"} {
		gv, gok := ad.EvaluateAttrInt(name)
		wv, wok := want.EvaluateAttrInt(name)
		if gok != wok || gv != wv {
			t.Errorf("%s: record %d %s = %d (ok=%v), want %d (ok=%v)", when, i, name, gv, gok, wv, wok)
			bad++
		}
	}
	return bad
}

// remDictShape is one store configuration the differential runs under.
type remDictShape struct {
	name     string
	archive  bool
	interned bool // segments keyed by their own dictionary (RetrainDict / InternAtSeal)
	encrypt  bool // Secret sealed at rest
	groups   bool // group schemas on
	rowGroup int  // RowGroupBytes; small values give several blocks per segment, each with its own dictionary
	// decoys makes the WRITER number names apart from both its records' order and any segment
	// dictionary's, by interning remDictDecoys backwards before anything else. Without it the writer's
	// ids and an interned segment's ids coincide, and a reference written in the wrong one of the two
	// reads back right.
	decoys bool
}

var remDictShapes = []remDictShape{
	{name: "mutable"},
	{name: "mutable-interned", interned: true, rowGroup: 1600},
	{name: "mutable-encrypted", encrypt: true, rowGroup: 2400},
	{name: "mutable-groups", groups: true, interned: true},
	{name: "archive", archive: true, rowGroup: 2000},
	{name: "archive-interned", archive: true, interned: true},
}

// remDictStore is a store built for one shape, columnarized, closed and reopened, so every payload and
// remnant is read back from disk.
type remDictStore struct {
	c    *Collection
	dir  string
	keys []string // in append order; nil for an archive
}

func (s remDictShape) options(dir string, dict int) Options {
	o := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, RowGroupBytes: s.rowGroup,
		RemnantDictionary: dict}
	if s.groups {
		o.GroupSchemaCount, o.GroupStabilityRuns = 4, 1
	}
	if s.encrypt {
		o.DataKey = remDictDataKey
		o.EncryptedAttrs = []string{"Secret"}
	}
	return o
}

// remDictDataKey is a fixed data key, so both arms of a differential share it.
var remDictDataKey = bytes.Repeat([]byte{7}, 32)

func (s remDictShape) archiveOptions(dir string, dict int) ArchiveOptions {
	o := ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, GroupSchemaCount: -1, RowGroupBytes: s.rowGroup,
		InternAtSeal: s.interned, RemnantDictionary: dict}
	if s.groups {
		o.GroupSchemaCount, o.GroupStabilityRuns = 4, 1
	}
	return o
}

// buildRemDictStore writes n records of gen into a store of shape s with Options.RemnantDictionary set
// to dict, columnarizes every sealed segment, and reopens it -- with reopen applied to the options, so a
// caller can make the reopened process number names differently from the writer.
func buildRemDictStore(t *testing.T, s remDictShape, dict, n int, gen func(int) string, reopen func(*Options)) remDictStore {
	t.Helper()
	dir := t.TempDir()
	st := remDictStore{dir: dir}
	// A freshly derived schema breaks ties between fields of one kind and width by intern id, and a
	// persistent store interns record names as the derivation meets them, in no fixed order -- so two
	// stores built from the same records can lay their columns out differently and reassemble the
	// same ad with its attributes in a different order. Interning every name up front, in ad order,
	// gives both arms of a differential one layout, so their reads can be compared byte for byte.
	pin := func(c *Collection) {
		if s.decoys {
			for i := len(remDictDecoys) - 1; i >= 0; i-- {
				c.intern.Intern(remDictDecoys[i])
			}
		}
		for i := range min(n, 400) {
			for _, a := range mustAd(t, gen(i)).AST().Attributes {
				c.intern.Intern(a.Name)
			}
		}
	}
	if s.archive {
		a, err := CreateArchive(s.archiveOptions(dir, dict))
		if err != nil {
			t.Fatal(err)
		}
		pin(a.c)
		for i := range n {
			if err = a.Append(mustAd(t, gen(i))); err != nil {
				t.Fatal(err)
			}
		}
		a.c.colBudget = 1 << 20
		if !a.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("schema scan did not enable")
		}
		for a.c.ColumnarizeSealed() > 0 {
		}
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		ao := s.archiveOptions(dir, dict)
		if reopen != nil {
			o := Options{}
			reopen(&o)
			ao.CategoricalAttrs, ao.HotAttrs = o.CategoricalAttrs, o.HotAttrs
		}
		if a, err = OpenArchive(ao); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		st.c = a.c
		return st
	}
	c, err := Open(s.options(dir, dict))
	if err != nil {
		t.Fatal(err)
	}
	pin(c)
	for i := range n {
		k := fmt.Sprintf("%d.0", i)
		st.keys = append(st.keys, k)
		if err = c.Put([]byte(k), mustAd(t, gen(i))); err != nil {
			t.Fatal(err)
		}
	}
	if s.interned {
		if _, err = c.RetrainDict(0); err != nil {
			t.Fatal(err)
		}
	}
	c.colBudget = 1 << 20
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	for c.ColumnarizeSealed() > 0 {
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	o := s.options(dir, dict)
	if reopen != nil {
		reopen(&o)
	}
	if c, err = Open(o); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	st.c = c
	return st
}

// remDictCensus is what a store holds on disk: columnarized segments, payload versions, blocks carrying
// a dictionary, their sizes, and records whose stored remnant holds a reference.
type remDictCensus struct {
	segs, v2, blocks, dictBlocks, entries, refRecords, oddBlocks, recs int
}

func takeRemDictCensus(t *testing.T, c *Collection) remDictCensus {
	t.Helper()
	var r remDictCensus
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			r.segs++
			r.recs += cn.seg.offsLen()
			if _, v, err := splitColPayload(recAd(seg.data, cn.rec)); err == nil && v == colPayloadVersionRemDict {
				r.v2++
			}
			k := 0
			for _, b := range cn.seg.blocks {
				r.blocks++
				if b.n%8 != 0 {
					r.oddBlocks++
				}
				if b.remDictN > 0 {
					r.dictBlocks++
					r.entries += b.remDictN
				}
				for i := 0; i < b.n; i++ {
					raw, err := seg.codec.Decompress(nil, recAd(seg.data, cn.seg.offAt(k+i)))
					if err != nil {
						t.Fatal(err)
					}
					_, ents, n, inline, ok := wire.Ad(raw).SplitBody()
					if !ok {
						t.Fatal("a stored remnant does not split")
					}
					holds := false
					remnantEntries(ents, n, inline, func(_, node []byte) bool {
						holds = holds || node[0] == wire.TagExternalRef
						return true
					})
					if holds {
						r.refRecords++
					}
				}
				k += b.n
			}
		}
	}
	return r
}

// remDictQueries are the constraints the read paths are compared under: over the dictionary-referenced
// attributes, over ones that never are, and over a nested record's member.
var remDictQueries = []string{
	"true",
	"MemoryUsage > 8",
	"Requirements =!= undefined",
	"Stats.A > 1010",
	"Stats.Inner.B > 4040",
	"size(Lst) == 3",
	"Rare is undefined",
	"Rare isnt undefined",
	"Uniq > 1500",
	`Owner == "user2" && MemoryUsage > 10`,
}

var remDictProjection = []string{"MemoryUsage", "Requirements", "Stats", "Lst", "Uniq", "W", "Owner"}

// remDictReads runs every read path over c and returns what each produced, as comparable strings: raw
// wire bytes as hex, everything else rendered. The fast-path counters are included, so a reader that
// declines to the slow path on a dictionary segment -- correct but no longer fast -- shows as a
// difference too.
func remDictReads(t *testing.T, c *Collection, keys []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	add := func(path, v string) { out[path] = append(out[path], v) }
	stats := func(path string, st *ScanStats) {
		add(path+"/stats", fmt.Sprintf("visited=%d reassembled=%d narrow=%d decided=%d matched=%d",
			st.RecordsVisited, st.RecordsReassembled, st.RecordsNarrowMatched, st.RecordsColumnDecided, st.RowsMatched))
	}
	for w := range c.ScanRawWire(nil, false) {
		add("ScanRawWire", hex.EncodeToString(w))
	}
	for w := range c.ScanRawWire(nil, true) {
		add("ScanRawWire/redacted", hex.EncodeToString(w))
	}
	for w := range c.ScanRawWire(remDictProjection, false) {
		add("ScanRawWire/projected", hex.EncodeToString(w))
	}
	for ra := range c.ScanRaw() {
		add("ScanRaw", string(bytes.Join(ra.Exprs, []byte("\n")))+ra.MyType+ra.TargetType)
	}
	for ad := range c.Scan() {
		add("Scan", canonAd(ad))
	}
	for _, k := range keys {
		ad, ok := c.Get([]byte(k))
		if !ok {
			add("Get", k+" missing")
			continue
		}
		add("Get", canonAd(ad))
	}
	for _, expr := range remDictQueries {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		var st ScanStats
		for w := range c.QueryRawWireStats(q, nil, false, &st) {
			add("QueryRawWire "+expr, hex.EncodeToString(w))
		}
		stats("QueryRawWire "+expr, &st)
		st = ScanStats{}
		for w := range c.QueryRawWireStats(q, remDictProjection, false, &st) {
			add("QueryRawWire/projected "+expr, hex.EncodeToString(w))
		}
		stats("QueryRawWire/projected "+expr, &st)
		for w := range c.QueryRawWireRefs(q, []string{"Requirements"}, false) {
			add("QueryRawWireRefs "+expr, hex.EncodeToString(w))
		}
		st = ScanStats{}
		for ra := range c.QueryRawProjectedStats(q, remDictProjection, true, false, &st) {
			add("QueryRawProjected "+expr, string(bytes.Join(ra.Exprs, []byte("\n"))))
		}
		stats("QueryRawProjected "+expr, &st)
		for ad := range c.Query(q) {
			add("Query "+expr, canonAd(ad))
		}
		for ad := range c.QueryRedacted(q) {
			add("QueryRedacted "+expr, canonAd(ad))
		}
		for vals := range c.QueryProject(q, []string{"MemoryUsage", "Uniq", "Requirements"}) {
			add("QueryProject "+expr, fmt.Sprint(vals))
		}
		n, ok := c.CountQuery(q)
		add("CountQuery "+expr, fmt.Sprint(n, ok))
		gc, ok := c.GroupCountQuery(q, "Owner")
		add("GroupCountQuery "+expr, fmt.Sprint(gc, ok))
		gs, ok := c.GroupStatsQuery(q, "Owner", []string{"MemoryUsage", "Uniq"})
		add("GroupStatsQuery "+expr, fmt.Sprint(gs, ok))
	}
	return out
}

// diffRemDictReads compares two remDictReads results path by path, and fails if any path named in
// nonEmpty produced nothing -- a comparison of two empty results proves nothing.
func diffRemDictReads(t *testing.T, label string, off, on map[string][]string, nonEmpty ...string) {
	t.Helper()
	paths := make([]string, 0, len(off))
	for p := range off {
		paths = append(paths, p)
	}
	for p := range on {
		if _, ok := off[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	bad := 0
	for _, p := range paths {
		a, b := off[p], on[p]
		if len(a) != len(b) {
			t.Errorf("%s %s: %d results without the dictionary, %d with it", label, p, len(a), len(b))
			bad++
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s %s: result %d differs\n without: %.300s\n    with: %.300s", label, p, i, a[i], b[i])
				bad++
				break
			}
		}
		if bad > 10 {
			t.Fatalf("%s: too many differences", label)
		}
	}
	for _, p := range append([]string{"ScanRawWire", "Scan"}, nonEmpty...) {
		if len(off[p]) == 0 {
			t.Errorf("%s %s: no results; the comparison proves nothing", label, p)
		}
	}
}

// TestRemnantDictReadsByteIdentical is the contract: a store whose columnarized records hold remnant
// dictionary references reads back byte for byte what the same store without them does, on every read
// path, in every store shape -- mutable, interned, encrypted, with group schemas, archive, interned
// archive -- with row groups both on and off the 8-record alignment. It also proves the dictionary was
// really there, so a store that quietly wrote none cannot pass.
func TestRemnantDictReadsByteIdentical(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	var oddBlocks atomic.Int64
	t.Run("shapes", func(t *testing.T) {
		for _, s := range remDictShapes {
			remDictByteIdenticalShape(t, s, &oddBlocks)
		}
	})
	if oddBlocks.Load() == 0 {
		t.Errorf("every block of every shape is a multiple of 8 records: the off-alignment case is not covered")
	}
}

func remDictByteIdenticalShape(t *testing.T, s remDictShape, oddBlocks *atomic.Int64) {
	t.Run(s.name, func(t *testing.T) {
		t.Parallel()
		const n = 1500
		off := buildRemDictStore(t, s, -1, n, remDictAd, nil)
		on := buildRemDictStore(t, s, 1, n, remDictAd, nil)
		if c := takeRemDictCensus(t, off.c); c.v2 != 0 || c.dictBlocks != 0 || c.refRecords != 0 {
			t.Fatalf("store without the dictionary holds one: %+v", c)
		}
		c := takeRemDictCensus(t, on.c)
		t.Logf("%s: %+v", s.name, c)
		if c.segs == 0 || c.v2 != c.segs || c.dictBlocks == 0 || c.refRecords == 0 {
			t.Fatalf("no dictionary was written: %+v", c)
		}
		oddBlocks.Add(int64(c.oddBlocks))
		if s.rowGroup > 0 && c.dictBlocks <= c.segs {
			// Several dictionaries per segment, so a record resolved against another block's
			// dictionary reads another block's nodes.
			t.Fatalf("one dictionary per segment: the per-block case is not covered: %+v", c)
		}
		diffRemDictReads(t, s.name, remDictReads(t, off.c, off.keys), remDictReads(t, on.c, on.keys),
			"QueryRawWire/projected MemoryUsage > 8", "QueryRawWire Stats.Inner.B > 4040", "Query Rare isnt undefined")
		// The held-cell index is built from the remnants: it must read them, not give up on them.
		for _, sh := range on.c.shards {
			for _, seg := range sh.segs {
				if cn := seg.colNative.Load(); cn != nil {
					if h := cn.seg.held(); h == nil || !h.ok {
						t.Fatalf("the held-cell index could not read the remnants of %s", seg.path)
					}
				}
			}
		}
	})
}

// TestRemnantDictRepetitionShapes runs the differential over blocks where every record repeats, where
// none does, and where exactly one pair does: the dictionary must hold exactly what repeats and the reads
// must not change.
func TestRemnantDictRepetitionShapes(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	gens := map[string]struct {
		gen  func(int) string
		dict bool // whether a dictionary is expected at all
	}{
		"every": {func(i int) string {
			return fmt.Sprintf(`[ ProcId=%d; W=%d; Req=(W > 3) && (TARGET.Memory > W); M=((W + 1023) / 1024) ]`, i, i%50)
		}, true},
		"none": {func(i int) string {
			return fmt.Sprintf(`[ ProcId=%d; W=%d; Req=(W > %d) && (TARGET.Memory > W) ]`, i, i%50, i)
		}, false},
		"one-pair": {func(i int) string {
			req := fmt.Sprintf("(W > %d)", i)
			if i == 2 || i == 5 {
				req = "(W >= 0) && (TARGET.Memory > W * 1000)"
			}
			return fmt.Sprintf(`[ ProcId=%d; W=%d; Req=%s ]`, i, i%50, req)
		}, true},
	}
	for name, g := range gens {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := remDictShape{name: name}
			off := buildRemDictStore(t, s, -1, 3000, g.gen, nil)
			on := buildRemDictStore(t, s, 1, 3000, g.gen, nil)
			c := takeRemDictCensus(t, on.c)
			t.Logf("%s: %+v", name, c)
			switch {
			case !g.dict && (c.dictBlocks != 0 || c.v2 != 0):
				t.Fatalf("a store where nothing repeats wrote a dictionary: %+v", c)
			case g.dict && c.dictBlocks == 0:
				t.Fatalf("no dictionary: %+v", c)
			case name == "one-pair" && (c.entries != 1 || c.refRecords != 2):
				t.Fatalf("one repeated pair: %d entries over %d records, want 1 over 2", c.entries, c.refRecords)
			}
			for i := range 3000 {
				ad, ok := on.c.Get([]byte(fmt.Sprintf("%d.0", i)))
				if !ok {
					t.Fatalf("record %d missing", i)
				}
				if got, want := canonAd(ad), canonAd(mustAd(t, g.gen(i))); got != want {
					t.Fatalf("record %d reads as %s, want %s", i, got, want)
				}
			}
			diffRemDictReads(t, name, remDictReads(t, off.c, off.keys), remDictReads(t, on.c, on.keys))
		})
	}
}

// remDictDecoys are interned first by a reopened process, so it numbers every name differently from
// the process that wrote the store: a reference resolved through this process's ids instead of the
// record's reads another attribute.
var remDictDecoys = []string{"Decoy0", "Decoy1", "Uniq", "Z", "Y", "X", "W", "Rare", "MemoryUsage", "Requirements",
	"Stats", "Lst", "Owner", "ProcId", "ClusterId", "Secret", "Last", "Decoy2", "A", "B", "Inner"}

// TestRemnantDictAcrossDivergentIDs reads dictionary-bearing stores back in a process whose intern ids
// differ from the writer's -- and differ from each segment dictionary's -- and asserts every value, not
// merely that the reads succeed. The writer's and reader's ids are checked to really differ.
func TestRemnantDictAcrossDivergentIDs(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, s := range []remDictShape{
		{name: "archive-interned", archive: true, interned: true, decoys: true},
		{name: "archive", archive: true, decoys: true},
		{name: "mutable-interned", interned: true, rowGroup: 1600, decoys: true},
	} {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			const n = 1200
			st := buildRemDictStore(t, s, 1, n, remDictAd, func(o *Options) {
				o.CategoricalAttrs = remDictDecoys
			})
			c := st.c
			if cs := takeRemDictCensus(t, c); cs.refRecords == 0 {
				t.Fatalf("no record holds a reference: %+v", cs)
			}
			// The ids really diverged: this process numbered the decoys before any name the writer met,
			// and in an interned segment the dictionary's id for W differs from this process's.
			if d, _ := c.intern.LookupID("Decoy0"); d > mustID(t, c, "W") {
				t.Fatal("the reopened process met W before the decoys; its ids did not diverge")
			}
			if s.interned {
				diverged := false
				for _, sh := range c.shards {
					for _, seg := range sh.segs {
						if d := seg.dict.Load(); d != nil {
							lid, _ := d.lookup("W")
							gid, _ := c.intern.LookupID("W")
							diverged = diverged || lid != gid
						}
					}
				}
				if !diverged {
					t.Fatal("no segment dictionary numbers W differently from this process")
				}
			}
			// An archive scans newest first, so each record is identified by its own ClusterId and ProcId.
			seen, bad := map[int]bool{}, 0
			for ad := range c.Scan() {
				cl, _ := ad.EvaluateAttrInt("ClusterId")
				p, _ := ad.EvaluateAttrInt("ProcId")
				i := int(cl*10 + p)
				seen[i] = true
				bad += remDictCheckValues(t, remDictAd, i, ad, s.name)
				if bad > 5 {
					t.Fatal("too many wrong values")
				}
			}
			if len(seen) != n {
				t.Fatalf("scan returned %d distinct ads, want %d", len(seen), n)
			}
		})
	}
}

// TestRemnantDictTwoSchemasMidConvergence is the store state in which an id-space mistake shows: a
// base-schema change converging one segment at a time leaves two schemas on disk, and the next process
// numbers names by whichever segment it meets first. One segment is re-columnarized under the new
// schema -- which reads its records through their old dictionary and writes a new one -- and every
// value must read back right in that process and in a fresh one that interned decoys first.
func TestRemnantDictTwoSchemasMidConvergence(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 2000
	opts := func(budget int) Options {
		return Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, ColumnarSegmentBudget: budget,
			RemnantDictionary: 1}
	}
	check := func(c *Collection, when string) {
		t.Helper()
		bad := 0
		for i := range n {
			ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i)))
			if !ok {
				t.Fatalf("%s: record %d missing", when, i)
			}
			bad += remDictCheckValues(t, remDictAd, i, ad, when)
			if bad > 5 {
				t.Fatalf("%s: too many wrong values", when)
			}
		}
	}
	c, err := Open(opts(0))
	if err != nil {
		t.Fatal(err)
	}
	// The writer numbers names apart from its records' order, so its ids and each segment dictionary's
	// differ from the start.
	for i := len(remDictDecoys) - 1; i >= 0; i-- {
		c.intern.Intern(remDictDecoys[i])
	}
	for i := range n {
		if err = c.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, remDictAd(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = c.RetrainDict(0); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 4) {
		c.Close()
		t.Skip("schema scan did not enable")
	}
	for c.ColumnarizeSealed() > 0 {
	}
	check(c, "writer")
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(opts(1))
	if err != nil {
		t.Fatal(err)
	}
	st := c2.schemaScan.Load()
	if st == nil {
		c2.Close()
		t.Skip("no schema adopted on reopen")
	}
	fields := make([]adField, 0, len(st.schema.fields))
	changed := false
	for _, f := range st.schema.fields {
		nf := adField{id: f.id, kind: f.kind, width: f.width, unsigned: f.unsigned}
		if nm, _ := c2.schemaFieldName(f.id); nm == "W" && nf.kind == akInt {
			nf.width, changed = 8, true // a width change moves W in the record layout
		}
		fields = append(fields, nf)
	}
	if !changed {
		c2.Close()
		t.Skip("W is not an int field of the schema")
	}
	if !c2.installSchemaScan(layoutSchema(fields), nil) {
		t.Fatal("install failed")
	}
	if got := c2.recolumnarizeStale(); got == 0 {
		t.Fatal("no segment converged; the two-schema state was not reached")
	}
	if cs := takeRemDictCensus(t, c2); cs.refRecords == 0 {
		t.Fatalf("no record holds a reference after convergence: %+v", cs)
	}
	check(c2, "mid-convergence")
	if err = c2.Close(); err != nil {
		t.Fatal(err)
	}

	o := opts(0)
	o.CategoricalAttrs = remDictDecoys
	c3, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	check(c3, "reopened with decoys")
}

// TestRemnantDictWithComputedCells: a columnarized segment carrying both computed values for its
// expression-valued schema fields and a remnant dictionary for the expressions themselves answers every
// constraint, and every full read, exactly as one carrying computed values alone.
func TestRemnantDictWithComputedCells(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	gen := func(i int) string { return computedAd(i, 211) }
	for _, s := range []remDictShape{{name: "mutable"}, {name: "archive-interned", archive: true, interned: true}} {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			off := buildRemDictStore(t, s, -1, 1800, gen, nil)
			on := buildRemDictStore(t, s, 1, 1800, gen, nil)
			computed := 0
			for _, sh := range on.c.shards {
				for _, seg := range sh.segs {
					if cn := seg.colNative.Load(); cn != nil && cn.seg.hasComputed() {
						computed++
					}
				}
			}
			cs := takeRemDictCensus(t, on.c)
			if computed == 0 || cs.refRecords == 0 {
				t.Fatalf("want both computed values and dictionary references: %d segments with computed values, %+v", computed, cs)
			}
			for _, expr := range computedQueries {
				q, err := vm.Parse(expr)
				if err != nil {
					t.Fatal(err)
				}
				a, aok := off.c.CountQuery(q)
				b, bok := on.c.CountQuery(q)
				if a != b || aok != bok {
					t.Errorf("CountQuery(%s) = %d,%v with the dictionary, %d,%v without", expr, b, bok, a, aok)
				}
				var sa, sb ScanStats
				var wa, wb []string
				for w := range off.c.QueryRawWireStats(q, []string{"MemUse", "Label", "Big"}, false, &sa) {
					wa = append(wa, hex.EncodeToString(w))
				}
				for w := range on.c.QueryRawWireStats(q, []string{"MemUse", "Label", "Big"}, false, &sb) {
					wb = append(wb, hex.EncodeToString(w))
				}
				if strings.Join(wa, ",") != strings.Join(wb, ",") {
					t.Errorf("QueryRawWire(%s) differs with the dictionary", expr)
				}
				if sa.RecordsReassembled != sb.RecordsReassembled || sa.RecordsColumnDecided != sb.RecordsColumnDecided {
					t.Errorf("QueryRawWire(%s): reassembled/decided %d/%d with the dictionary, %d/%d without",
						expr, sb.RecordsReassembled, sb.RecordsColumnDecided, sa.RecordsReassembled, sa.RecordsColumnDecided)
				}
			}
			var wa, wb []string
			for w := range off.c.ScanRawWire(nil, false) {
				wa = append(wa, hex.EncodeToString(w))
			}
			for w := range on.c.ScanRawWire(nil, false) {
				wb = append(wb, hex.EncodeToString(w))
			}
			if len(wa) != 1800 || strings.Join(wa, ",") != strings.Join(wb, ",") {
				t.Errorf("full scan: %d ads without the dictionary, %d with, or different bytes", len(wa), len(wb))
			}
		})
	}
}

// remDictBody encodes an inline-keyed remnant body from (name, node) pairs, as stripSchemaAttrs leaves one.
func remDictBody(t *testing.T, attrs ...any) []byte {
	t.Helper()
	var ents []byte
	for i := 0; i < len(attrs); i += 2 {
		ents = wire.AppendKey(ents, true, 0, attrs[i].(string))
		ents = append(ents, attrs[i+1].([]byte)...)
	}
	full := wire.EncodeInline(nil, mustAd(t, "[ X = 1 ]").AST())
	hdr, _, _, _, ok := wire.Ad(full).SplitBody()
	if !ok {
		t.Fatal("header")
	}
	return wire.BuildAd(nil, hdr, len(attrs)/2, ents)
}

// exprNode is the inline wire node of a ClassAd expression.
func exprNode(t *testing.T, expr string) []byte {
	t.Helper()
	w := wire.EncodeInline(nil, mustAd(t, "[ V = "+expr+" ]").AST())
	var node []byte
	wire.Ad(w).ForEachRaw(func(_ uint32, _ string, n []byte) bool { node = append([]byte(nil), n...); return false })
	return node
}

// sealedNode is a well-formed sealed node with fixed bytes, as two records would carry if their
// ciphertexts happened to coincide.
func sealedNode() []byte {
	n := []byte{0x17}
	n = binary.AppendUvarint(n, 12)
	n = append(n, bytes.Repeat([]byte{9}, 12)...)
	n = binary.AppendUvarint(n, 20)
	return append(n, bytes.Repeat([]byte{5}, 20)...)
}

// TestBuildRemnantDictSelection pins what may become an entry: a repeated non-literal node that is longer
// than its reference. Never a sealed node, even one repeated byte for byte; never a node seen once; and
// two expressions differing only in how an attribute is encoded are separate entries.
func TestBuildRemnantDictSelection(t *testing.T) {
	expr := exprNode(t, "(W + 1023) / 1024")
	other := exprNode(t, "(V + 1023) / 1024") // the same shape, another attribute: must not collide
	once := exprNode(t, "W * 77")
	sealed := sealedNode()
	if !wire.IsEncryptedNode(sealed) {
		t.Fatal("the sealed fixture is not a sealed node")
	}
	bodies := [][]byte{
		remDictBody(t, "M", expr, "S", sealed, "O", once),
		remDictBody(t, "M", expr, "S", sealed, "N", other),
		remDictBody(t, "M", other, "S", sealed),
	}
	orig := make([][]byte, len(bodies))
	for i, b := range bodies {
		orig[i] = append([]byte(nil), b...)
	}
	entries := buildRemnantDict(bodies)
	if len(entries) != 2 || !bytes.Equal(entries[0], expr) || !bytes.Equal(entries[1], other) {
		t.Fatalf("entries %x, want exactly [%x %x] in first-occurrence order", entries, expr, other)
	}
	for _, e := range entries {
		if wire.IsEncryptedNode(e) {
			t.Fatal("a sealed node became a dictionary entry")
		}
	}
	region := remnantDictRegion(entries)
	for i, b := range bodies {
		if bytes.Equal(b, orig[i]) {
			t.Fatalf("body %d was not rewritten", i)
		}
		if !bytes.Contains(b, sealed) {
			t.Fatalf("body %d lost its sealed node to the dictionary", i)
		}
		got, expanded, err := expandRemnantWith(b, region, len(entries), nil)
		if err != nil || !expanded || !bytes.Equal(got, orig[i]) {
			t.Fatalf("body %d expands to %x (expanded=%v err=%v), want %x", i, got, expanded, err, orig[i])
		}
	}
	// Nothing repeats: no dictionary, nothing rewritten.
	solo := [][]byte{remDictBody(t, "M", expr), remDictBody(t, "M", other)}
	before := append([]byte(nil), solo[0]...)
	if e := buildRemnantDict(solo); e != nil || !bytes.Equal(solo[0], before) {
		t.Fatalf("a block where nothing repeats got entries %x", e)
	}
}

// TestExpandRemnantRefusesUnresolvedReferences: a reference with no entry behind it -- out of range, into
// a region too short to hold its offsets, to an entry that is itself a reference, or on a block with no
// dictionary at all -- fails the read. It is never handed on as a value.
func TestExpandRemnantRefusesUnresolvedReferences(t *testing.T) {
	expr := exprNode(t, "(W + 1023) / 1024")
	ref := func(j uint64) []byte { return binary.AppendUvarint([]byte{wire.TagExternalRef}, j) }
	region := remnantDictRegion([][]byte{expr})
	body := remDictBody(t, "M", ref(0))
	if got, ok, err := expandRemnantWith(body, region, 1, nil); err != nil || !ok || !bytes.Equal(got, remDictBody(t, "M", expr)) {
		t.Fatalf("a valid reference: %x %v %v", got, ok, err)
	}
	cases := map[string]struct {
		body   []byte
		region []byte
		n      int
	}{
		"index past the end":     {remDictBody(t, "M", ref(1)), region, 1},
		"huge index":             {remDictBody(t, "M", ref(1<<40)), region, 1},
		"short region":           {body, region[:6], 1},
		"entry is a reference":   {body, remnantDictRegion([][]byte{ref(0)}), 1},
		"truncated reference":    {remDictBody(t, "M", []byte{wire.TagExternalRef}), region, 1},
		"region claims too many": {body, region, 9},
	}
	for name, cse := range cases {
		if got, _, err := expandRemnantWith(cse.body, cse.region, cse.n, nil); err == nil {
			t.Errorf("%s: expanded to %x, want errRemnantRef", name, got)
		}
	}
	// A block without a dictionary hands the bytes on untouched -- and wire refuses them, rather than
	// read the reference as a value.
	var b columnarBlock
	got, expanded, err := b.expandRemnant(body, nil, nil)
	if err != nil || expanded || !bytes.Equal(got, body) {
		t.Fatalf("no dictionary: %x %v %v", got, expanded, err)
	}
	if _, derr := wire.DecodeInline(got); derr == nil {
		t.Fatal("wire decoded a reference as a value")
	}
}

// TestRemnantDictPayloadVersion: a payload whose blocks carry a dictionary is version 2, one whose blocks
// do not is still version 1 (readable by older builds); and a version 2 payload whose dictionary section
// is damaged is refused as a whole -- its records fail to read rather than read with references in them.
func TestRemnantDictPayloadVersion(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	st := buildRemDictStore(t, remDictShape{name: "v"}, 1, 900, remDictAd, nil)
	cs := takeRemDictCensus(t, st.c)
	if cs.v2 == 0 || cs.v2 != cs.segs {
		t.Fatalf("payload versions: %+v", cs)
	}
	none := buildRemDictStore(t, remDictShape{name: "v1"}, 1, 3000, func(i int) string {
		return fmt.Sprintf(`[ ProcId=%d; W=%d; U=W + %d ]`, i, i%9, i)
	}, nil)
	if c := takeRemDictCensus(t, none.c); c.segs == 0 || c.v2 != 0 {
		t.Fatalf("a store with nothing to share wrote version 2: %+v", c)
	}

	// Damage the dictionary section's magic in every payload, fix the record CRC so only the section is
	// wrong, and reopen.
	dir := st.dir
	if err := st.c.Close(); err != nil {
		t.Fatal(err)
	}
	damaged := 0
	editPayloads(t, dir, func(p string, blob []byte) []byte {
		i := bytes.Index(blob, binary.LittleEndian.AppendUint32(nil, colRemDictSection))
		if i < 0 {
			return nil
		}
		nb := append([]byte(nil), blob...)
		nb[i] ^= 0xff
		damaged++
		return nb
	})
	if damaged == 0 {
		t.Fatal("no dictionary section found on disk")
	}
	c, err := Open(remDictShape{}.options(dir, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Every record of a segment whose section is damaged is refused; every other record reads, right.
	got := 0
	c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		var i int
		if _, err := fmt.Sscanf(key, "%d.0", &i); err != nil {
			t.Fatalf("key %q", key)
		}
		remDictCheckValues(t, remDictAd, i, ad, "after damage")
		got++
		return true
	})
	if want := 900 - cs.recs; got != want {
		t.Fatalf("%d records read with %d payloads' dictionary sections damaged, want %d (none of their %d records)",
			got, damaged, want, cs.recs)
	}
}

// TestHeldIndexRefusesRemnantItCannotWalk: a remnant the held-cell pass cannot walk to its end -- here,
// references left unexpanded because their block is made to look dictionary-less -- makes the whole index
// untrusted. Indexing the attributes before the stop and not the ones after would answer UNDEFINED for
// cells the record holds.
func TestHeldIndexRefusesRemnantItCannotWalk(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	st := buildRemDictStore(t, remDictShape{name: "held"}, 1, 900, remDictAd, nil)
	checked := 0
	for _, sh := range st.c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil || !cn.seg.hasRemnantDict() {
				continue
			}
			if _, ok := cn.seg.scanHeld(); !ok {
				t.Fatalf("%s: the held-cell pass refused a readable segment", seg.path)
			}
			saved := make([]int, len(cn.seg.blocks))
			for i, b := range cn.seg.blocks {
				saved[i], b.remDictN = b.remDictN, 0
			}
			_, ok := cn.seg.scanHeld()
			for i, b := range cn.seg.blocks {
				b.remDictN = saved[i]
			}
			if ok {
				t.Fatalf("%s: the held-cell pass trusted remnants it could not walk to their end", seg.path)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no segment with a remnant dictionary")
	}
}
