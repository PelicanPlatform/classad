package db

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// columnTable builds a table whose derived schema carries the attributes the tests read, plus
// one that it does not, so the mixed projection has something cold to find.
func columnTable(t *testing.T, n int) (*DB, map[string]*classad.ClassAd) {
	t.Helper()
	// Small segments so the fixture actually SEALS some: the accelerator covers sealed
	// segments only, and at the default size a few thousand small ads never fill one --
	// every window then takes the record tier and the column assertions below pass
	// against a scan that read no columns at all.
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 13})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	src := make(map[string]*classad.ClassAd, n)
	tx := d.Begin()
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("Idx", int64(i))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%5))
		ad.InsertAttrFloat("Ratio", float64(i%13)+float64(i%10)/10)
		ad.InsertAttrBool("Flag", i%3 == 0)
		key := fmt.Sprintf("k%d", i)
		src[key] = ad
		tx.NewClassAd(key, ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	// Enabling the accelerator derives the schema; it does not build the blocks. Without this
	// every window takes the record tier, the relay still answers correctly, and the tests
	// below that are about reading COLUMNS test nothing.
	d.c.ColumnarizeSealed()
	return d, src
}

// renderBatches drains a column sequence into one sorted line per row, so two reads of the same
// records can be compared whatever order each walked them in.
func renderBatches(seq func(func(*collections.Batch) bool), attrs []string) []string {
	var rows []string
	for b := range seq {
		for r := range b.N {
			parts := make([]string, len(b.Cols))
			for i := range b.Cols {
				col := &b.Cols[i]
				switch col.Kind[r] {
				case collections.ValueInt:
					parts[i] = fmt.Sprintf("int:%d", col.Int(r))
				case collections.ValueReal:
					parts[i] = fmt.Sprintf("real:%v", col.Float(r))
				case collections.ValueBool:
					parts[i] = fmt.Sprintf("bool:%v", col.Bool(r))
				case collections.ValueString:
					parts[i] = fmt.Sprintf("str:%s", col.Text(r))
				case collections.ValueUndefined:
					parts[i] = "undefined"
				default:
					parts[i] = "error"
				}
			}
			rows = append(rows, strings.Join(parts, "|"))
		}
	}
	sort.Strings(rows)
	return rows
}

// TestQueryColumnsMatchesEvaluation: the relay has to answer what evaluating the source ads
// answers, for the records the constraint keeps. Everything below asserts a property OF that
// answer, so this one comes first.
func TestQueryColumnsMatchesEvaluation(t *testing.T) {
	d, src := columnTable(t, 6000)
	attrs := []string{"Idx", "Owner", "Ratio", "Flag"}
	for _, tc := range []struct {
		name, where string
		keep        func(*classad.ClassAd) bool
	}{
		{"every record", "", func(*classad.ClassAd) bool { return true }},
		{"literally true", "true", func(*classad.ClassAd) bool { return true }},
		{"numeric", "Idx < 900", func(ad *classad.ClassAd) bool {
			i, _ := ad.EvaluateAttr("Idx").IntValue()
			return i < 900
		}},
		{"string", `Owner == "user2"`, func(ad *classad.ClassAd) bool {
			s, _ := ad.EvaluateAttr("Owner").StringValue()
			return s == "user2"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq, err := d.QueryColumns(tc.where, attrs, false)
			if err != nil {
				t.Fatalf("QueryColumns: %v", err)
			}
			got := renderBatches(seq, attrs)

			var want []string
			for _, ad := range src {
				if !tc.keep(ad) {
					continue
				}
				parts := make([]string, len(attrs))
				for i, a := range attrs {
					v := ad.EvaluateAttr(a)
					switch {
					case v.IsBool():
						b, _ := v.BoolValue()
						parts[i] = fmt.Sprintf("bool:%v", b)
					case v.IsInteger():
						n, _ := v.IntValue()
						parts[i] = fmt.Sprintf("int:%d", n)
					case v.IsReal():
						f, _ := v.RealValue()
						parts[i] = fmt.Sprintf("real:%v", f)
					case v.IsString():
						s, _ := v.StringValue()
						parts[i] = fmt.Sprintf("str:%s", s)
					default:
						parts[i] = "undefined"
					}
				}
				want = append(want, strings.Join(parts, "|"))
			}
			sort.Strings(want)
			if len(want) == 0 {
				t.Fatal("the constraint keeps nothing, so this case proves nothing")
			}
			if !slices.Equal(got, want) {
				t.Errorf("relay gave %d rows, evaluation %d", len(got), len(want))
				for i := range min(len(got), len(want)) {
					if got[i] != want[i] {
						t.Errorf("  first difference at %d:\n   got  %s\n   want %s", i, got[i], want[i])
						break
					}
				}
			}
		})
	}
}

