package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// What keeping the rows as bytes is worth, against the rung that builds values.
//
// Same scan, same ranking, same answer -- the differential test holds them to that. The only
// difference is what a kept row IS, so the gap here is the cost of materializing every matching
// record's projected attributes into classad.Values on the way past.
//
// The row WIDTH is the axis, because that is what the cost scales with: the group key and the
// order value are one attribute each on both rungs, and everything else in the projection is
// what the value rung decodes per record and this one does not.
func benchTopKRungs(b *testing.B, width int) {
	const groups, rows = 50, 2000
	d, err := OpenConfig(Config{Dir: b.TempDir(), SegmentSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	n := 0
	attrs := make([]string, 0, width)
	for i := 0; i < width; i++ {
		attrs = append(attrs, fmt.Sprintf("Attr%d", i))
	}
	for g := 0; g < groups; g++ {
		for r := 0; r < rows; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", g))
			ad.InsertAttr("QDate", int64(g*100000+r))
			for i, a := range attrs {
				ad.InsertAttrString(a, fmt.Sprintf("value-%d-%d", i, r))
			}
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	spec := TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}}, OrderBy: OrderOne("QDate", true), K: 5,
		Attrs: attrs,
	}
	b.Run("values", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := d.TopKRowsPerGroup("true", spec)
			if err != nil {
				b.Fatal(err)
			}
			if len(got) != groups {
				b.Fatalf("groups = %d, want %d", len(got), groups)
			}
		}
	})
	b.Run("wire", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := d.TopKRowsPerGroupWire("true", spec)
			if err != nil {
				b.Fatal(err)
			}
			if len(got) != groups {
				b.Fatalf("groups = %d, want %d", len(got), groups)
			}
		}
	})
}

func BenchmarkTopKRungs(b *testing.B) {
	for _, width := range []int{1, 5, 20} {
		b.Run(fmt.Sprintf("attrs=%d", width), func(b *testing.B) { benchTopKRungs(b, width) })
	}
}
