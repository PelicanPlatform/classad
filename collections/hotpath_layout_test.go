package collections

import (
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"testing"
	"unsafe"
)

// TestBlockZoneEntrySize pins the per-block zone entry at 32 bytes. There is one per numeric field per
// columnar block, so a field added here grows a store's zone heap by the same ratio (embedding
// zoneRange, with its Full flag, made it 40 and the zone heap 40% larger). Change the number only on
// purpose.
func TestBlockZoneEntrySize(t *testing.T) {
	if got := unsafe.Sizeof(blockZoneEntry{}); got != 32 {
		t.Fatalf("unsafe.Sizeof(blockZoneEntry{}) = %d, want 32", got)
	}
}

// TestGroupKeyIsPlainMemory pins every packed key type to fixed-size scalar fields, and pins its
// WIDTH.
//
// A group column looks its group up once per record, and the runtime hashes and compares a key like
// this as a block of memory; a string or pointer field makes it hash field by field instead, which
// made a numeric GROUP BY 55% slower when the key carried a string. That is why a string COMPONENT
// is a query-local integer id and not the string.
//
// The widths are the point of there being three types. One key per arity exists so that a
// one-column GROUP BY hashes and compares 16 bytes rather than the widest key any query might need:
// narrowing a one-column group from the 24-byte two-slot key to this one was worth 6-16% on the raw
// arms of BenchmarkGroupAccum. A type that grew here would charge its growth to every record of
// every query at that arity, so change these only on purpose.
func TestGroupKeyIsPlainMemory(t *testing.T) {
	for _, k := range []struct {
		name string
		typ  reflect.Type
		size uintptr
		n    int
	}{
		{"groupKey1", reflect.TypeFor[groupKey1](), 16, 1}, // 1 int64 + 1 kind byte, padded
		{"groupKey2", reflect.TypeFor[groupKey2](), 24, 2},
		{"groupKey3", reflect.TypeFor[groupKey3](), 32, 3},
	} {
		for i := 0; i < k.typ.NumField(); i++ {
			f := k.typ.Field(i)
			et := f.Type
			if et.Kind() == reflect.Array {
				et = et.Elem()
			}
			switch et.Kind() {
			case reflect.Int64, reflect.Uint8:
			default:
				t.Fatalf("%s.%s is a %s; keep the key to fixed-size integers", k.name, f.Name, f.Type)
			}
		}
		if got := k.typ.Size(); got != k.size {
			t.Fatalf("unsafe.Sizeof(%s{}) = %d, want %d", k.name, got, k.size)
		}
	}
	// There is a key type for every arity the cap admits, AND the dispatch reaches it. Without
	// this, raising maxGroupCols without instantiating the key at that width would leave the top
	// arity dispatching to nothing -- which declines rather than truncating, so it is safe, but it
	// is also silently not the acceleration the cap now claims to offer.
	//
	// The accumulator's own arity is not enough to show that. newGroupAccum sets it from the column
	// count before it picks a map, so an arity with no map still reports the arity it was asked
	// for; what distinguishes "served" from "declined" is whether accFor forms a key. Both the raw
	// and the bucketed dispatch are asked, because they are separate switches and a case missing
	// from either one is a width that silently falls back.
	for n := 1; n <= maxGroupCols; n++ {
		for _, bucketed := range []bool{false, true} {
			buckets := make([]int64, n)
			if bucketed {
				buckets[n-1] = 3600
			}
			a := newGroupAccum(buckets)
			if a.n != n {
				t.Fatalf("%d group columns built an accumulator of arity %d: maxGroupCols admits "+
					"an arity with no key type", n, a.n)
			}
			var comps [maxGroupCols]groupComp
			for i := range comps[:n] {
				comps[i] = groupComp{kind: gkInt, raw: int64(i) + 1}
			}
			g, ok := a.accFor(&comps, 0)
			if !ok || g == nil {
				t.Fatalf("%d group columns (bucketed=%v): accFor = %v, %v; maxGroupCols admits an "+
					"arity the dispatch does not serve, so the cap claims an acceleration that "+
					"falls back to the record scan", n, bucketed, g, ok)
			}
			if a.nGroups() != 1 {
				t.Fatalf("%d group columns (bucketed=%v): one record formed %d groups; the arity "+
					"has a key type but nGroups does not read its map", n, bucketed, a.nGroups())
			}
		}
	}
	// And the component the key is assembled from, which is passed by value per record per column.
	if got := unsafe.Sizeof(groupComp{}); got != 16 {
		t.Fatalf("unsafe.Sizeof(groupComp{}) = %d, want 16", got)
	}
}

