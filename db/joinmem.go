package db

// What a held join row costs, and how a plan estimates it before holding any.
//
// MaxHashRows counts rows, and a row count does not express a memory budget: a held row is a
// DECODED ad, and what that costs is set by how many attributes it carries rather than by how
// many bytes it occupies on disk. Measured by holding 50,000 rows exactly as buildStage holds
// them -- decoded, in a bucket map -- and reading HeapAlloc:
//
//	attrs   disk B/row   heap B/row   ratio
//	    5          169          832    4.9x
//	   23          278        2,919   10.5x
//	   63          448        8,519   19.0x
//	  123          681       16,183   23.8x
//
// Two things follow. The expansion over the stored form is not a constant -- it moves by
// nearly 5x across this range -- so a byte size taken from the store cannot predict the hold.
// And the per-ATTRIBUTE cost is nearly flat at ~130 bytes, because what a decoded ad spends
// is per-attribute overhead (a map entry, a string header, a boxed value) that the columnar
// and dictionary encodings compress away on disk.
//
// So the model is linear in the attribute count, and at a realistic history width of 100+
// attributes the default MaxHashRows of 500,000 authorises several gigabytes for one join.

const (
	// hashRowBytesBase is what a held row costs before any attribute: the joinRow, its
	// bucket-map entry and the key string.
	hashRowBytesBase = 180
	// hashRowBytesPerAttr is the marginal cost of one attribute of a decoded ad.
	hashRowBytesPerAttr = 130
	// adWidthSample is how many ads are read to estimate a side's width. The width of a
	// table's ads varies far less than their count does, so a small sample answers it; this
	// runs while a plan is being chosen, so it must not become a scan.
	adWidthSample = 32
)

// estHashBytes is what holding rows rows of width-attribute ads is expected to cost.
func estHashBytes(rows, width int) int {
	if rows <= 0 || width < 0 {
		return 0
	}
	return rows * (hashRowBytesBase + hashRowBytesPerAttr*width)
}

// archiveAdWidth samples the side's ads for their attribute count.
//
// Sampled rather than taken from the schema: SchemaFit answers only where schema scanning is
// configured, and an archive without it would then be priced at a width of zero -- which
// reads as "holding this side is free" exactly where the estimate matters. A sample is a
// bounded read that every archive can answer.
//
// Reports false when the side yields nothing, so a caller can tell "narrow ads" from "no
// answer" instead of treating an empty table as a cheap one.
func archiveAdWidth(t *ArchiveTable) (int, bool) {
	ads, err := t.QueryLimit("true", adWidthSample)
	if err != nil {
		return 0, false
	}
	total, n := 0, 0
	for ad := range ads {
		total += ad.Size()
		n++
	}
	if n == 0 {
		return 0, false
	}
	return (total + n - 1) / n, true
}

// sideAdWidth samples any side's ads for their attribute count.
//
// A side it cannot sample reports false, and the byte budget is then not applied to that
// step: an unknown width must not be read as a narrow one, which would let the very sides
// this cannot price past the bound it exists to impose. MaxHashRows still applies there.
func (cat *Catalog) sideAdWidth(side JoinSide) (int, bool) {
	if a, ok := cat.ArchiveTable(side.Table); ok {
		return archiveAdWidth(a)
	}
	if d, ok := cat.Table(side.Table); ok {
		ads, err := d.Query("true")
		if err != nil {
			return 0, false
		}
		total, n := 0, 0
		for ad := range ads {
			total += ad.Size()
			n++
			if n >= adWidthSample {
				break
			}
		}
		if n == 0 {
			return 0, false
		}
		return (total + n - 1) / n, true
	}
	return 0, false
}
