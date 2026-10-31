package collections

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// computedAd is one record of the computed-values fixture. Its fields cover the shapes the computed
// cells must get exactly right, each mixing literal, computed and uncomputed cells in the same blocks:
//
//	MemUse  ((Rss + 1023) / 1024) mostly; a literal in some records; UNDEFINED where Rss is absent;
//	        Rss itself an expression in some; `TARGET.Memory / 2` every excEvery records, which no
//	        record can compute and the readers must evaluate one record at a time
//	Label   two different string expressions and a literal, so a block holds several expression texts
//	Big     integers past 2^53, as a literal and as an expression
//	Ratio   a real result where most literals are integers
//	Flag    a boolean result
//	Age     an expression on the clock, never computed
//	Loop    a literal, and in a few records a cyclic reference that evaluates to ERROR
func computedAd(i, excEvery int) string {
	rss := fmt.Sprint((i * 7919) % (1 << 22))
	switch {
	case i%53 == 0:
		rss = "" // absent: MemUse is UNDEFINED
	case i%17 == 0:
		rss = "RssBase * 2"
	case i%97 == 0:
		rss += ".5" // a real: MemUse evaluates to a real where its neighbours are integers
	}
	mem := "((Rss + 1023) / 1024)"
	switch {
	case excEvery > 0 && i%excEvery == excEvery/2:
		mem = "TARGET.Memory / 2"
	case i%10 == 3:
		mem = fmt.Sprint(i % 9000)
	}
	label := fmt.Sprintf("%q", fmt.Sprintf("lit%d", i%5))
	switch i % 3 {
	case 0:
		label = `strcat(Owner, "-", Site)`
	case 1:
		label = "toUpper(Site)"
	}
	big := fmt.Sprint(9007199254740993 + int64(i)*3)
	if i%2 == 0 {
		big = "BigBase + 1"
	}
	ratio := fmt.Sprintf("%d.25", i%50)
	if i%4 != 0 {
		ratio = "Rss / 3.0"
	}
	loop := fmt.Sprint(i % 7)
	if i%131 == 9 {
		loop = "Loop + 1"
	}
	s := fmt.Sprintf(`[ ClusterId = %d; ProcId = %d; Owner = "user%d"; Site = "site%d"; QDate = %d; JobStatus = %d;
		RssBase = %d; BigBase = %d; MemUse = %s; Label = %s; Big = %s; Ratio = %s; Flag = Rss > 2000000;
		Age = time() - QDate; Loop = %s`,
		i, i%10, i%7, i%4, 1700000000+i, 1+i%5, i*3, 9007199254740993+int64(i), mem, label, big, ratio, loop)
	if rss != "" {
		s += "; Rss = " + rss
	}
	return s + " ]"
}

// computedQueries are the constraints the fixture is checked under.
var computedQueries = []string{
	"MemUse > 2047",
	"MemUse > 2047 && QDate > 1700001000",
	"MemUse <= 100 || ProcId == 3",
	"MemUse is undefined",
	"MemUse isnt undefined",
	"isUndefined(MemUse)",
	"(MemUse ?: -1) >= 0",
	"MemUse == 3",
	`Label == "USER1-site2"`,
	`Label =?= "SITE3"`,
	`Label == "lit2"`,
	"Big > 9007199254741000",
	"Big == 9007199254740995",
	"Big == 9007199254740996",
	"Ratio > 1000.5",
	"Ratio < 10",
	"Flag",
	"Flag == false",
	"Flag is undefined", // no record holds Flag as a literal, so every block's own cells call it absent
	"Flag isnt undefined",
	"Age > 0",
	"Loop > 3",
	"Loop is undefined",
	"Rss is undefined",
}

// buildComputedFixture writes n fixture records into a persistent collection (archive when archive is
// true), columnarizes, closes and reopens it so the columns are read back from disk.
func buildComputedFixture(t *testing.T, dir string, n, excEvery int, archive bool) *Collection {
	return buildComputedFixtureOpts(t, dir, n, excEvery, archive, false)
}

