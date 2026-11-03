package collections

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// produceAttrs is deliberately a mix of what storage does different things with: a unique integer so
// a row can be told from its neighbour, a low-cardinality string that becomes a dictionary, a real,
// a boolean, an attribute only some records have, one whose TYPE changes from record to record, and
// one that is an expression rather than a literal.
var produceAttrs = []string{"Idx", "Owner", "Ratio", "Flag", "Note", "Score", "Derived"}

func produceAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Idx = %d; ", i)
	fmt.Fprintf(&b, "Owner = \"user%d\"; ", i%5)
	fmt.Fprintf(&b, "Ratio = %d.%d; ", i%13, i%10)
	fmt.Fprintf(&b, "Flag = %v; ", i%3 == 0)
	fmt.Fprintf(&b, "Rss = %d; ", i*37%100000)
	// Present on most records, absent on the rest: UNDEFINED has to be distinguishable from a
	// value, and an attribute no record has is a different case again (see the absent test).
	if i%7 != 0 {
		fmt.Fprintf(&b, "Note = \"note-%d\"; ", i%11)
	}
	// The same attribute as an integer on some records and a string on others. A column fits one
	// of them and the other escapes, which is the shape a producer must not flatten.
	if i%4 == 0 {
		fmt.Fprintf(&b, "Score = \"high\"; ")
	} else {
		fmt.Fprintf(&b, "Score = %d; ", i%50)
	}
	// Computed: its value lives in the column now, but the stored node is still the expression.
	b.WriteString("Derived = (Rss + 1023) / 1024")
	return b.String()
}

type produceFixture struct {
	c   *Collection
	src map[int]*classad.ClassAd
}

