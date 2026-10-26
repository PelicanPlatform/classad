// Package gen generates the archives and the queries the aggregate differential fuzzer runs.
//
// A grouped aggregate over an archive can be answered by several different pieces of code -- an
// index/metadata path that reads no records, a columnar accelerator path, and a record scan -- and
// they must all produce identical answers. The fuzzer compares them; this package's whole job is to
// produce data and queries where the FAST paths actually fire. Random data lands almost entirely on
// the record scan, and a fuzz run where every query takes the reference path is a green run that
// tested nothing, so every attribute below exists to steer one specific decision in the storage
// layer:
//
//   - Schema inference drops attributes. buildAdSchema keeps an attribute only when its dominant
//     storable kind is present in at least 90% of the sampled ads, so the columnar columns here are
//     written in >= ~92% of records, and `Rare` is deliberately written in 30% so there is also an
//     attribute that is NOT columnar whatever else happens.
//   - String dictionaries only form when they pay: dictWorthwhile needs distinct*2 <= records in
//     the block. `Owner` has 40 distinct values and takes the dictionary reader; `Host` is distinct
//     per record and takes the positional string walk. Both are present in every fixture, so a run
//     cannot miss either reader.
//   - Zone maps only prune SEALED segments, so Shape.SegmentBytes is small (tens of KiB) and a
//     fixture seals a dozen or so. `Ts` advances monotonically, which is what makes a segment's zone
//     fall inside one bucket and lets the bucketed index path attribute it wholesale.
//   - Column width drives the raw/vectorized read, so the integer columns span 1, 2, 4, 6 and 8
//     bytes, signed and unsigned, and `Mem` has a ~2% tail that does not fit its 2-byte slot (under
//     baseFit = 0.95 the width is still chosen) so some records escape into the cold tail.
//   - Constraints have to be winnable, so the literals a query draws come from the value domains
//     below rather than from the whole int64 range.
//
// The other half is values that must appear BY CONSTRUCTION rather than by luck, because that is
// where the bugs have been. Group identity is the RENDERED TEXT, so two values that render alike
// have to land in one group: `Site` carries an absent attribute, the UNDEFINED keyword and the
// literal string "undefined" (three spellings, one group), plus the number 42 beside the string
// "42"; `NumLike` is a string column whose text is the decimal spelling of an integer; `Rate` mixes
// integral reals (3.0, which renders "3") with integers; `Flag` is a boolean; `Expr` holds an
// expression where a literal usually is; `Err` evaluates to ERROR; `Rate` also carries +/-Inf
// literals and a stored NaN, and `Calc` carries a NaN reached by arithmetic instead, which is a
// different storage path and the one production data takes.
//
// Everything here is a pure function of (Shape, record index): no clock, no rand. That is what lets
// a failure be reported as a list of record indices and replayed exactly, and what lets the
// shrinker rebuild subsets.
package gen

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// Index configuration the fixture must be created with. These are not arbitrary: the categorical
// index over Owner is the only thing the index tier can read group counts out of, and the zone maps
// over Ts/ClusterId/Mem are what let a constrained count be attributed to a whole segment.
var (
	// CategoricalAttrs is indexed per segment by value; Owner is a string literal in EVERY record,
	// which is what lets the postings account for all of them (the index path compares its summed
	// total against the record count and declines when they disagree).
	CategoricalAttrs = []string{"Owner"}
	// ValueAttrs gets a per-segment value index (and a zone map for free).
	//
	// Rate is here for one reason: it holds a NaN in a minority of records, and a value index is
	// keyed by the value, so a NaN key can never be looked up again -- the index built a sorted
	// key list holding a NaN whose own lookup returned a nil bitmap and dereferenced it. This
	// fixture crashed on that during index build, which is why Rate was kept out of the set while
	// the bug stood. It is fixed now, so the set covers it: drop Rate from here and the only
	// indexed column is one that cannot hold a NaN, which is the shape that let the crash exist.
	ValueAttrs = []string{"ClusterId", "Rate"}
	// ZoneAttrs get per-segment [min,max] zone maps, and the set is deliberately mixed, because a
	// zone is only interesting where it is NOT total. Ts/ClusterId/Mem hold a number in every
	// record, so their zones come out Full and a segment can be attributed to a range wholesale
	// without reading it; before the other three were added, every zone in every fixture was Full
	// and the Full guard never decided anything.
	//
	// The other three each clear Full a different way. Rate holds a NaN in a minority of records
	// and no comparison is true of a NaN, so it is left out of the bounds. Calc holds an
	// expression in a minority, which no bound can cover at all. Rare is simply absent from 70% of
	// records. The resulting zones bound only the values they CAN, so a range every bounded value
	// satisfies reads as always-true over the zone -- and attributing a segment wholesale on that
	// basis would count the records the zone left out. Full is the only thing between those two
	// readings, and this is the data that makes it matter.
	ZoneAttrs = []string{"Ts", "ClusterId", "Mem", "Rate", "Calc", "Rare"}
)