// groupAccumStates names the per-arity state fields of groupAccum, in the order they must appear,
// beside the arity each one serves and the per-component tables it has to carry.
//
// It is a literal list rather than anything derived from the types, because the two tests below are
// checking the types AGAINST it: a check that read the arity off the field it was checking could
// only ever agree with itself. A fourth arity means a fourth row here, after g3 -- and the offsets
// pinned in TestGroupAccumHotFieldOffsets then say whether adding it moved anything.
var groupAccumStates = []struct {
	field  string   // the field of groupAccum
	arity  int      // how many group columns it serves
	tables []string // the per-component tables it must carry, each an array of exactly `arity`
}{
	// Arity 1 has no component index lists: accFor serves one group column straight-line and never
	// walks one.
	{"g1", 1, []string{"mIv", "bucket"}},
	{"g2", 2, []string{"mIv", "bucket", "rawIdx", "bktIdx"}},
	{"g3", 3, []string{"mIv", "bucket", "rawIdx", "bktIdx"}},
}

// TestGroupAccumHasNoCapSizedState fails if per-component state sized by maxGroupCols reappears in
// groupAccum, or in an arity that is not the top one.
//
// This is the property the per-arity split exists for, and it is not about correctness: an
// accumulator with the interval table and the bucket widths held as cap-sized arrays computes
// exactly the same groups. What it does is put three components' worth of tables among the fields a
// ONE-column query reads once per record, so raising the cap moves a query that executes nothing
// different -- which is what capped the number of group columns at the width every query could
// afford. Sized by the arity instead, a one-column query carries one component's worth.
//
// Two rules, and both have to hold or the property is only true by accident:
//
//   - Outside the TOP arity's state, nothing in groupAccum's field tree may be an array of
//     maxGroupCols elements. That is the exact shape of the thing removed, and it catches it coming
//     back anywhere in the tree rather than only as a direct field.
//   - Each arity's state must carry every table named above as an array of EXACTLY that arity.
//     Without this half the first rule is vacuous -- deleting the tables, renaming them, or sizing
//     them by something else that happens not to equal the cap would all pass it.
func TestGroupAccumHasNoCapSizedState(t *testing.T) {
	if len(groupAccumStates) != maxGroupCols {
		t.Fatalf("groupAccumStates lists %d arities, maxGroupCols is %d: an arity the cap admits "+
			"has no state of its own, or one that is listed has no key type",
			len(groupAccumStates), maxGroupCols)
	}
	at := reflect.TypeFor[groupAccum]()
	byName := map[string]reflect.StructField{}
	for i := 0; i < at.NumField(); i++ {
		byName[at.Field(i).Name] = at.Field(i)
	}
	prev := -1
	for _, st := range groupAccumStates {
		f, ok := byName[st.field]
		if !ok {
			t.Fatalf("groupAccum has no field %q: the per-arity state for %d group columns is "+
				"gone or renamed", st.field, st.arity)
		}
		if f.Index[0] <= prev {
			t.Fatalf("groupAccum.%s is declared before the state of a lower arity; the states go "+
				"in ascending arity so that a new one lands behind the others and moves nothing "+
				"ahead of it", st.field)
		}
		prev = f.Index[0]
		for _, name := range st.tables {
			tf, ok := f.Type.FieldByName(name)
			if !ok {
				t.Fatalf("%s (arity %d) has no %s: the per-component tables live in the arity's "+
					"own state, sized by it", st.field, st.arity, name)
			}
			if tf.Type.Kind() != reflect.Array || tf.Type.Len() != st.arity {
				t.Fatalf("%s.%s is %s; at arity %d it must be an array of exactly %d, never one "+
					"sized by maxGroupCols", st.field, name, tf.Type, st.arity, st.arity)
			}
		}
	}
	// And nothing anywhere else in the accumulator is cap-sized. The top arity's state is exempt
	// because its arrays are legitimately maxGroupCols long -- that is its arity.
	top := groupAccumStates[len(groupAccumStates)-1].field
	for i := 0; i < at.NumField(); i++ {
		f := at.Field(i)
		if f.Name == top {
			continue
		}
		if where := capSizedArray(f.Type, "groupAccum."+f.Name); where != "" {
			t.Fatalf("%s is an array of maxGroupCols (%d): per-component state belongs in the "+
				"arity's own state, sized by that arity, so that a query of a lower arity does not "+
				"read past the components it has and a higher cap cannot reach it at all",
				where, maxGroupCols)
		}
	}
}

