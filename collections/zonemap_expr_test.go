package collections

// A segment zone map once folded only numeric LITERALS into its [min,max] and skipped every other
// value, so a sealed segment holding a record whose zone attribute is an expression was pruned
// whenever the probe fell outside the literals' span -- even though the expression evaluates into
// it. `CompletionDate > 500` over records at 100 plus one `CompletionDate = 1000 + 1` returned 0
// rows with the zone and 1 without. A negative constant, stored as unary minus over a literal,
// was skipped the same way. These tests check every reader of the zone map against evaluating
// each record.

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// exprZonePad keeps records large enough that a 4 KiB segment holds ~15 of them.
var exprZonePad = strings.Repeat("x", 200)

// fillExprZoned appends n records [Id; Owner; CompletionDate; Pad] with CompletionDate 100,
// except where cd names another value source (an expression, a negative constant, or "" for
// absent).
func fillExprZoned(t *testing.T, a *Archive, n int, cd map[int]string) {
	t.Helper()
	for i := range n {
		v, ok := cd[i]
		if !ok {
			v = "100"
		}
		field := ""
		if v != "" {
			field = "CompletionDate = " + v + ";"
		}
		ad, err := classad.Parse(fmt.Sprintf(`[Id = %d; Owner = "u%d"; %s Pad = %q]`, i, i%3, field, exprZonePad))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

func openExprZoned(t *testing.T, dir string, zoneAttrs, valueAttrs, catAttrs []string) (*Archive, OpenTiming) {
	t.Helper()
	var got OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { got = d }
	defer func() { OpenIndexDiagHook = prev }()
	a, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 4096, ZoneAttrs: zoneAttrs,
		ValueAttrs: valueAttrs, CategoricalAttrs: catAttrs})
	if err != nil {
		t.Fatal(err)
	}
	return a, got.Timing
}

// sealedZoneStates reports, over the sealed segments, how many have a zone entry for attr, how many
// lack one, and the smallest Min among the entries.
func sealedZoneStates(a *Archive, attr string) (with, without int, lo float64) {
	id, _ := a.c.intern.LookupID(attr)
	lo = math.Inf(1)
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s == nil || s == sh.act {
				continue
			}
			if z, ok := s.zones[id]; ok {
				with++
				lo = math.Min(lo, z.Min)
			} else {
				without++
			}
		}
		sh.mu.RUnlock()
	}
	return with, without, lo
}

var exprZoneConstraints = []string{
	"CompletionDate > 500",
	"CompletionDate >= 1001",
	"CompletionDate == 1001",
	"CompletionDate == 1001 || CompletionDate == 7",
	"!(CompletionDate <= 500)",
	"CompletionDate > 100 && Id >= 0",
	"CompletionDate == 2020",
	"CompletionDate < 0",
	"CompletionDate <= -5",
	"CompletionDate == -5",
	"CompletionDate =?= -5",
	"CompletionDate < 100",
}

// checkExprZoneCounts compares every constraint's row count with evaluating it on every record, and
// with the same predicate written so that nothing can prune it.
func checkExprZoneCounts(t *testing.T, a *Archive, stage string) {
	t.Helper()
	for _, c := range exprZoneConstraints {
		want := zoneTruth(t, a, c)
		if got := zoneCount(t, a, c); got != want {
			t.Errorf("%s: %s: %d rows, want %d", stage, c, got, want)
		}
		if want == 0 {
			t.Errorf("%s: %s matches nothing; the fixture does not test it", stage, c)
		}
		unpruned := strings.Replace(c, "CompletionDate", "(CompletionDate + 0)", 1)
		if got := zoneCount(t, a, unpruned); got != want {
			t.Errorf("%s: %s: %d rows, want %d", stage, unpruned, got, want)
		}
	}
}

// exprZoneFixture: Id 0 holds an expression evaluating to 1001, Id 20 an expression referring to
// another attribute (2020), Id 40 a negative constant. Each sits in its own sealed segment, among
// records at 100.
var exprZoneFixture = map[int]string{0: "1000 + 1", 20: "Id + 2000", 40: "-5"}