// TestQueryColumnsRefusesBeforeAnyBatch: a relay that returned a sequence and only then found it
// could not serve one would be indistinguishable from a query that matched nothing. That is the
// failure this API exists to prevent, so it must not be one it can cause.
func TestQueryColumnsRefusesBeforeAnyBatch(t *testing.T) {
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := range 50 {
		ad := classad.New()
		ad.InsertAttr("Idx", int64(i))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// No accelerator yet: refused, and refused with the sequence nil rather than empty.
	seq, err := d.QueryColumns("true", []string{"Idx"}, false)
	if err == nil {
		t.Fatal("served a table with no columnar accelerator")
	}
	if seq != nil {
		t.Error("refused and still handed back a sequence; a caller could range it and see nothing")
	}

	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	// A constraint whose attributes are only known once it runs cannot be planned columnar.
	if _, err := d.QueryColumns(`eval(strcat("Id", "x")) == 1`, []string{"Idx"}, false); err == nil {
		t.Error("served a non-native constraint")
	}
	// Nothing asked for.
	if _, err := d.QueryColumns("true", nil, false); err == nil {
		t.Error("served an empty attribute list")
	}
	// A constraint that does not parse is a caller error, and a different one.
	if _, err := d.QueryColumns("Idx <<< 3", []string{"Idx"}, false); err == nil {
		t.Error("accepted an unparseable constraint")
	} else if strings.Contains(err.Error(), "cannot serve") {
		t.Errorf("a bad constraint was reported as an unservable table: %v", err)
	}
}

// TestQueryColumnsStatsReportTheSplit: the stats are the only thing that says a projection was
// served from the records rather than the columns. A caller that asked for a cold attribute gets
// the right answer at the speed of the path this API exists to replace, and nothing else in the
// result mentions it.
func TestQueryColumnsStatsReportTheSplit(t *testing.T) {
	d, _ := columnTable(t, 6000)
	hot := []string{"Idx", "Owner", "Ratio", "Flag"}

	var warm collections.ColumnScanStats
	seq, err := d.QueryColumnsStats("true", hot, false, &warm)
	if err != nil {
		t.Fatal(err)
	}
	rows := len(renderBatches(seq, hot))
	if warm.Rows != rows {
		t.Errorf("stats say %d rows, the sequence yielded %d", warm.Rows, rows)
	}
	if warm.ColumnCells == 0 {
		t.Fatalf("no cell came from the columns: %+v", warm)
	}

	// The same query with one attribute the schema does not carry: still correct, and the stats
	// say what it cost.
	cold := append(slices.Clone(hot), "NoSuchAttributeAnywhere")
	var mixed collections.ColumnScanStats
	seq, err = d.QueryColumnsStats("true", cold, false, &mixed)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(renderBatches(seq, cold)); n != rows {
		t.Errorf("the mixed projection yielded %d rows, want %d", n, rows)
	}
	if mixed.ColumnCells == 0 {
		t.Errorf("one unknown attribute gave up every column: %+v", mixed)
	}
}

// TestQueryColumnsRedactedWithholdsSealedValues: the relay ships VALUES, so the entitlement has
// to hold here and not only on the row reads. The entitled arm is the premise -- an assertion
// that a redacted read contains no secret passes just as well against a table that has none.
func TestQueryColumnsRedactedWithholdsSealedValues(t *testing.T) {
	// PERSISTENT, and that is load-bearing rather than incidental. An in-memory table has no
	// at-rest to encrypt: it holds EncryptedAttrs values in the clear and a redacted read
	// returns them, which the row reads do too. Written against one, this test watches a
	// keyless read hand back every secret and calls it a pass for the wrong reason.
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 13, PoolKeys: []KEK{poolKey("POOL")},
		EncryptedAttrs: []string{"Secret"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	tx := d.Begin()
	for i := range 6000 {
		ad := classad.New()
		ad.InsertAttr("Idx", int64(i))
		ad.InsertAttrString("Secret", fmt.Sprintf("s3cret-%d", i%7))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	d.c.ColumnarizeSealed()
	if !d.SealsValues() {
		t.Fatal("this table does not seal values, so withholding them proves nothing")
	}
	attrs := []string{"Idx", "Secret"}

	seq, err := d.QueryColumns("true", attrs, false)
	if err != nil {
		t.Fatal(err)
	}
	open := renderBatches(seq, attrs)
	secrets := 0
	for _, r := range open {
		if strings.Contains(r, "s3cret-") {
			secrets++
		}
	}
	if secrets == 0 {
		t.Fatal("the entitled read saw no secrets, so withholding them proves nothing")
	}

	seq, err = d.QueryColumns("true", attrs, true)
	if err != nil {
		t.Fatal(err)
	}
	keyless := renderBatches(seq, attrs)
	for _, r := range keyless {
		if strings.Contains(r, "s3cret-") {
			t.Fatalf("the keyless read produced a sealed value: %s", r)
		}
	}
	if len(keyless) != len(open) {
		t.Errorf("the keyless read produced %d rows, the entitled one %d: withholding a value "+
			"must not drop the record", len(keyless), len(open))
	}
}
