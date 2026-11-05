package collections

import (
	"sort"
	"testing"
)

// TestProductionBlockWidthIsSetByBytesNotRows pins what the vector executor's batch width actually
// IS on real ads, because a cost model was written against the wrong number and the number is not
// visible from any constant.
//
// colGroupMaxRows is 8192 and reads like the batch size. It is a BACKSTOP. What binds on real ads is
// colGroupTargetBytes: a group seals at 128 KiB of uncompressed row form, and an OSPool slot ad is
// ~8.6 KB of that, so a block holds about sixteen records and the row cap is never approached. A
// vector register at that width is a few hundred bytes, not a few hundred kilobytes, and every
// argument about the register file evicting the batch from cache is off by the ratio between them.
//
// The row cap would bind only on ads narrow enough that 8192 of them fit in the byte budget, about
// 16 bytes of row form each, which no ClassAd worth storing is. Measured on synthetic ads: 30
// numeric attributes still seals at 2048 records on the byte rule. So the row cap is not a second
// regime this test declines to cover -- it is one that essentially never happens, and reading 8192
// as the batch width is reading a bound that nothing reaches.
//
// What this checks is therefore the one regime there is, two ways: that the width is far below the
// row cap, and that the byte budget is what put it there. A change that stopped the byte rule
// binding fails here rather than silently moving the batch width by a factor of hundreds.
//
// Checked by mutation: making colGrouping.full ignore targetBytes takes the record-weighted mean
// width from 14.7 to 969 and fails both assertions here. Exactly five tests in the package fail
// under it and the rest pass -- this one, TestRowGroupsBoundedByBytes, TestRemnantDictReadsByteIdentical,
// and TestRowGroupsNoRunts and TestColGroupsAlignedToEight, which fail by reporting that their
// fixtures no longer exercise the byte rule at all.
func TestProductionBlockWidthIsSetByBytesNotRows(t *testing.T) {
	sample := loadCorpus(t)
	// Enough to seal several segments and so produce many blocks; the width is a per-block property,
	// so more records buy confidence in the distribution rather than a different answer, and these
	// are ~8.6 KB each.
	c := populate(t, sample, 20000)
	// The per-ad stored size, captured BEFORE the accelerator is enabled, because that is the size the
	// byte budget was applied to: a group seals when the records it has taken reach
	// colGroupTargetBytes. Enabling columnarizes, and columnarizing removes those very bytes from the
	// arena -- the schema'd values move into the payload and the records keep only their remnants --
	// so measuring the arena afterwards divides the budget by the size of what is LEFT and then asks
	// why the blocks are not wider. On this corpus that is 3477 B/ad against the 8569 the blocks were
	// built from, and the expectation comes out 2.5x too high.
	//
	// It went unnoticed because the rewrite never ran here: an in-memory collection refused to
	// columnarize, so a test that explicitly asks for the columnar-native regime (the loop below) was
	// measuring sidecar blocks over an untouched arena.
	bytesPerAd := float64(liveBytes(c)) / float64(c.Len())
	if !c.BuildAndEnableSchemaScan(1<<16, 8) {
		t.Skip("schema scan did not enable; there is no columnar block to measure")
	}
	for c.ColumnarizeSealed() > 0 {
	}
	if info := c.SchemaScanInfo(); info.ColumnarSegments == 0 {
		t.Fatal("no sealed segment was columnarized: the loop above did nothing, so this measures " +
			"sidecar blocks rather than the regime the test asks for")
	}

	widths, records := blockWidths(t, c)
	if len(widths) == 0 {
		t.Fatal("no columnar blocks built; this test is not testing what it says")
	}
	sort.Ints(widths)
	// Record-weighted rather than block-weighted: the question is the width a RECORD is evaluated
	// at, and a segment's short final group would otherwise drag the plain mean down.
	var sq int
	for _, w := range widths {
		sq += w * w
	}
	mean := float64(sq) / float64(records)

	t.Logf("%d blocks over %d records: p50=%d max=%d record-weighted mean width=%.1f (%.0f B/ad)",
		len(widths), records, widths[len(widths)/2], widths[len(widths)-1], mean, bytesPerAd)

	// Two orders of magnitude below the row cap. The bound is loose on purpose -- the point is the
	// SCALE, not the exact width, which moves with the corpus and with the codec.
	if mean >= colGroupMaxRows/16 {
		t.Errorf("record-weighted mean block width %.1f is near the %d row backstop; the byte budget "+
			"is supposed to bind first on ads this wide", mean, colGroupMaxRows)
	}
	// And it is the byte budget that put it there: the width should land within a factor of two of
	// the budget divided by the per-ad row size.
	want := float64(colGroupTargetBytes) / bytesPerAd
	if mean > 2*want || mean < want/2 {
		t.Errorf("record-weighted mean block width %.1f is not explained by the byte budget "+
			"(%d B / %.0f B per ad = %.1f records)", mean, colGroupTargetBytes, bytesPerAd, want)
	}
}

// blockWidths returns every sealed columnar block's record count, and the total they cover.
func blockWidths(tb testing.TB, c *Collection) (widths []int, records int) {
	tb.Helper()
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			seg := w.seg.colblk.Load()
			if seg == nil {
				continue
			}
			for _, blk := range seg.blocks {
				widths = append(widths, blk.n)
				records += blk.n
			}
		}
	}
	return widths, records
}