// capSizedArray returns the path to the first array of maxGroupCols elements in t, or "".
func capSizedArray(t reflect.Type, path string) string {
	switch t.Kind() {
	case reflect.Array:
		if t.Len() == maxGroupCols {
			return path
		}
		return capSizedArray(t.Elem(), path+"[]")
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if where := capSizedArray(f.Type, path+"."+f.Name); where != "" {
				return where
			}
		}
	}
	// Not through a pointer, a slice or a map: those are one word here whatever they address, and
	// what this is looking for is state held INLINE among the fields the per-record paths read.
	return ""
}

// TestGroupAccumHotFieldOffsets pins where the fields the per-record paths read actually sit.
//
// The numbers are the point. A memo HIT on a single bucketed group column -- the shape a time series
// asks, and the one the interval memo exists for -- reads the component count, the bucketed-column
// count, the memo's accumulator and ONE interval, and those four are one cache line. They were one
// line before the per-arity split too. What the split changed is that they no longer stay one line
// BY ARITHMETIC THAT INCLUDES THE CAP: a fourth component's worth of intervals ahead of them used to
// push the whole tail along, and now there is no cap-sized table in front of anything.
//
// So this fails two ways. The cache-line checks below say the single-column hit still reads one line
// and the two-column one two adjacent lines, which are the properties and survive the fields being
// reordered within them. The literal offsets say nothing moved at all -- including the arity-2
// state, which no cache-line claim pins to a particular place and which a fourth arity inserted in
// the wrong position would shift.
//
// EVERY NUMBER HERE MUST SURVIVE A HIGHER CAP. A groupState4 added after g3 moves none of them; one
// added anywhere else, or a cap-sized array put back among these fields, moves most of them. That is
// the whole content of the claim that the cap is free to the arities already here, and it is checked
// rather than asserted.
func TestGroupAccumHotFieldOffsets(t *testing.T) {
	var a groupAccum
	const line = 64
	for _, f := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"n", unsafe.Offsetof(a.n), 0},
		{"bktN", unsafe.Offsetof(a.bktN), 8},
		{"rawN", unsafe.Offsetof(a.rawN), 16},
		{"mAcc", unsafe.Offsetof(a.mAcc), 32},
		{"g1", unsafe.Offsetof(a.g1), 40},
		{"g1.mIv", unsafe.Offsetof(a.g1) + unsafe.Offsetof(a.g1.mIv), 40},
		{"g1.bucket", unsafe.Offsetof(a.g1) + unsafe.Offsetof(a.g1.bucket), 56},
		{"g1.acc", unsafe.Offsetof(a.g1) + unsafe.Offsetof(a.g1.acc), 64},
		{"g1.key", unsafe.Offsetof(a.g1) + unsafe.Offsetof(a.g1.key), 72},
		{"g2", unsafe.Offsetof(a.g2), 88},
		{"g2.mIv", unsafe.Offsetof(a.g2) + unsafe.Offsetof(a.g2.mIv), 88},
		{"g2.rawIdx", unsafe.Offsetof(a.g2) + unsafe.Offsetof(a.g2.rawIdx), 120},
		{"g2.mKey", unsafe.Offsetof(a.g2) + unsafe.Offsetof(a.g2.mKey), 128},
		{"g3", unsafe.Offsetof(a.g3), 200},
	} {
		if f.got != f.want {
			t.Fatalf("groupAccum.%s is at byte %d, want %d: the per-record paths were laid out "+
				"around these offsets, and a higher cap must not be able to change them",
				f.name, f.got, f.want)
		}
	}
	// The single-column bucketed memo hit, in full: n, bktN, mAcc and interval 0. One line.
	hit := unsafe.Offsetof(a.g1) + unsafe.Offsetof(a.g1.mIv) + unsafe.Sizeof(a.g1.mIv[0])
	if hit > line {
		t.Fatalf("a single-column bucketed memo hit reads out to byte %d, past the %d-byte line "+
			"it is meant to fit in: n, bktN, mAcc and one interval are the whole of that path",
			hit, line)
	}
	// The TWO-column bucketed memo hit, which is "per owner per day" and the shape a dashboard
	// draws: the header and mAcc, then the intervals, the component lists and the probe. Those
	// three are read together and must not spread past two adjacent lines. Declared the way the
	// accumulator's old cap-sized arrays were -- intervals at the front of the state, probe behind
	// the map -- they spread across three, which is one more line than this path reached before the
	// per-arity split.
	g2 := unsafe.Offsetof(a.g2)
	first := (g2 + unsafe.Offsetof(a.g2.mIv)) / line
	last := (g2 + unsafe.Offsetof(a.g2.mKey) + unsafe.Sizeof(a.g2.mKey) - 1) / line
	if last-first > 1 {
		t.Fatalf("a two-column bucketed memo hit reads lines %d..%d of the accumulator; the "+
			"intervals, the component lists and the probe are meant to be two adjacent lines",
			first, last)
	}
}