func TestZoneMapExpressionValuedRecords(t *testing.T) {
	for _, tc := range []struct {
		name          string
		zones, values []string
	}{
		{name: "zone attr", zones: []string{"CompletionDate"}},
		{name: "value index", values: []string{"CompletionDate"}}, // zoned automatically
		{name: "no zones"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a, _ := openExprZoned(t, dir, tc.zones, tc.values, nil)
			fillExprZoned(t, a, 80, exprZoneFixture)
			zoned := len(tc.zones)+len(tc.values) > 0
			if zoned {
				// The expression-valued records must be in SEALED segments: an active segment is
				// never pruned, so a record there would pass whether or not the zone is right.
				with, without, lo := sealedZoneStates(a, "CompletionDate")
				if without < 2 || with == 0 {
					t.Fatalf("sealed segments with a CompletionDate zone: %d, without: %d; want the two "+
						"expression-valued records in two sealed segments among zoned ones", with, without)
				}
				if lo != -5 {
					t.Fatalf("smallest sealed CompletionDate zone is %v, want -5 (the negative constant)", lo)
				}
			}
			checkExprZoneCounts(t, a, "written")
			a.Close()

			for _, stage := range []string{"reopened", "reopened again"} {
				a, tm := openExprZoned(t, dir, tc.zones, tc.values, nil)
				if zoned && tm.ZoneRecomputed != 0 {
					t.Errorf("%s: recomputed %d zone maps; the persisted ones should be adopted", stage, tm.ZoneRecomputed)
				}
				if zoned {
					if _, without, lo := sealedZoneStates(a, "CompletionDate"); without < 2 || lo != -5 {
						t.Errorf("%s: adopted zones lost the expression or the negative constant (without=%d, lo=%v)",
							stage, without, lo)
					}
				}
				checkExprZoneCounts(t, a, stage)
				a.Close()
			}
		})
	}
}

// zoneValue's classification, against what a numeric comparison makes of each kind of value.
func TestZoneValueClassification(t *testing.T) {
	for _, tc := range []struct {
		val  string
		f    float64
		cell zoneCell
	}{
		{"7", 7, zoneNumber},
		{"2.5", 2.5, zoneNumber},
		{"true", 1, zoneNumber},
		{"false", 0, zoneNumber},
		{"-5", -5, zoneNumber},
		{"-2.5", -2.5, zoneNumber},
		{"+5", 5, zoneNumber},
		{"(5)", 5, zoneNumber},
		{"-(-(5))", 5, zoneNumber},
		{"-9223372036854775808", math.MinInt64, zoneNumber},
		{`"600"`, 0, zoneNever},
		{"undefined", 0, zoneNever},
		{"error", 0, zoneNever},
		{"1000 + 1", 0, zoneOpaque},
		{"Id + 2000", 0, zoneOpaque},
		{"-true", 0, zoneOpaque},
		{"!5", 0, zoneOpaque},
		{"~5", 0, zoneOpaque},
		{`real("NaN")`, 0, zoneOpaque},
		{"{1, 2}", 0, zoneOpaque},
		{"[a = 1]", 0, zoneOpaque},
		{"x ?: 3", 0, zoneOpaque},
	} {
		ad, err := classad.Parse("[x = " + tc.val + "]")
		if err != nil {
			t.Fatalf("%s: %v", tc.val, err)
		}
		node, ok := encodeInlineForTest(ad).LookupByName("x")
		if !ok {
			t.Fatalf("%s: x not found", tc.val)
		}
		f, cell := zoneValue(node)
		if cell != tc.cell || (cell == zoneNumber && f != tc.f) {
			t.Errorf("%s: got (%v, %d), want (%v, %d)", tc.val, f, cell, tc.f, tc.cell)
		}
		// A value the zone leaves out must not be able to satisfy a numeric comparison.
		if cell == zoneNever {
			for _, q := range []string{"x > -1e300", "x < 1e300", "x == 600", "x >= 0"} {
				if b, err := ad.EvaluateExprWithTarget(mustParseExpr(t, q), nil).BoolValue(); err == nil && b {
					t.Errorf("%s: %s is true, but the zone leaves the value out", tc.val, q)
				}
			}
		}
	}
	nan := classad.New()
	nan.InsertAttrFloat("x", math.NaN())
	node, _ := encodeInlineForTest(nan).LookupByName("x")
	if _, cell := zoneValue(node); cell != zoneNever {
		t.Errorf("NaN literal: cell %d, want never (no comparison with NaN is true)", cell)
	}
}

func encodeInlineForTest(ad *classad.ClassAd) wire.Ad {
	return wire.Ad(wire.EncodeInline(nil, ad.AST()))
}

func mustParseExpr(t *testing.T, s string) *classad.Expr {
	t.Helper()
	e, err := classad.ParseExpr(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// zoneSidecars lists every sidecar under dir.
func zoneSidecars(t *testing.T, dir string) []string {
	t.Helper()
	var idxs []string
	if err := filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".idx") {
			idxs = append(idxs, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return idxs
}

// znm2Blob encodes zones in the previous name-keyed format, which had no flags.
func znm2Blob(zones map[string]zoneRange) []byte {
	b := binary.LittleEndian.AppendUint32(nil, zoneBlobMagicV2)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(zones)))
	for name, z := range zones {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(name)))
		b = append(b, name...)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(z.Min))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(z.Max))
	}
	return b
}