func buildProduceFixture(t *testing.T, n int) *produceFixture {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &produceFixture{c: c, src: make(map[int]*classad.ClassAd, n)}
	for i := range n {
		ad, err := classad.ParseOld(produceAd(i))
		if err != nil {
			t.Fatal(err)
		}
		f.src[i] = ad
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// cellText renders one produced cell. Rows are compared as sorted text because ScanColumns hands
// back values and not keys: there is nothing to join a row to its ad by, so the comparison is of
// multisets.
func cellText(col *Column, i int) string {
	switch col.Kind[i] {
	case ValueInt:
		return fmt.Sprintf("int:%d", col.Int(i))
	case ValueReal:
		return fmt.Sprintf("real:%v", col.Float(i))
	case ValueBool:
		return fmt.Sprintf("bool:%v", col.Bool(i))
	case ValueString:
		return fmt.Sprintf("str:%s", col.Text(i))
	case ValueUndefined:
		return "undefined"
	}
	return "error"
}

// wantText renders what EVALUATING the ad says the same cell should be. Evaluation is the reference
// for every tier, including the columnar ones: a computed cell's value is in the column, so a
// producer that returned the expression instead would be returning something no reader asked for.
func wantText(ad *classad.ClassAd, attr string) string {
	v := ad.EvaluateAttr(attr)
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsBool():
		b, _ := v.BoolValue()
		return fmt.Sprintf("bool:%v", b)
	case v.IsInteger():
		i, _ := v.IntValue()
		return fmt.Sprintf("int:%d", i)
	case v.IsReal():
		r, _ := v.RealValue()
		return fmt.Sprintf("real:%v", r)
	case v.IsString():
		s, _ := v.StringValue()
		return fmt.Sprintf("str:%s", s)
	}
	return "error"
}

// collectProduced runs a scan and renders every row.
func collectProduced(t *testing.T, c *Collection, q *vm.Query, attrs []string) ([]string, ColumnScanStats) {
	t.Helper()
	var rows []string
	st, ok := c.ScanColumns(q, attrs, func(b *Batch) bool {
		if len(b.Cols) != len(attrs) {
			t.Errorf("batch has %d columns, asked for %d", len(b.Cols), len(attrs))
			return false
		}
		for i := range b.Cols {
			if b.Cols[i].Name != attrs[i] {
				t.Errorf("column %d is %q, asked for %q", i, b.Cols[i].Name, attrs[i])
				return false
			}
			if len(b.Cols[i].Kind) != b.N {
				t.Errorf("column %q has %d cells, batch says %d rows", attrs[i], len(b.Cols[i].Kind), b.N)
				return false
			}
		}
		for r := range b.N {
			parts := make([]string, len(b.Cols))
			for i := range b.Cols {
				parts[i] = cellText(&b.Cols[i], r)
			}
			rows = append(rows, strings.Join(parts, "|"))
		}
		return true
	})
	if !ok {
		t.Fatal("ScanColumns declined")
	}
	return rows, st
}

// wantRows renders what evaluation says the scan should have produced, for the ads a predicate keeps.
func wantRows(src map[int]*classad.ClassAd, attrs []string, keep func(*classad.ClassAd) bool) []string {
	var rows []string
	for _, ad := range src {
		if !keep(ad) {
			continue
		}
		parts := make([]string, len(attrs))
		for i, a := range attrs {
			parts[i] = wantText(ad, a)
		}
		rows = append(rows, strings.Join(parts, "|"))
	}
	return rows
}

func TestScanColumnsMatchesEvaluation(t *testing.T) {
	f := buildProduceFixture(t, 3000)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	for _, tc := range []struct {
		name  string
		where string
		keep  func(*classad.ClassAd) bool
	}{
		{"all", "Idx >= 0", func(*classad.ClassAd) bool { return true }},
		{"numeric", "Idx < 1000", func(ad *classad.ClassAd) bool {
			i, _ := ad.EvaluateAttr("Idx").IntValue()
			return i < 1000
		}},
		{"string equality", `Owner == "user3"`, func(ad *classad.ClassAd) bool {
			s, _ := ad.EvaluateAttr("Owner").StringValue()
			return s == "user3"
		}},
		{"boolean", "Flag", func(ad *classad.ClassAd) bool {
			b, _ := ad.EvaluateAttr("Flag").BoolValue()
			return b
		}},
		{"disjunction", `Idx < 50 || Owner == "user1"`, func(ad *classad.ClassAd) bool {
			i, _ := ad.EvaluateAttr("Idx").IntValue()
			s, _ := ad.EvaluateAttr("Owner").StringValue()
			return i < 50 || s == "user1"
		}},
		{"over the computed column", "Derived > 50", func(ad *classad.ClassAd) bool {
			v, ok := ad.EvaluateAttrNumber("Derived")
			return ok && v > 50
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := vm.Parse(tc.where)
			if err != nil {
				t.Fatal(err)
			}
			got, st := collectProduced(t, f.c, q, produceAttrs)
			want := wantRows(f.src, produceAttrs, tc.keep)
			if len(want) == 0 {
				t.Fatal("the predicate keeps nothing, so this case proves nothing")
			}
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("produced %d rows, evaluation says %d%s", len(got), len(want), firstDiff(got, want))
			}
			if st.Rows != len(got) {
				t.Errorf("stats say %d rows, callback saw %d", st.Rows, len(got))
			}
		})
	}
}

func firstDiff(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("; first difference at %d:\n got  %s\n want %s", i, got[i], want[i])
		}
	}
	return ""
}