// buildComputedFixtureOpts is buildComputedFixture; interned archives key each sealed segment's records
// by its own dictionary.
func buildComputedFixtureOpts(t *testing.T, dir string, n, excEvery int, archive, interned bool) *Collection {
	t.Helper()
	reopened := false
	type store struct {
		c     *Collection
		put   func(i int, ad *classad.ClassAd) error
		close func() error
	}
	open := func() store {
		if archive {
			opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, GroupSchemaCount: -1, InternAtSeal: interned}
			a, err := CreateArchive(opts)
			if err != nil && reopened {
				a, err = OpenArchive(opts)
			}
			if err != nil {
				t.Fatal(err)
			}
			return store{a.c, func(_ int, ad *classad.ClassAd) error { return a.Append(ad) }, a.Close}
		}
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
		if err != nil {
			t.Fatal(err)
		}
		return store{c, func(i int, ad *classad.ClassAd) error { return c.Put([]byte(fmt.Sprintf("%d.0", i)), ad) }, c.Close}
	}
	st := open()
	for i := range n {
		if err := st.put(i, mustAd(t, computedAd(i, excEvery))); err != nil {
			t.Fatal(err)
		}
	}
	if !st.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	reopened = true
	st = open()
	c := st.c
	c.BuildAndEnableSchemaScan(4096, 8)
	t.Cleanup(func() { st.close() })
	return c
}

// computedRefAds decodes every record by a full scan, the reference every columnar answer is checked against.
func computedRefAds(t *testing.T, c *Collection) []*classad.ClassAd {
	t.Helper()
	var ads []*classad.ClassAd
	for ad := range c.Scan() {
		ads = append(ads, ad)
	}
	if len(ads) != c.Len() {
		t.Fatalf("scan returned %d of %d records", len(ads), c.Len())
	}
	return ads
}

func computedRefCount(t *testing.T, ads []*classad.ClassAd, constraint string) int {
	t.Helper()
	e, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ad := range ads {
		if b, err := e.Eval(ad).BoolValue(); err == nil && b {
			n++
		}
	}
	return n
}

// computedRefStats is NumStats computed by evaluating attr in every record.
func computedRefStats(ads []*classad.ClassAd, attr string) NumStats {
	st := NumStats{Min: math.Inf(1), Max: math.Inf(-1), IntMin: math.MaxInt64, IntMax: math.MinInt64}
	for _, ad := range ads {
		v := ad.EvaluateAttr(attr)
		var f float64
		switch {
		case v.IsInteger():
			i, _ := v.IntValue()
			f = float64(i)
			st.IntSum += i
			if i < st.IntMin {
				st.IntMin = i
			}
			if i > st.IntMax {
				st.IntMax = i
			}
		case v.IsReal():
			f, _ = v.RealValue()
			st.AnyReal = true
		default:
			continue
		}
		st.N++
		st.Sum += f
		st.Min, st.Max = math.Min(st.Min, f), math.Max(st.Max, f)
	}
	return st
}