// Shape is one generated archive: a record count plus the storage knobs that decide which
// accelerator tiers its segments can serve.
type Shape struct {
	ID int
	// N is the record count. Large enough that several segments seal at SegmentBytes, small
	// enough that the reference scan is milliseconds.
	N int
	// RunLen is how many consecutive records share an Owner. 1 is the interleaved extreme (40
	// distinct values in every block, which still dictionary-encodes); 256 is the submit-burst
	// extreme (one or two per block).
	RunLen int
	// SegmentBytes is the sealed-segment size. Small, so segments actually seal: at the 8 MiB
	// default a fixture this size stays in one unsealed segment and no zone map ever prunes.
	SegmentBytes int
	// RowGroupBytes is the columnar row-group budget. It sets how many records share a block, and
	// so how many distinct values a block sees -- which is what dictWorthwhile decides on. 0 takes
	// the default.
	RowGroupBytes int
	// WideInts moves Disk and Big from 4-byte/1-byte columns to 6-byte/8-byte ones, so a run
	// covers the wide end of the slot widths as well as the narrow one.
	WideInts bool
	// HotTopN is how many numeric columns are kept in the HOT tier, and it is not only a
	// performance knob: a hot int column is re-fitted to cover ~every sampled value
	// (refitHotIntWidths), so nothing escapes it, while a cold one keeps the 0.95 fit percentile
	// and its outliers live in the cold tail. Half the shapes therefore keep the hot set small, so
	// the fuzzer actually reaches the cold-tail fallback on a NUMERIC column instead of only on a
	// string one.
	HotTopN int
}

// Shapes is the fixture set: the cross product of the knobs above, so a sweep covers every string
// clustering against both segment sizes and both integer width regimes.
func Shapes() []Shape {
	var out []Shape
	for _, runLen := range []int{1, 8, 64, 256} {
		for _, seg := range []int{16 << 10, 64 << 10} {
			for _, wide := range []bool{false, true} {
				// The larger segment takes a small row group, so block sizes vary with segment
				// size rather than needing a fourth axis.
				rg := 0
				if seg > 16<<10 {
					rg = 8 << 10
				}
				// A small hot set where the integers are narrow, a large one where they are
				// wide: that pairs the cold-tail-escape case with the narrow columns and the
				// all-hot, nothing-escapes case with the wide ones, without a fourth axis.
				hot := 3
				if wide {
					hot = 32
				}
				out = append(out, Shape{ID: len(out), N: 1100, RunLen: runLen, SegmentBytes: seg,
					RowGroupBytes: rg, WideInts: wide, HotTopN: hot})
			}
		}
	}
	return out
}

// ShapeByID returns the shape with that id, wrapping, so any fuzz byte selects a valid shape.
func ShapeByID(id int) Shape {
	s := Shapes()
	return s[((id%len(s))+len(s))%len(s)]
}