// TestScanColumnsReadsTheColumns is the vacuity guard. Every assertion above passes just as well if
// the producer read every value from the records, which is the path it exists to replace: the rows
// would be identical and only the speed would say so. See ColumnScanStats.
func TestScanColumnsReadsTheColumns(t *testing.T) {
	f := buildProduceFixture(t, 3000)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	// The attributes the derived schema carries. Note and Score are deliberately NOT among them --
	// they live in the cold tail -- and they get their own case below.
	hot := []string{"Idx", "Owner", "Ratio", "Flag", "Derived"}
	q, _ := vm.Parse("Idx >= 0")
	_, st := collectProduced(t, f.c, q, hot)
	// Exact arithmetic rather than a ratio. Every cell is accounted for, and every row is either
	// wholly columnar or was materialized from the wire -- which is the claim a ratio would let
	// slip, since a columnar block that quietly fell back still leaves most of the scan columnar.
	//
	// The record reads here are the ACTIVE segment, which has no columnar block and never will
	// until it seals. That tier is expected; a sealed block reaching it is not.
	if st.ColumnCells+st.RecordCells != st.Rows*len(hot) {
		t.Errorf("%d + %d cells over %d rows of %d columns: %+v",
			st.ColumnCells, st.RecordCells, st.Rows, len(hot), st)
	}
	if st.RecordCells != st.RecordReads*len(hot) {
		t.Errorf("%d record cells over %d record reads of %d columns: a row mixed its sources "+
			"when nothing asked it to: %+v", st.RecordCells, st.RecordReads, len(hot), st)
	}
	if st.ColumnCells != (st.Rows-st.RecordReads)*len(hot) {
		t.Errorf("%d column cells, but %d of %d rows were not materialized: a sealed block fell "+
			"back: %+v", st.ColumnCells, st.Rows-st.RecordReads, st.Rows, st)
	}
	if st.ColumnCells == 0 {
		t.Fatalf("no cell came from the columns: %+v", st)
	}
	if st.ColumnCells < st.RecordCells {
		t.Errorf("more cells came from records than from columns: %+v", st)
	}
	if st.VectorBlocks == 0 {
		t.Errorf("no block vectorized: %+v", st)
	}
	if st.Unreadable != 0 {
		t.Errorf("%d records matched and could not then be read: %+v", st.Unreadable, st)
	}

	// One cold attribute beside the hot ones must not give up the hot ones, and must cost one
	// record read per row rather than one per cold attribute.
	_, mixed := collectProduced(t, f.c, q, append(slices.Clone(hot), "Note", "Score"))
	if mixed.ColumnCells == 0 {
		t.Errorf("a projection with one cold attribute read no columns at all: %+v", mixed)
	}
	if mixed.RecordCells == 0 {
		t.Fatalf("the cold attributes read no records: %+v", mixed)
	}
	if mixed.RecordReads > mixed.RecordCells {
		t.Errorf("%d record reads for %d record cells: a row was materialized more than once: %+v",
			mixed.RecordReads, mixed.RecordCells, mixed)
	}
	if want := mixed.ColumnCells + mixed.RecordCells; want > mixed.Rows*(len(hot)+2) {
		t.Errorf("counted %d cells over %d rows of %d columns: %+v", want, mixed.Rows, len(hot)+2, mixed)
	}
}

// TestScanColumnsServesTheRowTier covers a collection with no columnar block at all: records are in
// the active segment, so every value comes from the record's own wire form. A tier that contributed
// nothing would be a result silently missing records.
func TestScanColumnsServesTheRowTier(t *testing.T) {
	f := buildProduceFixture(t, 300)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	q, _ := vm.Parse("Idx >= 0")
	got, st := collectProduced(t, f.c, q, produceAttrs)
	want := wantRows(f.src, produceAttrs, func(*classad.ClassAd) bool { return true })
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("row tier produced %d rows, evaluation says %d%s", len(got), len(want), firstDiff(got, want))
	}
	if st.RowWindows == 0 {
		t.Fatalf("this fixture was supposed to have no columnar block: %+v", st)
	}
	if st.ColumnCells != 0 {
		t.Errorf("the row tier read %d cells from columns that do not exist: %+v", st.ColumnCells, st)
	}
}

// TestScanColumnsAbsentAttributeIsUndefined: an attribute NO record has is undefined everywhere, and
// is not an error and not a missing column. It is also the one case the producer answers without
// consulting storage, so it is worth checking it answers at all.
func TestScanColumnsAbsentAttributeIsUndefined(t *testing.T) {
	f := buildProduceFixture(t, 2000)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	q, _ := vm.Parse("Idx < 100")
	attrs := []string{"Idx", "NoSuchAttributeAnywhere"}
	rows, st := collectProduced(t, f.c, q, attrs)
	if len(rows) != 100 {
		t.Fatalf("got %d rows, want 100", len(rows))
	}
	for _, r := range rows {
		if !strings.HasSuffix(r, "|undefined") {
			t.Fatalf("row %q does not report the absent attribute as undefined", r)
		}
	}
	// And asking for it must not have dragged every block onto the record path.
	if st.RecordReads != 0 {
		t.Errorf("an attribute nothing has materialized %d records: %+v", st.RecordReads, st)
	}
}

