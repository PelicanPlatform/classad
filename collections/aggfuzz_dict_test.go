package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/aggfuzz/gen"
)

// The aggregate differential fuzzer (db/aggfuzz_*.go) only tests what its fixtures reach, and the
// most expensive way to be wrong about that is to believe a fast path fired when it did not. Which
// READER a block uses is a property of the data, not of the query, and nothing above this package
// can see it -- so the fuzzer's claim that its string columns exercise BOTH string readers is
// asserted here, where the blocks are visible, against the very same generator.
//
// Without this the whole string half of the fuzzer could be running on the positional walk, and the
// dictionary group reader -- which is where a string GROUP BY's bugs live -- would be untested by a
// green run. It is not hypothetical: the collections fixtures that predate this one build ZERO
// dictionaries, because dictWorthwhile needs distinct*2 <= records in the block and their
// `user(i%512)` values over ~378-record row groups never clear it.
func TestAggFuzzFixtureBuildsStringDictionaries(t *testing.T) {
	t.Parallel()
	for _, shape := range gen.Shapes() {
		a, err := OpenArchive(ArchiveOptions{
			Dir:              t.TempDir(),
			SegmentSize:      shape.SegmentBytes,
			RowGroupBytes:    shape.RowGroupBytes,
			CategoricalAttrs: gen.CategoricalAttrs,
			ValueAttrs:       gen.ValueAttrs,
			ZoneAttrs:        gen.ZoneAttrs,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, ad := range shape.Ads() {
			if err := a.Append(ad); err != nil {
				t.Fatal(err)
			}
		}
		if !a.BuildAndEnableSchemaScan(2000, shape.HotTopN) {
			t.Fatalf("shape %d: no sealed segments to accelerate -- SegmentBytes is too large for %d records",
				shape.ID, shape.N)
		}
		// Owner: 40 distinct values, so distinct*2 clears any block. Host: distinct per record, so
		// it never does. Both are present in every shape, which is what keeps a run from missing a
		// reader however the other knobs move.
		ownerDict, blocks := dictStats(t, a.c, "Owner")
		hostDict, _ := dictStats(t, a.c, "Host")
		if blocks == 0 {
			t.Fatalf("shape %d: no columnar blocks", shape.ID)
		}
		if ownerDict == 0 {
			t.Errorf("shape %d: 0/%d blocks dictionary-encode Owner; the fuzzer's string GROUP BY "+
				"would never reach the dictionary reader", shape.ID, blocks)
		}
		if hostDict != 0 {
			t.Errorf("shape %d: %d/%d blocks dictionary-encode Host; it is meant to be the "+
				"high-cardinality case that takes the positional string walk", shape.ID, hostDict, blocks)
		}
		// The ESCAPE path. A column reader has to fall back for a record whose value does not fit
		// the slot the schema chose, and that fallback is only exercised if such records exist --
		// which depends on a width the derivation chooses, not on anything the generator can state
		// directly. So it is checked against the LIVE schema rather than assumed.
		st := a.c.schemaScan.Load()
		if st == nil {
			t.Fatalf("shape %d: the accelerator is not enabled", shape.ID)
		}
		memID, ok := a.c.intern.LookupID("Mem")
		if !ok {
			t.Fatalf("shape %d: Mem has no interned id", shape.ID)
		}
		fi, ok := st.schema.byID[memID]
		if !ok {
			t.Fatalf("shape %d: Mem is not a columnar field", shape.ID)
		}
		fl := st.schema.fields[fi]
		escapes := 0
		for i := 0; i < shape.N; i++ {
			if v, ok := shape.Ad(i).EvaluateAttrInt("Mem"); ok && !intFits(v, fl.width, fl.unsigned) {
				escapes++
			}
		}
		// Only a COLD int column can have escapes: refitHotIntWidths re-chooses a hot one's width
		// to cover ~every sampled value, so a hot Mem swallows its own outliers and the cold-tail
		// fallback is never taken. That is exactly why half the shapes keep the hot set small, and
		// exactly the kind of thing that can stop being true without any test noticing.
		if shape.HotTopN <= 4 && escapes == 0 {
			t.Errorf("shape %d: no record escapes the %d-byte Mem column, so the cold-tail "+
				"fallback is not exercised anywhere in this shape", shape.ID, fl.width)
		}
		// The ZONE shape, for the same reason: a zone map's Full flag is what separates "every value
		// in the segment is in these bounds" from "every value a comparison could be true of is".
		// Only the second is enough to prune; only the first is enough to attribute a whole segment
		// to a range. Every zone attribute used to be numeric in every record, so every zone came
		// out Full and the distinction decided nothing anywhere the fuzzer could reach.
		//
		// Three attributes now break it, each a different way, and which way is the point: a NaN is
		// a value no comparison is true of, so it is left OUT of the bounds and clears Full; an
		// absent attribute does the same; an expression is a value only evaluation can give, so the
		// attribute gets no entry in that segment at all. Whether each still lands where it is
		// meant to depends on moduli and segment sizes set elsewhere, so it is counted rather than
		// assumed.
		full, partial, absent := map[string]int{}, map[string]int{}, map[string]int{}
		for _, sh := range a.c.shards {
			_, wins := sh.snapshot()
			for _, w := range wins {
				if len(w.zones) == 0 {
					continue // unsealed: no zone map at all, which says nothing about any attribute
				}
				for _, name := range gen.ZoneAttrs {
					id, ok := a.c.intern.LookupID(name)
					if !ok {
						continue
					}
					switch z, has := w.zones[id]; {
					case !has:
						absent[name]++
					case z.Full:
						full[name]++
					default:
						partial[name]++
					}
				}
			}
			releaseWindows(wins)
		}
		for _, c := range []struct {
			attr, want string
			n          int
		}{
			// The controls: a number in every record, so nothing clears Full and nothing
			// suppresses the entry. If one of these ever goes partial the comparison below stops
			// meaning anything, because then EVERY zone is partial.
			{"Ts", "non-Full zones", partial["Ts"] + absent["Ts"]},
			{"ClusterId", "non-Full zones", partial["ClusterId"] + absent["ClusterId"]},
			{"Mem", "non-Full zones", partial["Mem"] + absent["Mem"]},
		} {
			if c.n != 0 {
				t.Errorf("shape %d: %s has %d %s; it holds a number in every record and is the "+
					"control for the attributes that do not", shape.ID, c.attr, c.n, c.want)
			}
		}
		if partial["Rate"] == 0 {
			t.Errorf("shape %d: every Rate zone came out Full (%d of them, %d absent), so no query "+
				"in the run reaches a segment whose bounds leave a record out", shape.ID, full["Rate"], absent["Rate"])
		}
		if partial["Rare"] == 0 {
			t.Errorf("shape %d: every Rare zone came out Full (%d of them, %d absent); Rare is "+
				"written in 30%% of records and its zone must say so", shape.ID, full["Rare"], absent["Rare"])
		}
		if absent["Calc"] == 0 {
			t.Errorf("shape %d: Calc has a zone in every segment (%d Full, %d partial); its "+
				"expression-valued records are meant to suppress the entry outright", shape.ID,
				full["Calc"], partial["Calc"])
		}
		t.Logf("shape %02d: %d blocks, Owner dict in %d, Host dict in %d, Mem %d-byte with %d escapes; "+
			"zones Full %v partial %v absent %v",
			shape.ID, blocks, ownerDict, hostDict, fl.width, escapes, full, partial, absent)
		a.Close()
	}
}