// Source renders record i of this shape in old-ClassAd text. It is the reproducer form: paste the
// lines a failure prints into a fixture and the same records come back.
//
// NaN is the one value this text cannot carry AS A LITERAL, which is a narrower claim than "text
// cannot express NaN". The VALUE is perfectly expressible and both engines agree on it: `0.0/0.0`
// evaluates to NaN here and in libclassad (`0/0` is integer division and gives ERROR, not NaN), and
// `real("NaN")` is the canonical spelling. What old-ClassAd has no syntax for is a NaN LITERAL --
// `nan` lexes as a bare reference and evaluates UNDEFINED -- and the literal is what matters,
// because only a stored literal reaches a column and an expression would be skipped by the very
// reader under test. So Rate's designated records write a placeholder here and Ad patches in a real
// NaN after the parse, while Calc carries the arithmetic form, which is how an ad in the wild
// actually acquires a NaN and which lands in the cold tail instead of the column.
func (s Shape) Source(i int) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	// --- strings -----------------------------------------------------------------------------
	// Owner: OwnerCardinality values in runs of RunLen, a string literal in every record. Low
	// enough cardinality that every block dictionary-encodes it, and complete enough that the
	// categorical index can account for every record -- the two things the dictionary reader and
	// the index tier need. The cardinality is not free to raise: dictWorthwhile needs
	// distinct*2 <= the block's record count, and the smallest shape here puts ~78 records in a
	// block, so 40 owners silently cost those shapes their dictionaries (the collections-side
	// assertion caught exactly that).
	w("Owner = %q", ownerOf(i, s.RunLen))
	// Host: distinct per record. distinct*2 > records in any block, so dictWorthwhile declines and
	// this field is read by the positional string walk instead.
	w("Host = %q", fmt.Sprintf("slot%d@host%d.example.net", i%13, i))
	// Site: the sharp group column. Three spellings render "undefined" and must land in ONE group;
	// the number 42 and the string "42" likewise. The moduli are coprime so the exceptions do not
	// line up with each other or with the predicates a query draws.
	//
	// Its ordinary values also come in two CASES (see ownerOf for why that is here and not only a
	// constraint literal), so the group column that carries every render-alike collision also
	// carries the pair that must NOT be merged.
	switch {
	case i%37 == 3: // absent entirely
	case i%37 == 11:
		w("Site = undefined") // the UNDEFINED keyword
	case i%41 == 5:
		w("Site = %q", "undefined") // ... and the literal text, which renders identically
	case i%43 == 7:
		w("Site = 42") // a number in a string column: escapes into the cold tail
	case i%47 == 9:
		w("Site = %q", "42") // ... beside the string that renders identically to it
	default:
		site := fmt.Sprintf("site-%02d", (i/3)%12)
		if i%5 == 2 {
			site = strings.ToUpper(site)
		}
		w("Site = %q", site)
	}
	// NumLike: a STRING column whose values are the decimal spellings of the integers ProcId holds,
	// with a minority stored as actual integers. Two storage kinds, one rendered group.
	if i%17 == 4 {
		w("NumLike = %d", i%10)
	} else {
		w("NumLike = %q", strconv.Itoa(i%10))
	}

	// --- integers, one per column width ------------------------------------------------------
	w("ClusterId = %d", 1000+i/8)
	w("ProcId = %d", i%8)       // 1 byte, unsigned
	w("Status = %d", 1+(i*7)%5) // 1 byte, unsigned
	w("Small = %d", (i*31)%60000)
	// Prio is the SIGNED column. Written here as text for the reproducer's sake, but re-inserted
	// as a literal by Ad: old-ClassAd parses `-20` as unary minus applied to the literal 20, which
	// is an expression, not a stored literal -- so a column of negative numbers written as text
	// falls below the presence floor and never becomes a column at all. (The fixture-steering
	// assertion in db is what caught that; without it half the width axis was silently missing.)
	w("Prio = %d", -20+(i*13)%41)
	if i%53 == 6 {
		// The ESCAPE case: ~2% of Mem does not fit the 2-byte slot its other values earn. Under
		// baseFit = 0.95 a 2% tail does not move a COLD column's width, so these records live in
		// the cold tail and every column reader has to fall back for them. It only works while Mem
		// is cold -- a hot int column is re-fitted to cover everything -- which is what HotTopN is
		// for, and what the collections-side assertion checks rather than assumes.
		w("Mem = %d", 100000+i)
	} else {
		w("Mem = %d", 512+(i*97)%60000)
	}
	if s.WideInts {
		w("Disk = %d", 5_000_000_000+int64(i)*7919)            // past 2^32: a 6-byte column
		w("Big = %d", int64(math.MaxInt64)-int64(i)*1_000_003) // 8 bytes; SUM overflows int64
	} else { // ... identically on both sides, so the comparison still holds
		w("Disk = %d", (i*7919)%100000) // 4 bytes
		w("Big = %d", (i*31)%200)       // 1 byte
	}
	// Ts advances monotonically, so a sealed segment's zone spans a narrow range -- which is what
	// lets a bucketed group attribute a whole segment and a range constraint prune one.
	w("Ts = %d", TsBase+i*TsStep)

	// --- reals -------------------------------------------------------------------------------
	switch {
	case i%101 == 17:
		w("Rate = 1.0e999") // a +Inf real LITERAL (old-ClassAd has no `inf` keyword either)
	case i%103 == 19:
		w("Rate = -1.0e999")
	case i%107 == 23:
		w("Rate = 0") // placeholder; Ad patches in a literal NaN
	case i%19 == 2:
		w("Rate = %d", -5+(i*3)%11) // an integer in a real column: SUM stays integral without it
	case i%7 == 1:
		w("Rate = %d.0", -3+(i*5)%7) // an INTEGRAL real: renders "3", exactly like the integers above
	default:
		// Mixed sign, so a float SUM can cancel and the tolerance has to be the real error bound
		// rather than a relative epsilon.
		w("Rate = %s", strconv.FormatFloat(float64(-500+(i*37)%1000)/8.0+float64(i%13)/3.0, 'g', -1, 64))
	}

	// Calc: a real column that acquires its NaN BY ARITHMETIC rather than as a stored literal,
	// which is the way a real ad gets one. 0.0/0.0 is an expression, so the record has no storable
	// kind and escapes into the cold tail; the column reader has to fall back for it and only the
	// evaluated value is NaN. That is a different code path from Rate's literal NaN, and the one
	// production data takes. Kept to ~0.5% so Calc's real share stays far above the 0.90 presence
	// floor and the attribute remains a columnar field at all.
	if i%199 == 31 {
		w("Calc = 0.0 / 0.0")
	} else {
		w("Calc = %d.25", -20+(i*11)%40)
	}

	// --- booleans, expressions, errors, and one sub-floor attribute --------------------------
	if i%19 != 3 {
		// A bool column: groupableKind declines it as a group column and AnyBool declines it as an
		// aggregate, so this is the shape that must reach the scan and agree there.
		w("Flag = %t", i%2 == 0)
	}
	if i%13 == 5 {
		w("Expr = ProcId * 2") // an expression where a literal usually is: must decline, not guess
	} else {
		w("Expr = %d", (i*3)%9)
	}
	if i%29 == 8 {
		w("Err = 1 / 0") // evaluates to ERROR, which is its own rendered group
	}
	if i%10 < 3 {
		w("Rare = %d", (i*17)%50) // 30% presence: below the 0.90 floor, so never a columnar field
	}
	return b.String()
}