// TestScanColumnsStopsWhenAsked: a consumer that has what it needs stops the scan, and that is a
// stop rather than a failure.
func TestScanColumnsStopsWhenAsked(t *testing.T) {
	f := buildProduceFixture(t, 3000)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	q, _ := vm.Parse("Idx >= 0")
	batches := 0
	st, ok := f.c.ScanColumns(q, produceAttrs, func(b *Batch) bool {
		batches++
		return false
	})
	if !ok {
		t.Fatal("stopping early reported a decline")
	}
	if batches != 1 {
		t.Errorf("callback ran %d times after asking to stop, want 1", batches)
	}
	if st.Rows == 0 || st.Rows == 3000 {
		t.Errorf("stopped scan counted %d rows; want the first batch only", st.Rows)
	}
}

// TestScanColumnsDeclinesBeforeProducing: ok=false has to mean nothing was produced, or a caller
// falling back to its own scan would emit those rows twice.
func TestScanColumnsDeclinesBeforeProducing(t *testing.T) {
	f := buildProduceFixture(t, 100)
	q, _ := vm.Parse("Idx >= 0")
	called := false
	fn := func(*Batch) bool { called = true; return true }
	// No accelerator yet.
	if _, ok := f.c.ScanColumns(q, produceAttrs, fn); ok {
		t.Error("served with no schema scan enabled")
	}
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	if _, ok := f.c.ScanColumns(nil, produceAttrs, fn); ok {
		t.Error("served a nil query")
	}
	if _, ok := f.c.ScanColumns(q, nil, fn); ok {
		t.Error("served an empty attribute list")
	}
	// A query whose attributes are only known once it runs: there is no plan to make from it.
	if nq, err := vm.Parse(`eval(strcat("Ow", "ner")) == "user3"`); err == nil {
		if _, ok := f.c.ScanColumns(nq, produceAttrs, fn); ok {
			t.Error("served a non-native query")
		}
	}
	if called {
		t.Error("a declined scan produced a batch")
	}
}

// TestScanColumnsTypesSurviveTheRoundTrip checks the one thing the text comparison above cannot:
// that a real arrives as a real and an integer as an integer, rather than both arriving as whichever
// one formats the same. Num holds a real as its bits, and a producer that forgot would hand a
// consumer a very large integer.
func TestScanColumnsTypesSurviveTheRoundTrip(t *testing.T) {
	f := buildProduceFixture(t, 2000)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	q, _ := vm.Parse("Idx < 200")
	kinds := map[string]map[ValueKind]int{}
	_, ok := f.c.ScanColumns(q, produceAttrs, func(b *Batch) bool {
		for i := range b.Cols {
			col := &b.Cols[i]
			m := kinds[col.Name]
			if m == nil {
				m = map[ValueKind]int{}
				kinds[col.Name] = m
			}
			for r := range b.N {
				m[col.Kind[r]]++
				if col.Kind[r] == ValueReal {
					v := col.Float(r)
					if math.IsNaN(v) || v < 0 || v > 100 {
						t.Fatalf("%s real cell reads %v, which is not a value this fixture holds", col.Name, v)
					}
				}
			}
		}
		return true
	})
	if !ok {
		t.Fatal("ScanColumns declined")
	}
	if kinds["Ratio"][ValueReal] == 0 {
		t.Errorf("Ratio produced no reals: %v", kinds["Ratio"])
	}
	if kinds["Idx"][ValueInt] == 0 {
		t.Errorf("Idx produced no integers: %v", kinds["Idx"])
	}
	if kinds["Flag"][ValueBool] == 0 {
		t.Errorf("Flag produced no booleans: %v", kinds["Flag"])
	}
	if kinds["Owner"][ValueString] == 0 {
		t.Errorf("Owner produced no strings: %v", kinds["Owner"])
	}
	if kinds["Note"][ValueUndefined] == 0 {
		t.Errorf("Note is absent on some records but produced no undefined: %v", kinds["Note"])
	}
	// Score is an integer on three records in four and a string on the fourth. Both must survive:
	// a column that fits one of them and escapes the other is exactly what a producer flattens.
	if kinds["Score"][ValueInt] == 0 || kinds["Score"][ValueString] == 0 {
		t.Errorf("Score did not produce both integers and strings: %v", kinds["Score"])
	}
}

