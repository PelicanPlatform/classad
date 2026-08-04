package collections

import (
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestSchemaTieredSize measures the popularity-tiered layout: the top-K most popular numeric
// int/real fields kept UNCOMPRESSED (fast scan), the remaining numerics compressed as a
// standalone column-group (partial decompression), bools + strings + cold each compressed.
// Popularity here is proxied by presence (the static corpus has no query demand; the real
// system uses c.demand read counts, as ANALYZE/RefreshHotSet do). Sweeps K.
func TestSchemaTieredSize(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	_, wires := encodeOSPool(t, ads)
	var baseCat []byte
	for _, w := range wires {
		baseCat = append(baseCat, w...)
	}
	baseZ := zstdLen(baseCat)

	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	recs := make([][]byte, len(wires))
	present := make([]int, len(s.fields))
	for i, w := range wires {
		recs[i] = s.encode(wire.Ad(w))
		esc := recs[i][:s.escBytes]
		for j := range s.fields {
			if !testBit(esc, j) {
				present[j]++
			}
		}
	}
	// Scannable numeric fields (int/real), ranked by presence (popularity proxy), desc.
	var numeric []int
	for j := range s.fields {
		if s.fields[j].kind == akInt || s.fields[j].kind == akReal {
			numeric = append(numeric, j)
		}
	}
	sort.Slice(numeric, func(a, b int) bool { return present[numeric[a]] > present[numeric[b]] })

	// Fixed per-record parts shared across K: string region + cold tail (each its own stream),
	// and the bool bitset (always in the compressed group).
	var strCat, coldCat, boolCat []byte
	for _, r := range recs {
		_, str, cold := s.splitRecord(r)
		strCat = append(strCat, str...)
		coldCat = append(coldCat, cold...)
		boolCat = append(boolCat, r[s.escBytes:s.escBytes+s.boolBytes]...) // bool bitset
	}
	strZ, coldZ, boolZ := zstdLen(strCat), zstdLen(coldCat), zstdLen(boolCat)

	widthOf := func(j int) int { return s.fields[j].width }
	for _, K := range []int{0, 5, 10, 20, 40, len(numeric)} {
		if K > len(numeric) {
			K = len(numeric)
		}
		hot := map[int]bool{}
		hotWidth := 0
		for i := 0; i < K; i++ {
			hot[numeric[i]] = true
			hotWidth += widthOf(numeric[i])
		}
		hotRaw := hotWidth * len(wires) // uncompressed hot int/real slots, fixed stride

		// Cold int/real fields, columnar (per field contiguous) for best compression.
		var coldNum []byte
		for _, j := range numeric {
			if hot[j] {
				continue
			}
			off := s.escBytes + s.fields[j].off
			w := s.fields[j].width
			for _, r := range recs {
				coldNum = append(coldNum, r[off:off+w]...)
			}
		}
		coldNumZ := zstdLen(coldNum)

		total := hotRaw + coldNumZ + boolZ + strZ + coldZ
		t.Logf("K=%-3d hotWidth=%dB | hot UNCOMPRESSED=%dKB + coldNum zstd=%dKB + bool=%dKB + str=%dKB + cold=%dKB = %dKB vs %dKB (%+.1f%%)",
			K, hotWidth, hotRaw/1024, coldNumZ/1024, boolZ/1024, strZ/1024, coldZ/1024, total/1024, baseZ/1024,
			100*float64(total-baseZ)/float64(baseZ))
	}
}