func checkComputedFixture(t *testing.T, c *Collection, label string) {
	t.Helper()
	ads := computedRefAds(t, c)
	parse := func(e string) *vm.Query {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	for _, e := range computedQueries {
		want := computedRefCount(t, ads, e)
		q := parse(e)
		if got, ok := c.CountQuery(q); (!ok && q.Native()) || (ok && got != want) {
			t.Errorf("%s: CountQuery(%s) = %d (ok=%v), want %d", label, e, got, ok, want)
		}
		if got, ok := c.PresenceCountQuery(q); ok && got != want {
			t.Errorf("%s: PresenceCountQuery(%s) = %d, want %d", label, e, got, want)
		}
		if got, ok := c.VectorEvalCount(q); ok && got != want {
			t.Errorf("%s: VectorEvalCount(%s) = %d, want %d", label, e, got, want)
		}
		if got, ok := c.ColumnarEvalCount(q); ok && got != want {
			t.Errorf("%s: ColumnarEvalCount(%s) = %d, want %d", label, e, got, want)
		}
		if n := len(readAll(t, c, e)); n != want {
			t.Errorf("%s: query %s returned %d records, want %d", label, e, n, want)
		}
		if gs, ok := c.GroupStatsQuery(q, "JobStatus", nil); ok {
			sum := 0
			for _, g := range gs {
				sum += g.Count
			}
			if sum != want {
				t.Errorf("%s: GroupStatsQuery(%s) BY JobStatus sums to %d, want %d", label, e, sum, want)
			}
		}
	}
	all := parse("ProcId >= 0")
	for _, attr := range []string{"MemUse", "Big", "Ratio", "Loop"} {
		want := computedRefStats(ads, attr)
		st, ok := c.NumStatsQuery(all, attr)
		if !ok {
			continue
		}
		if st.N != want.N || st.AnyReal != want.AnyReal || math.Abs(st.Sum-want.Sum) > 1e-6*math.Max(1, math.Abs(want.Sum)) {
			t.Errorf("%s: NumStatsQuery(%s) = N %d sum %v real %v, want N %d sum %v real %v",
				label, attr, st.N, st.Sum, st.AnyReal, want.N, want.Sum, want.AnyReal)
		}
		if !want.AnyReal && want.N > 0 && (st.IntSum != want.IntSum || st.IntMin != want.IntMin || st.IntMax != want.IntMax) {
			t.Errorf("%s: NumStatsQuery(%s) ints sum %d [%d,%d], want sum %d [%d,%d]",
				label, attr, st.IntSum, st.IntMin, st.IntMax, want.IntSum, want.IntMin, want.IntMax)
		}
		if want.N > 0 && (st.Min != want.Min || st.Max != want.Max) {
			t.Errorf("%s: NumStatsQuery(%s) range [%v,%v], want [%v,%v]", label, attr, st.Min, st.Max, want.Min, want.Max)
		}
		if gs, ok := c.GroupStatsQuery(all, "JobStatus", []string{attr}); ok {
			n := 0
			for _, g := range gs {
				n += g.Stats[0].N
			}
			if n != want.N {
				t.Errorf("%s: GroupStatsQuery COUNT(%s) = %d, want %d", label, attr, n, want.N)
			}
		}
	}
}

// TestComputedCellsMatchPlainEvaluation is the differential check: every columnar reader agrees with
// evaluating each record in full, on a mutable table and an archive, with uncomputable cells at 0.1% and
// 5% of the records mixed into blocks of computed and literal ones -- and the readers serve the field
// from the columns rather than giving the query back.
func TestComputedCellsMatchPlainEvaluation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excEvery int
		archive  bool
		interned bool
	}{
		{"mutable/0.1%", 1000, false, false},
		{"mutable/5%", 20, false, false},
		{"archive/0.1%", 1000, true, false},
		{"archive/5%", 20, true, false},
		{"archive-interned/5%", 20, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := computedCellsBuilt.Load()
			c := buildComputedFixtureOpts(t, t.TempDir(), 3000, tc.excEvery, tc.archive, tc.interned)
			if tc.interned {
				dicts := 0
				for _, sh := range c.shards {
					for _, seg := range sh.segs {
						if seg.dict.Load() != nil && seg.columnarized() {
							dicts++
						}
					}
				}
				if dicts == 0 {
					t.Fatal("no columnarized segment is interned")
				}
			}
			if segs, _ := ColumnarizedSegments(); segs == 0 {
				t.Fatal("nothing was columnarized")
			}
			if computedCellsBuilt.Load() == built {
				t.Fatal("no cell was computed")
			}
			st := c.schemaScan.Load()
			for _, attr := range []string{"MemUse", "Label", "Big", "Ratio", "Flag"} {
				id, _ := c.intern.LookupID(attr)
				if _, ok := st.schema.byID[id]; !ok {
					t.Fatalf("%s is not a schema field: an expression-valued field the cells compute should be one", attr)
				}
			}
			checkComputedFixture(t, c, tc.name)

			// The fast paths: the aggregate answers from the columns, evaluating only the records it
			// cannot compute.
			evals := heldExceptionEvals.Load()
			q, _ := vm.Parse("ProcId >= 0")
			if _, ok := c.NumStatsQuery(q, "MemUse"); !ok {
				t.Error("NumStatsQuery(MemUse) gave the query back")
			}
			if heldExceptionEvals.Load() == evals {
				t.Error("no uncomputed cell was evaluated on its own")
			}
			fb := ColumnarFallbacks()
			mq, _ := vm.Parse("MemUse > 2047")
			c.ColumnarEvalCount(mq)
			if got, limit := ColumnarFallbacks()-fb, int64(c.Len()/tc.excEvery+c.Len()/50); got > limit {
				t.Errorf("ColumnarEvalCount(MemUse > 2047) fell back %d times, want at most %d", got, limit)
			}
		})
	}
}

