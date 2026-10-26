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
	// There is a key type for every arity the cap admits. Without this, raising maxGroupCols
	// without adding a type would leave the top arity dispatching to nothing -- which declines
	// rather than truncating, so it is safe, but it is also silently not the acceleration the cap
	// now claims to offer.
	for n := 1; n <= maxGroupCols; n++ {
		a := newGroupAccum(make([]int64, n))
		if a.n != n {
			t.Fatalf("%d group columns built an accumulator of arity %d: maxGroupCols admits an "+
				"arity with no key type", n, a.n)
		}
	}
	// And the component the key is assembled from, which is passed by value per record per column.
	if got := unsafe.Sizeof(groupComp{}); got != 16 {
		t.Fatalf("unsafe.Sizeof(groupComp{}) = %d, want 16", got)
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