// An archive written by a build whose zones skipped expressions carries ZNM2 sections that exclude
// those records. They are not trusted: the first open recomputes and rewrites them, the answers are
// right from that open on, and the next open adopts the rewritten sections.
func TestZNM2ZoneSectionsRecomputedOnce(t *testing.T) {
	dir := t.TempDir()
	zones := []string{"CompletionDate"}
	a, _ := openExprZoned(t, dir, zones, nil, nil)
	fillExprZoned(t, a, 80, exprZoneFixture)
	a.Close()

	// Rewrite each sealed sidecar's zone section the way the previous build wrote it: literals only,
	// so every segment's CompletionDate range is [100,100] -- the expressions and the unary-minus
	// constant were skipped.
	legacy := 0
	for _, p := range zoneSidecars(t, dir) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		attr, key, col, zone, used, dictRec, ok := splitSegmentSidecarV4(data)
		if !ok || len(zone) == 0 {
			continue
		}
		out := buildSegmentSidecar(append([]byte(nil), attr...), append([]byte(nil), key...),
			append([]byte(nil), col...), znm2Blob(map[string]zoneRange{"CompletionDate": {Min: 100, Max: 100}}), used, dictRec, nil)
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
		legacy++
	}
	if legacy < 3 {
		t.Fatalf("rewrote %d zone sections; the fixture should seal at least 3 segments", legacy)
	}

	a, t1 := openExprZoned(t, dir, zones, nil, nil)
	if t1.ZoneRecomputed != legacy || t1.ZoneUpgraded != legacy {
		t.Errorf("first open: recomputed %d, upgraded %d of %d ZNM2 sections", t1.ZoneRecomputed, t1.ZoneUpgraded, legacy)
	}
	checkExprZoneCounts(t, a, "ZNM2 sections")
	a.Close()

	a, t2 := openExprZoned(t, dir, zones, nil, nil)
	defer a.Close()
	if t2.ZoneRecomputed != 0 {
		t.Errorf("second open recomputed %d segments: the upgrade did not stick", t2.ZoneRecomputed)
	}
	checkExprZoneCounts(t, a, "upgraded")
}

func TestZoneBlobFormat(t *testing.T) {
	ids := map[string]uint32{"A": 1, "B": 2}
	nameOf := func(id uint32) (string, bool) {
		for n, i := range ids {
			if i == id {
				return n, true
			}
		}
		return "", false
	}
	idOf := func(n string) (uint32, bool) { id, ok := ids[n]; return id, ok }
	in := map[uint32]zoneRange{1: {Min: -5, Max: 7, Full: true}, 2: {Min: 3, Max: 3}}
	blob := buildZoneBlob(in, nameOf)
	got, st := parseZoneBlob(blob, idOf)
	if st != zoneBlobOK || len(got) != 2 || got[1] != in[1] || got[2] != in[2] {
		t.Fatalf("round trip: %v %v, want %v", got, st, in)
	}
	// The build before this format reads a section by its ZNM2 marker, falling back to the
	// id-keyed layout; it must find neither here, so it recomputes rather than misreads.
	if binary.LittleEndian.Uint32(blob) == zoneBlobMagicV2 || legacyZoneBlobValid(blob) {
		t.Error("an older build would accept the new section")
	}
	// A section with no entries is a zone map (adopted, not recomputed); a nil map writes nothing.
	if z, st := parseZoneBlob(buildZoneBlob(map[uint32]zoneRange{}, nameOf), idOf); st != zoneBlobOK || z == nil || len(z) != 0 {
		t.Errorf("empty zone map: %v %v", z, st)
	}
	if b := buildZoneBlob(nil, nameOf); b != nil {
		t.Errorf("nil zone map encoded as %d bytes", len(b))
	}
	if _, st := parseZoneBlob(znm2Blob(map[string]zoneRange{"A": {Min: 1, Max: 2}}), idOf); st != zoneBlobLegacy {
		t.Errorf("ZNM2 section: status %v, want legacy", st)
	}
	// A flag this build does not know means a format it does not know.
	bad := append([]byte(nil), blob...)
	bad[8+2+1] |= 0x80 // first entry ("A"): u16 length, 1-byte name, then flags
	if _, st := parseZoneBlob(bad, idOf); st != zoneBlobBad {
		t.Errorf("unknown flag bit: status %v, want bad", st)
	}
}