// TestScanColumnsRedactedDoesNotOpenSealedValues: the producer hands back VALUES, so a reader not
// entitled to a sealed one must not get it from any tier -- not from the record, and not from a
// column, which on a store that may hold sealed attributes in the clear holds the clear value too.
//
// The entitled arm is the PREMISE and is not decoration: an assertion that a redacted scan contains
// no secrets passes just as well against a fixture that has none, or a scan that produced no rows.
func TestScanColumnsRedactedDoesNotOpenSealedValues(t *testing.T) {
	var lay sealedLayout
	for _, l := range sealedLayouts {
		if l.name == "columnar" {
			lay = l
		}
	}
	if lay.build == nil {
		t.Fatal("no columnar sealed layout to test against")
	}
	c := openSealedLayout(t, lay)
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		t.Fatal(err)
	}
	attrs := []string{"ProcId", "Owner", "Token", "SealedNum"}

	secrets := map[string]bool{}
	for i := range sealedParityN {
		secrets[sealedParityOwner(i, false)] = true
		if tok, ok := sealedParityToken(i); ok {
			secrets[tok] = true
		}
	}

	// PREMISE: the entitled reader sees the sealed values, so there is something to withhold.
	openSeen := map[string]int{}
	openRows := 0
	if _, ok := c.ScanColumns(q, attrs, func(b *Batch) bool {
		openRows += b.N
		for i := range b.Cols {
			col := &b.Cols[i]
			for r := range b.N {
				if col.Kind[r] == ValueString && secrets[col.Text(r)] {
					openSeen[col.Name]++
				}
			}
		}
		return true
	}); !ok {
		t.Fatal("the entitled scan declined, so the premise is untested")
	}
	if openRows == 0 {
		t.Fatal("the entitled scan produced no rows")
	}
	if openSeen["Owner"] == 0 || openSeen["Token"] == 0 {
		t.Fatalf("the entitled reader saw no sealed values (%v), so withholding them proves nothing", openSeen)
	}

	// The keyless reader: same query, same attributes, no sealed value anywhere in the output.
	redRows := 0
	kinds := map[string]map[ValueKind]int{}
	if _, ok := c.ScanColumnsRedacted(q, attrs, func(b *Batch) bool {
		redRows += b.N
		for i := range b.Cols {
			col := &b.Cols[i]
			m := kinds[col.Name]
			if m == nil {
				m = map[ValueKind]int{}
				kinds[col.Name] = m
			}
			for r := range b.N {
				m[col.Kind[r]]++
				if col.Kind[r] == ValueString && secrets[col.Text(r)] {
					t.Fatalf("the keyless scan produced the sealed value %q for %s", col.Text(r), col.Name)
				}
			}
		}
		return true
	}); !ok {
		t.Fatal("the keyless scan declined")
	}
	if redRows != openRows {
		t.Errorf("the keyless scan produced %d rows, the entitled one %d: withholding a value must "+
			"not drop the record", redRows, openRows)
	}
	// ProcId is not sealed and must come through unchanged, or the test above would pass by
	// producing nothing of substance.
	if kinds["ProcId"][ValueInt] != redRows {
		t.Errorf("ProcId is not sealed but produced %v over %d rows", kinds["ProcId"], redRows)
	}
	for _, a := range []string{"Owner", "Token", "SealedNum"} {
		if n := kinds[a][ValueInt] + kinds[a][ValueString] + kinds[a][ValueBool] + kinds[a][ValueReal]; n != 0 {
			t.Errorf("%s is sealed but the keyless scan produced %d values for it: %v", a, n, kinds[a])
		}
	}
	t.Logf("keyless kinds: %v", kinds)
}

