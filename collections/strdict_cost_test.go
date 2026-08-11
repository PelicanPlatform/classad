package collections

import (
	"testing"
	"time"
)

// What the string dictionary COSTS, on the real OSPool slot corpus (~561 attributes, ~25.7 KB per ad),
// measured by building the same corpus twice with the encoding switched rather than estimated.
//
// Measured, 827 accelerator-covered records in 8 blocks:
//
//	accelerator without dictionaries   571.4 B/record   2.2% of a 25.7 KB ad
//	accelerator with dictionaries      649.4 B/record   2.5%
//	the dictionaries themselves         78.0 B/record   0.30% of an ad, +13.6% of the accelerator
//	seal-time build                    1.85s -> 1.74s   no measurable cost
//
// Most of that 78 B is the DICTIONARY (58 KB) rather than the codes (5.9 KB), and it is the price of the
// additive design: a dictionary-encoded field's values are also still in the positional region, so the
// duplication is ~42% of the string region. Making the dictionary authoritative -- dropping those fields
// from the positional region and having reconstruct splice them back in positional order -- would turn this
// cost into a saving. That is the follow-up, and this test is how to tell whether it worked.
//
// Note the accelerator is only ~2% of a raw ad because it is a PROJECTION: the full ad lives compressed in
// the segment arena, and the accelerator holds the schema'd scalar fields plus a cold tail.
func TestStrDictCost(t *testing.T) {
	type stats struct {
		blocks, records                              int
		bits, hotCol, coldNum, str, cold, dict, code int
		dictFields                                   int
		build                                        time.Duration
	}
	measure := func(dict bool) stats {
		old := strDictEnabled
		strDictEnabled = dict
		defer func() { strDictEnabled = old }()
		start := time.Now()
		c, _ := ospoolFixture(t)
		var st stats
		st.build = time.Since(start)
		defer c.Close()
		for _, sh := range c.shards {
			_, ws := sh.snapshot()
			for _, w := range ws {
				seg := w.seg.colblk.Load()
				if seg == nil || seg.schema() == nil {
					continue
				}
				for _, b := range seg.blocks {
					st.blocks++
					st.records += b.n
					st.bits += len(b.bits)
					st.hotCol += len(b.hotCol)
					st.coldNum += len(b.coldNumComp)
					st.str += len(b.strComp)
					st.cold += len(b.coldComp)
					st.dict += len(b.strDictComp)
					st.code += len(b.strCodeComp)
					st.dictFields += len(b.strDict)
				}
			}
			releaseWindows(ws)
		}
		return st
	}
	off := measure(false)
	on := measure(true)
	if on.records == 0 || off.records == 0 {
		t.Skip("no records")
	}
	tot := func(s stats) int { return s.bits + s.hotCol + s.coldNum + s.str + s.cold + s.dict + s.code }
	t.Logf("OSPool corpus: %d records in %d blocks", on.records, on.blocks)
	t.Logf("  accelerator bytes without dictionaries: %d (%.1f B/record)",
		tot(off), float64(tot(off))/float64(off.records))
	t.Logf("  accelerator bytes with dictionaries:    %d (%.1f B/record)  +%.1f%%",
		tot(on), float64(tot(on))/float64(on.records),
		100*float64(tot(on)-tot(off))/float64(tot(off)))
	t.Logf("  added: dict=%d code=%d over %d dictionary-encoded fields (%.1f B/record)",
		on.dict, on.code, on.dictFields, float64(on.dict+on.code)/float64(on.records))
	t.Logf("  region breakdown with dict: bits=%d hotCol=%d coldNum=%d str=%d coldTail=%d",
		on.bits, on.hotCol, on.coldNum, on.str, on.cold)
	t.Logf("  seal-time build: %v without -> %v with (%.2fx)",
		off.build, on.build, float64(on.build)/float64(off.build))
}

// TestOSPoolAdSize reports the corpus size so the accelerator's cost can be stated as a fraction of a
// record rather than of the whole file -- the accelerator covers only the SEALED segments, so dividing the
// file size by the covered record count overstates a record badly.
func TestOSPoolAdSize(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	if len(ads) == 0 {
		t.Skip("no OSPool ads")
	}
	total := 0
	for _, ad := range ads {
		total += len(ad.MarshalOld())
	}
	t.Logf("corpus: %d ads, %d bytes marshaled, %.0f B/ad", len(ads), total, float64(total)/float64(len(ads)))
}