// TestGroupMemoPathsInline fails if the bucket interval memo's containment test stops being
// inlined into the per-record paths that call it.
//
// It is worth a test of its own because the cost is invisible in the source and large in the
// measurement. The function is a handful of instructions; out of line it is a call frame per record
// on a path whose entire purpose is to replace a float divide with two compares, and the two-column
// bucketed arm -- "per owner per day", about 99.6% memo hits -- measured 19% slower with not one
// instruction of it changed. Nothing else in the accumulator moved; the body had crossed the
// inliner's cost budget by a single node.
//
// It is also the kind of thing the per-arity split invites. One function per arity means one body
// per arity to keep under the budget, and the obvious way to give each of them the live arity's
// interval table -- a method on *groupAccum opening with `m := &a.g2` -- is exactly the local that
// crossed it.
func TestGroupMemoPathsInline(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the package")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(goBin, "build", "-gcflags=-m=2", "-o", os.DevNull, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build -gcflags=-m=2: %v\n%s", err, out)
	}
	// Two calls each: the all-bucketed branch and the one that compares the probe first.
	for _, fn := range []struct {
		name  string
		pat   string
		calls int
	}{
		{"(*groupState2).inMemoIntervals", `\(\*groupState2\)\.inMemoIntervals`, 2},
		{"(*groupState3).inMemoIntervals", `\(\*groupState3\)\.inMemoIntervals`, 2},
	} {
		// "can inline" is one step upstream of the thing that matters: a function can be under the
		// budget and still be left out of line at a call site whose own body has grown too big to
		// take it. The count of "inlining call to" lines is the property itself, so that is what
		// this counts -- and counting them, rather than looking for one, also fails if one of the
		// two branches stops taking it while the other still does.
		got := len(regexp.MustCompile(`(?m): inlining call to `+fn.pat+`\b`).FindAll(out, -1))
		if got != fn.calls {
			why := regexp.MustCompile(`(?m)^.*cannot inline ` + fn.pat + `.*$`).Find(out)
			t.Fatalf("%s is inlined at %d call sites, want %d; out of line it is a call frame per "+
				"record on the memo's hit path. %s", fn.name, got, fn.calls, why)
		}
	}
}

// TestDamageHoldsMatchesCovers checks holds, the inlinable form segWindow.visible uses, against covers
// on random span sets, at every 8-byte offset in and around them.
func TestDamageHoldsMatchesCovers(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 200; iter++ {
		var spans []damageSpan
		pos := uint32(rng.Intn(4)) * 8
		for n := 1 + rng.Intn(6); n > 0; n-- {
			start := pos + uint32(rng.Intn(4))*8
			end := start + 8 + uint32(rng.Intn(4))*8
			spans = append(spans, damageSpan{start, end})
			pos = end
		}
		d := newDamageSet(spans)
		for o := uint32(0); o <= d.end+16; o += 8 {
			if got, want := d.holds(o), d.covers(o); got != want {
				t.Fatalf("spans %v: holds(%d) = %v, covers = %v", spans, o, got, want)
			}
		}
		enc := encodeDamage(d)
		if dec, ok := decodeDamage(enc); !ok || dec.end != d.end {
			t.Fatalf("spans %v: decoded set end = %v (ok %v), want %d", spans, dec, ok, d.end)
		}
	}
}

// TestSegWindowVisibleInlinable fails if segWindow.visible stops being inlinable. It runs once per row
// of every columnar count and aggregate; out of line, the call and the copy of the window cost those
// scans 10-37%.
func TestSegWindowVisibleInlinable(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the package")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(goBin, "build", "-gcflags=-m=2", "-o", os.DevNull, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build -gcflags=-m=2: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m): can inline segWindow\.visible\b`).Match(out) {
		t.Fatalf("segWindow.visible is no longer inlinable: %s",
			regexp.MustCompile(`(?m)^.*inline segWindow\.visible.*$`).Find(out))
	}
}