// Age retention must keep a segment whose age attribute some record holds as an expression: only
// evaluating it says how old that record is. Rotation drops oldest first and stops at the first
// segment it keeps, so the expression sits in the third segment and the two before it must go.
func TestRetentionKeepsExpressionValuedAge(t *testing.T) {
	for _, tc := range []struct {
		name string
		ret  Retention
		gc   float64
	}{
		{name: "max age", ret: Retention{MaxAgeAttr: "CompletionDate", MaxAge: 1000}},
		{name: "gc floor", ret: Retention{MinAgeAttr: "CompletionDate"}, gc: 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a, _ := openExprZoned(t, dir, []string{"CompletionDate"}, nil, nil)
			defer a.Close()
			fillExprZoned(t, a, 80, map[int]string{40: "5000 + 0"})
			if _, without, _ := sealedZoneStates(a, "CompletionDate"); without != 1 {
				t.Fatalf("%d sealed segments without a CompletionDate zone, want the expression's one", without)
			}
			const young = "CompletionDate >= 4000"
			before, total := zoneTruth(t, a, young), a.Count()
			if before != 1 {
				t.Fatalf("%s: %d records before rotation, want 1", young, before)
			}
			a.SetRetention(tc.ret)
			if tc.gc > 0 {
				a.c.SetGCFloor(tc.gc)
			}
			if _, err := a.Rotate(5000); err != nil {
				t.Fatal(err)
			}
			if after := zoneTruth(t, a, young); after != before {
				t.Errorf("retention dropped a young record: %d of %d left", after, before)
			}
			if left := a.Count(); left >= total {
				t.Errorf("retention dropped nothing (%d of %d left); the older segments should go", left, total)
			}
		})
	}
}

// groupTruth is the per-Owner count of records satisfying constraint, by evaluating each record.
func groupTruth(t *testing.T, a *Archive, constraint string) map[string]int64 {
	t.Helper()
	expr := mustParseExpr(t, constraint)
	all, _ := vm.Parse("true")
	out := map[string]int64{}
	for ad := range a.Query(all) {
		if b, err := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); err == nil && b {
			o, _ := ad.EvaluateAttrString("Owner")
			out[o]++
		}
	}
	return out
}

func sameCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// bucketTruth is the per-day, per-Owner count of records with a numeric CompletionDate.
func bucketTruth(t *testing.T, a *Archive) map[int64]map[string]int64 {
	t.Helper()
	all, _ := vm.Parse("true")
	out := map[int64]map[string]int64{}
	for ad := range a.Query(all) {
		v := ad.EvaluateAttr("CompletionDate")
		f, err := v.NumberValue()
		if err != nil {
			continue
		}
		o, _ := ad.EvaluateAttrString("Owner")
		b := bucketOf(f, 86400)
		if out[b] == nil {
			out[b] = map[string]int64{}
		}
		out[b][o]++
	}
	return out
}

// Zone-decided GROUP BY counts: a segment may be counted wholesale only when every record in it
// satisfies the constraint, and excluded only when none can.
func TestGroupCountsWithExpressionOrAbsentZoneAttr(t *testing.T) {
	for _, tc := range []struct {
		name string
		cd   map[int]string
		base string
		// mustAnswer: the fast path has to give an answer here (it can read every record).
		mustAnswer bool
	}{
		// Every literal is 100, so before the fix the expression's segment was "never" for
		// `> 500` and its record silently left out; and its zone sat in one day bucket, so the
		// record was counted in day 0 rather than day 86400.
		{name: "expression", cd: map[int]string{0: "100000 + 1"}, base: "100"},
		// Every literal is 1000, so the segment was "always" for `> 500` and counted wholesale --
		// including the record that has no CompletionDate and so does not match.
		{name: "absent", cd: map[int]string{0: ""}, base: "1000", mustAnswer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a, _ := openExprZoned(t, dir, []string{"CompletionDate"}, nil, []string{"Owner"})
			defer a.Close()
			cd := map[int]string{}
			for i := range 80 {
				cd[i] = tc.base
			}
			for k, v := range tc.cd {
				cd[k] = v
			}
			fillExprZoned(t, a, 80, cd)
			a.Reindex()
			if with, _, _ := sealedZoneStates(a, "CompletionDate"); with < 2 {
				t.Fatalf("%d sealed segments with a CompletionDate zone; the fixture needs several", with)
			}
			for _, c := range []string{"CompletionDate > 500", "CompletionDate >= 100 && CompletionDate < 200000"} {
				want := groupTruth(t, a, c)
				got, ok := a.CategoricalGroupCountsWhere("Owner", c)
				if tc.mustAnswer && !ok {
					t.Errorf("%s: declined", c)
				}
				if ok && !sameCounts(got, want) {
					t.Errorf("%s: %v, want %v", c, got, want)
				}
			}
			got, ok := a.CategoricalGroupCountsBucketed("Owner", "CompletionDate", 86400)
			switch {
			case ok && tc.mustAnswer:
				// A record with no CompletionDate is in no bucket, so the counts cannot add up to
				// the archive: the only right answer is to decline.
				t.Errorf("bucketed answered %v over a record with no bucket value", got)
			case ok:
				if want := bucketTruth(t, a); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("bucketed: %v, want %v", got, want)
				}
			}
		})
	}
}
