package collections

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Is the column producer worth the wiring above it?
//
// The question is NOT whether it beats the text path -- it obviously does, and that path was
// already replaced by the wire-native read. The question the design asks is whether it beats
// SCANRAWWIRE PLUS THE DECODE A CONSUMER STILL HAS TO DO, because that is what a caller runs
// today and what a column producer would replace. A producer that merely matched it would not
// earn an API in collections, an op in db, a wire op in dbrpc and a consumer above them.
//
// Both arms produce the SAME THING: a typed value per attribute per matching record. Arm A reads
// the projected wire ad and decodes each attribute from it, which is what a consumer does with
// what ScanRawWire hands back. Arm B reads the columns.
//
// One store, built once and shared, because building it dominates either arm. Both orders are
// run, because the first arm pays for the page cache and the block cache the second one then
// finds warm -- which has flattered an arm in this package before.

func benchProduceStore(tb testing.TB, n int) (*Collection, []string) {
	tb.Helper()
	c, err := Open(Options{Dir: tb.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	for i := range n {
		ad, err := classad.ParseOld(produceAd(i))
		if err != nil {
			tb.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		tb.Fatal("no schema derived")
	}
	c.ColumnarizeSealed()
	// The attributes the derived schema carries: the shape a producer is for. A projection
	// naming cold-tail attributes measures the record path in both arms and answers nothing.
	return c, []string{"Idx", "Owner", "Ratio", "Flag", "Derived"}
}

// benchWireArm is what a consumer runs today: the wire-native scan, plus the per-record decode
// of each projected attribute into a typed value.
func benchWireArm(b *testing.B, c *Collection, attrs []string, project bool) {
	lookups := make([]fieldLookup, len(attrs))
	for i, a := range attrs {
		id, ok := c.intern.LookupID(a)
		if !ok {
			b.Fatalf("%s is not interned", a)
		}
		lookups[i] = c.attrLookup(id)
	}
	// REF-CLOSED, and that is not a detail. A projection naming a COMPUTED attribute without
	// the attributes its expression reads ships a row that cannot be evaluated, and the decode
	// then reports undefined for it -- less work and a wrong answer, which would have flattered
	// this arm. TestTheBenchmarkArmsProduceTheSameValues is what caught that.
	//
	// project=false ships the whole ad instead, which is the other thing a consumer can do and
	// is measured separately: closing the projection is itself work, and charging it to this
	// arm when a caller could avoid it would flatter the other one.
	scan := c.ScanRawWireRefs
	proj := attrs
	if !project {
		proj = nil
	}
	var sink int64
	for b.Loop() {
		for raw := range scan(proj, false) {
			ad := wire.Ad(raw)
			for i := range lookups {
				node, found := lookups[i](nil, ad)
				st, num, str := nodeCell(node, found, nil)
				sink += int64(st) + num + int64(len(str))
			}
		}
	}
	_ = sink
}

func benchColumnArm(b *testing.B, c *Collection, attrs []string) {
	q, err := vm.Parse("true")
	if err != nil {
		b.Fatal(err)
	}
	var sink int64
	for b.Loop() {
		_, ok := c.ScanColumns(q, attrs, func(bat *Batch) bool {
			for i := range bat.Cols {
				col := &bat.Cols[i]
				for r := range bat.N {
					sink += int64(col.Kind[r]) + col.Num[r] + int64(len(col.Str[r]))
				}
			}
			return true
		})
		if !ok {
			b.Fatal("ScanColumns declined")
		}
	}
	_ = sink
}

func BenchmarkProducerVsWire(b *testing.B) {
	c, attrs := benchProduceStore(b, 20000)
	b.Run("wire-projected+decode", func(b *testing.B) { benchWireArm(b, c, attrs, true) })
	b.Run("wire-whole+decode", func(b *testing.B) { benchWireArm(b, c, attrs, false) })
	b.Run("columns", func(b *testing.B) { benchColumnArm(b, c, attrs) })
	// Flipped, because the first arm warms what the second one reads.
	b.Run("columns(second-run)", func(b *testing.B) { benchColumnArm(b, c, attrs) })
	b.Run("wire-whole+decode(second-run)", func(b *testing.B) { benchWireArm(b, c, attrs, false) })
}

// TestTheBenchmarkArmsProduceTheSameValues: the benchmark above claims one arm is an order of
// magnitude faster than the other. That is a result only if they produce the SAME THING -- an arm
// that quietly yields fewer values, or coarser ones, would post exactly this kind of number.
//
// Compared as sorted multisets because the two walk the records in different orders: the wire scan
// visits them as stored, the column scan block by block.
func TestTheBenchmarkArmsProduceTheSameValues(t *testing.T) {
	c, attrs := benchProduceStore(t, 3000)

	lookups := make([]fieldLookup, len(attrs))
	for i, a := range attrs {
		id, ok := c.intern.LookupID(a)
		if !ok {
			t.Fatalf("%s is not interned", a)
		}
		lookups[i] = c.attrLookup(id)
	}
	var fromWire []string
	for raw := range c.ScanRawWireRefs(attrs, false) {
		ad := wire.Ad(raw)
		parts := make([]string, len(lookups))
		for i := range lookups {
			node, found := lookups[i](nil, ad)
			st, num, str := nodeCell(node, found, nil)
			parts[i] = fmt.Sprintf("%d/%d/%s", st, num, str)
		}
		fromWire = append(fromWire, strings.Join(parts, "|"))
	}

	q, _ := vm.Parse("true")
	var fromColumns []string
	if _, ok := c.ScanColumns(q, attrs, func(bat *Batch) bool {
		for r := range bat.N {
			parts := make([]string, len(bat.Cols))
			for i := range bat.Cols {
				col := &bat.Cols[i]
				var st uint8
				switch col.Kind[r] {
				case ValueInt:
					st = vm.VsInt
				case ValueReal:
					st = vm.VsReal
				case ValueBool:
					st = vm.VsBool
				case ValueString:
					st = vm.VsString
				case ValueUndefined:
					st = vm.VsUndef
				default:
					st = vm.VsError
				}
				parts[i] = fmt.Sprintf("%d/%d/%s", st, col.Num[r], col.Str[r])
			}
			fromColumns = append(fromColumns, strings.Join(parts, "|"))
		}
		return true
	}); !ok {
		t.Fatal("ScanColumns declined")
	}

	if len(fromWire) == 0 {
		t.Fatal("the wire arm produced nothing")
	}
	sort.Strings(fromWire)
	sort.Strings(fromColumns)
	if !slices.Equal(fromWire, fromColumns) {
		t.Errorf("the arms disagree: %d rows from the wire, %d from the columns%s",
			len(fromWire), len(fromColumns), firstDiff(fromColumns, fromWire))
	}
}