// TestComputedCellsReassembleByteIdentically: computing values changes no byte any read returns.
//
// Within one store, the same reads with the computed values dropped from the live blocks return
// identical bytes. Across two stores built from the same records with and without computed values,
// every reassembled ad holds the same attributes with byte-identical nodes. (The ORDER of attributes in
// a reassembled ad follows the process's attribute numbering, which already differs between two builds
// of the same data, so the cross-store comparison is per attribute.)
func TestComputedCellsReassembleByteIdentically(t *testing.T) {
	q, _ := vm.Parse("true")
	collect := func(c *Collection, proj []string, perAttr bool) []string {
		var out []string
		for w := range c.QueryRawWire(q, proj, false) {
			if !perAttr {
				out = append(out, string(w))
				continue
			}
			hdr, _, _, _, ok := wire.Ad(w).SplitBody()
			if !ok {
				t.Fatal("a reassembled ad does not parse")
			}
			var ents []string
			wire.Ad(w).ForEachRaw(func(id uint32, name string, node []byte) bool {
				ents = append(ents, fmt.Sprintf("%d/%s=%x", id, name, node))
				return true
			})
			sort.Strings(ents)
			out = append(out, fmt.Sprintf("%x|%v", hdr, ents))
		}
		sort.Strings(out)
		return out
	}
	projections := [][]string{nil, {"MemUse"}, {"MemUse", "Label", "Owner"}, {"Big", "Ratio", "Flag", "Rss"}}
	same := func(label string, a, b []string) {
		t.Helper()
		if !slices.Equal(a, b) {
			diff := 0
			for i := 0; i < len(a) && i < len(b); i++ {
				if a[i] != b[i] {
					diff++
				}
			}
			t.Errorf("%s: %d records against %d, %d differ", label, len(a), len(b), diff)
		}
	}

	with := buildComputedFixture(t, t.TempDir(), 2000, 50, false)
	colComputeCells = false
	without := buildComputedFixture(t, t.TempDir(), 2000, 50, false)
	colComputeCells = true
	if !sameSchemaLayout(with.schemaScan.Load().schema, without.schemaScan.Load().schema) {
		t.Fatal("the two arms derived different schemas")
	}
	for _, proj := range projections {
		same(fmt.Sprintf("across stores, projection %v", proj), collect(with, proj, true), collect(without, proj, true))
	}

	before := make([][]string, len(projections))
	for i, proj := range projections {
		before[i] = collect(with, proj, false)
	}
	computed := 0
	for _, sh := range with.shards {
		for _, seg := range sh.segs {
			if cn := seg.colNative.Load(); cn != nil {
				for _, b := range cn.seg.blocks {
					computed += len(b.computed)
					b.computed, b.computedComp = nil, nil
				}
			}
		}
	}
	if computed == 0 {
		t.Fatal("the computed arm holds no computed values")
	}
	for i, proj := range projections {
		same(fmt.Sprintf("one store, projection %v", proj), before[i], collect(with, proj, false))
	}
}