// TestScanColumnsTiersAgree: a tier decides which records match; it must never decide what their
// values are. Each case here lands on a DIFFERENT tier and is checked against the same reference --
// evaluating the source ads, which knows nothing about tiers -- and asserts the tier it meant to
// exercise actually ran, because every one of these passes trivially if the query quietly took the
// vector path instead.
func TestScanColumnsTiersAgree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where string
		keep  func(*classad.ClassAd) bool
		// tier names the counter that must be non-zero for this case to have tested anything.
		tier func(ColumnScanStats) int
	}{
		// Score is in the cold tail, so the executor has no column to compare and gives the block
		// back to per-record evaluation -- while the VALUES still come from the columns that do
		// exist, which is the asymmetry this test is about.
		{"the executor declines the block", `Score == 7`, func(ad *classad.ClassAd) bool {
			i, err := ad.EvaluateAttr("Score").IntValue()
			return err == nil && i == 7
		}, func(st ColumnScanStats) int { return st.ScopeBlocks }},
		{"the block vectorizes", "Idx < 1500", func(ad *classad.ClassAd) bool {
			i, _ := ad.EvaluateAttr("Idx").IntValue()
			return i < 1500
		}, func(st ColumnScanStats) int { return st.VectorBlocks }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildProduceFixture(t, 3000)
			if !f.c.BuildAndEnableSchemaScan(4096, 8) {
				t.Fatal("no schema derived")
			}
			q, err := vm.Parse(tc.where)
			if err != nil {
				t.Fatal(err)
			}
			got, st := collectProduced(t, f.c, q, produceAttrs)
			if tc.tier(st) == 0 {
				t.Fatalf("this case was supposed to exercise its tier and did not: %+v", st)
			}
			want := wantRows(f.src, produceAttrs, tc.keep)
			if len(want) == 0 {
				t.Fatal("the predicate keeps nothing, so this case proves nothing")
			}
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("tier produced %d rows, evaluation says %d%s", len(got), len(want), firstDiff(got, want))
			}
		})
	}
}

// TestScanColumnsSupersededRecordsAgree: a sealed block holds the records that WERE there, and a
// rewritten one is still in it. Which of them a reader may see is the snapshot's business, and the
// producer has to honour it in both tiers that read a columnar block.
//
// Two densities, because they take DIFFERENT tiers and the mistakes are different. Rewrite almost
// everything and the block goes to per-record evaluation, where visibility is checked per record.
// Rewrite a tenth and the block still vectorizes, where the predicate is evaluated for every record
// including the ones nobody can see and only the live mask keeps them out of the result. A fixture
// with only the heavy case leaves the vector tier's mask untested, and dropping it then produces
// superseded records with no test failing.
func TestScanColumnsSupersededRecordsAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		// keepEvery leaves one record in keepEvery at its first generation; the rest are rewritten.
		keepEvery int
		tier      func(ColumnScanStats) int
	}{
		{"mostly superseded, evaluated per record", 50, func(st ColumnScanStats) int { return st.ChurnBlocks }},
		{"lightly superseded, still vectorized", 10, func(st ColumnScanStats) int { return st.VectorBlocks }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			const n = 3000
			src := make(map[int]*classad.ClassAd, n)
			put := func(i, gen int) {
				ad, err := classad.ParseOld(produceAd(gen))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
					t.Fatal(err)
				}
				src[i] = ad
			}
			for i := range n {
				put(i, i)
			}
			if !c.BuildAndEnableSchemaScan(4096, 8) {
				t.Fatal("no schema derived")
			}
			for i := range n {
				if tc.name[0] == 'm' && i%tc.keepEvery == 0 {
					continue // left at its first generation, so the old blocks are not wholly dead
				}
				if tc.name[0] == 'l' && i%tc.keepEvery != 0 {
					continue // only one in keepEvery is rewritten
				}
				put(i, i+n)
			}
			q, _ := vm.Parse("Idx >= 0")
			got, st := collectProduced(t, c, q, produceAttrs)
			if tc.tier(st) == 0 {
				t.Fatalf("this case was supposed to exercise its tier and did not: %+v", st)
			}
			want := wantRows(src, produceAttrs, func(*classad.ClassAd) bool { return true })
			if len(got) != len(want) {
				t.Errorf("produced %d rows over %d live records: a superseded record was produced "+
					"or a live one dropped: %+v", len(got), len(want), st)
			}
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("produced %d rows, evaluation says %d%s", len(got), len(want), firstDiff(got, want))
			}
		})
	}
}