// Value-domain constants. A query draws its literals from these so its constraints are winnable --
// comparing Ts against a random int64 matches everything or nothing and tests neither.
const (
	TsBase = 1_700_000_000
	TsStep = 37
	// OwnerCardinality is the number of distinct Owner values. See Source for why it is this low.
	OwnerCardinality = 16
)

// ownerOf is record i's Owner: OwnerCardinality lower-case values in runs of RunLen, with two of
// them ALSO spelled in upper case for part of their run.
//
// The case variants are the fixture's statement that group identity is the EXACT BYTES. The
// dictionaries this data is read through are fold-ORDERED and the comparison path works in fold
// ranges, so a group key derived from a folded form -- or from a fold range -- would merge "u07"
// with "U07" and report one group where the record scan gives two. Without a pair of records that
// differ ONLY in case, a sweep over this data cannot tell the two apart and such a key passes by
// luck. ("U07" was already a constraint literal, which exercises the dictionary PRUNE; that is a
// different question from identity.)
//
// Two owners, not sixteen: dictWorthwhile needs distinct*2 <= the block's record count, the
// smallest shape here puts ~78 records in a block, and a spelling per owner would push the distinct
// count past half of that and silently cost those shapes their dictionaries.
func ownerOf(i, runLen int) string {
	o := (i / runLen) % OwnerCardinality
	lower := fmt.Sprintf("u%02d", o)
	switch {
	case o == 7 && i%3 == 0, o == 15 && i%5 == 0:
		return strings.ToUpper(lower)
	}
	return lower
}

// hasNaN reports whether record i is one of the records whose Rate is patched to NaN.
func hasNaN(i int) bool { return i%107 == 23 }

// Ad builds record i as a ClassAd, including the values the text form cannot carry.
func (s Shape) Ad(i int) *classad.ClassAd {
	ad, err := classad.ParseOld(s.Source(i))
	if err != nil {
		panic(fmt.Sprintf("aggfuzz/gen: shape %d record %d does not parse: %v", s.ID, i, err))
	}
	if hasNaN(i) {
		ad.InsertAttrFloat("Rate", math.NaN())
	}
	return ad
}

// Ads builds every record of the shape, in append order.
func (s Shape) Ads() []*classad.ClassAd {
	out := make([]*classad.ClassAd, s.N)
	for i := range out {
		out[i] = s.Ad(i)
	}
	return out
}

// AdsAt builds just the records at the given indices, in the order given. The shrinker uses it to
// rebuild a candidate subset, and a reproducer names its records this way.
func (s Shape) AdsAt(idx []int) []*classad.ClassAd {
	out := make([]*classad.ClassAd, len(idx))
	for k, i := range idx {
		out[k] = s.Ad(i)
	}
	return out
}