// TestComputedCellPurity pins which cells are computed, and that a computed value is the one the record
// evaluates to in EVERY context -- alone, and matched against a target ad that defines what the record
// lacks. A value that changed with the context would be right for one reader and wrong for another.
func TestComputedCellPurity(t *testing.T) {
	c, err := Open(Options{Shards: 1, DataKey: bytes.Repeat([]byte{7}, 32), EncryptedAttrs: []string{"Secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Plain is written in the clear and only then made sealable, as a record written before the
	// attribute joined EncryptedAttrs holds it.
	base := `Rss = 4096; RssExpr = Rss * 2; Plain = 11; Secret = 5; Memory = 64; ClusterId = 1; CurrentTime = 100; Clock = CurrentTime; `
	target := mustAd(t, `[ Memory = 9999; Missing = 3; Rss = 1 ]`)
	// Another record of the table holds Missing, so the name is known; this one lacks it.
	c.intern.Intern("Missing")
	for _, tc := range []struct {
		expr string
		want string // the computed value, or "" when the cell must not be computed
	}{
		{"((Rss + 1023) / 1024)", "4"},
		{"(RssExpr + 1023) / 1024", "8"},
		{"Rss / 3.0", "1365.3333333333333"},
		{"MY.Rss + 1", "4097"},
		{`strcat("a", Rss)`, `"a4096"`},
		{"ifThenElse(Rss > 10, Rss, 0)", "4096"},
		{"Rss > 10 && Memory > 1", "true"},
		{"Rss ?: 7", "4096"},
		{"TARGET.Memory / 2", ""},
		{"ifThenElse(isUndefined(TARGET.Memory), 1, 2)", ""},
		{"isUndefined(Missing)", ""},
		{"Missing + 1", ""},
		{"PARENT.Rss", ""},
		{".Rss + 1", ""},
		{"time() - 5", ""},
		{"CurrentTime - 5", ""},
		// Never computed, even where the record defines CurrentTime: a reader substitutes the clock
		// for a CurrentTime the record lacks, so the name is left to the row path outright.
		{"Clock - 5", ""},
		{"random(10)", ""},
		{`eval("Rss")`, ""},
		{"Secret + 1", ""},
		{"Plain + 1", ""},
		{"Self + 1", ""}, // a cycle: ERROR
		{"1 / 0", ""},
		{"undefined", ""},
		{"{1, 2}[0]", ""},
		{"[a = 1].a", ""},
	} {
		src := "[ " + base + "Self = " + tc.expr + "; Field = " + tc.expr + " ]"
		ad := mustAd(t, src)
		// The record as the builder sees it: global-interned wire, sealed values sealed.
		iw := wire.EncodeWithHotEnc(nil, ad.AST(), c.intern, nil, c.shouldEncrypt, c.sealer)
		if err := c.SetEncryptedAttrs([]string{"Secret", "Plain"}); err != nil {
			t.Fatal(err)
		}
		id := c.intern.Intern("Field")
		s := layoutSchema([]adField{{id: id, kind: akInt, width: 8}})
		cells := newCellComputer(c).cells(iw, s)
		if err := c.SetEncryptedAttrs([]string{"Secret"}); err != nil {
			t.Fatal(err)
		}
		got := ""
		if len(cells) == 1 {
			lit, _ := wire.LiteralValue(cells[0].node)
			got = litToValue(lit).String()
		}
		if got != tc.want {
			t.Errorf("Field = %s: computed %q, want %q", tc.expr, got, tc.want)
			continue
		}
		if got == "" {
			continue
		}
		solo := mustAd(t, src).EvaluateAttr("Field")
		matched := mustAd(t, src)
		matched.SetTarget(target)
		if alone, against := solo.String(), matched.EvaluateAttr("Field").String(); alone != got || against != got {
			t.Errorf("Field = %s: computed %s, but evaluates to %s alone and %s against a target", tc.expr, got, alone, against)
		}
	}
}

// TestComputedTrailerIsOptional: the computed values are an accelerator. A payload whose computed trailer
// is damaged parses to the same segment without them, and the records still answer every query exactly.
func TestComputedTrailerIsOptional(t *testing.T) {
	c := buildComputedFixture(t, t.TempDir(), 2000, 50, false)
	checked := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil || !cn.seg.hasComputed() {
				continue
			}
			body, _, err := splitColPayload(recAd(seg.data, cn.rec))
			if err != nil {
				t.Fatal(err)
			}
			trailer := appendComputedTrailer(nil, cn.seg.blocks)
			if !bytes.HasSuffix(body, trailer) {
				t.Fatal("the computed trailer is not the payload's last bytes")
			}
			torn := slices.Clone(body[:len(body)-1])
			cs := unmarshalColSegment(torn, c.regionCodec(), c.intern.Intern)
			if cs == nil || cs.hasComputed() || cs.remnants != nil || len(cs.blocks) != len(cn.seg.blocks) {
				t.Fatal("a torn computed trailer did not parse to the segment without it")
			}
			// Drop the computed values from the live segment: the answers must not move.
			for _, b := range cn.seg.blocks {
				b.computed, b.computedComp = nil, nil
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no segment carries computed values")
	}
	checkComputedFixture(t, c, "without computed values")
}