// TestScanColumnsRedactedOnAStoreSealedLater covers the withholding rule that fires BEFORE any
// storage is read, which the fixture above cannot reach.
//
// A store that seals values may have had the policy turned on AFTER records were written, so it
// holds a sealed attribute IN THE CLEAR for everything written before. Its
// COLUMNS hold the clear value too, and reading the column hands the keyless reader exactly what it
// is not entitled to -- which no care at the record tier would catch, because the record is never
// read. hidesClearSealed is the condition, and the withholding is unconditional under it.
//
// The fixture has to write the records BEFORE sealing Owner. Writing them after seals every one of
// them, there is then no clear value anywhere, and the test passes against a producer with no such
// rule at all. That version of this test was written first and caught nothing.
func TestScanColumnsRedactedOnAStoreSealedLater(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	// A key, but nothing named as sealed yet: Owner is written, columnarized and indexed in the
	// clear.
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 16, GroupSchemaCount: -1,
		DataKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	const n = 2000
	for i := range n {
		ad, err := classad.ParseOld(produceAd(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	c := a.c
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	c.ColumnarizeSealed()

	q, _ := vm.Parse("Idx >= 0")
	attrs := []string{"Idx", "Owner"}

	// PREMISE, in two halves. The clear owners must be reachable from the COLUMNS before the
	// policy changes -- that is the thing the rule has to withhold afterwards, and if the scan
	// were reading them from records instead, withholding them would be the record tier's job.
	before := 0
	beforeStats, ok := c.ScanColumns(q, attrs, func(b *Batch) bool {
		for r := range b.N {
			if b.Cols[1].Kind[r] == ValueString && strings.HasPrefix(b.Cols[1].Text(r), "user") {
				before++
			}
		}
		return true
	})
	if !ok {
		t.Fatal("the scan declined before the policy changed")
	}
	if before == 0 {
		t.Fatal("no clear owner was readable before the policy changed, so there is nothing to withhold")
	}
	if beforeStats.ColumnCells == 0 {
		t.Fatalf("the clear owners were not reachable from the COLUMNS, so withholding them is the "+
			"record tier's job and this test does not reach the rule: %+v", beforeStats)
	}

	if err := a.SetEncryptedAttrs([]string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	if !c.hidesClearSealed() || !c.keylessHides("Owner") {
		t.Fatal("after sealing Owner the store does not withhold clear sealed values, so the rule " +
			"under test is not in play")
	}

	// The entitled reader still sees them.
	after := 0
	if _, ok := c.ScanColumns(q, attrs, func(b *Batch) bool {
		for r := range b.N {
			if b.Cols[1].Kind[r] == ValueString && strings.HasPrefix(b.Cols[1].Text(r), "user") {
				after++
			}
		}
		return true
	}); !ok {
		t.Fatal("the entitled scan declined")
	}
	if after == 0 {
		t.Fatal("the entitled reader lost the owners it could see a moment ago")
	}

	// The keyless one sees none of them, from any tier.
	rows := 0
	if _, ok := c.ScanColumnsRedacted(q, attrs, func(b *Batch) bool {
		rows += b.N
		for r := range b.N {
			if k := b.Cols[1].Kind[r]; k != ValueUndefined {
				t.Fatalf("the keyless scan produced %s for an attribute the policy seals",
					cellText(&b.Cols[1], r))
			}
			if b.Cols[0].Kind[r] != ValueInt {
				t.Fatalf("Idx is not sealed but came back as %s", cellText(&b.Cols[0], r))
			}
		}
		return true
	}); !ok {
		t.Fatal("the keyless scan declined")
	}
	if rows != n {
		t.Errorf("the keyless scan produced %d rows, want %d: withholding a value must not drop the record", rows, n)
	}
}
