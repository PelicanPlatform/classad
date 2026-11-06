package classad

import (
	"math"
	"reflect"
	"runtime"
	"testing"
	"unsafe"
)

// gcAndChurn runs a collection and then allocates enough garbage to make a freed
// object's memory likely to be reused, so a Value whose payload pointer the
// collector failed to see would come back corrupted rather than merely stale.
func gcAndChurn() {
	runtime.GC()
	sink := make([][]byte, 0, 64)
	for range 64 {
		b := make([]byte, 4096)
		for i := range b {
			b[i] = 0xAA
		}
		sink = append(sink, b)
	}
	runtime.KeepAlive(sink)
	runtime.GC()
}

// TestValueSize pins the size of Value. Value is copied on every bytecode stack
// push/pop and every operator call, and it is also the element type of the VM's
// evaluation stack and of every materialized list, so its size is a direct
// multiplier on copy traffic and on allocation volume. It went 48 -> 40 by
// merging the mutually exclusive *ClassAd and *listData fields into one pointer,
// then 40 -> 32 by aliasing that pointer onto strVal's data word; growing it back
// silently would undo that, so the number is asserted rather than left to a
// benchmark to notice. 32 also means two Values to a 64-byte cache line.
//
// Four words is the floor for a Value that keeps strVal a real Go string: the
// type tag, the scalar slot, and the two-word string header. Going lower means
// hand-rolling the string header, which costs content equality (see the comment
// on Value for why that trade is refused).
func TestValueSize(t *testing.T) {
	const want = 32
	if got := unsafe.Sizeof(Value{}); got != want {
		t.Errorf("unsafe.Sizeof(Value{}) = %d, want %d", got, want)
	}
	if got := unsafe.Sizeof(Value{}) * 2; got > 64 {
		t.Errorf("two Values are %d bytes, want <= one 64-byte cache line", got)
	}
}

// TestValueSizeIsAGoSizeClass guards a trap that a plain size assertion misses.
// Go rounds every individual heap allocation up to a size class, and the classes
// run 8, 16, 24, 32, 48, 64, 80, ... with NOTHING between 32 and 48. A Value of
// 33..48 bytes therefore occupies a 48-byte slot: the intermediate 40-byte layout
// this type passed through saved nothing at all for a boxed *Value, only for the
// contiguous []Value that the VM stack and materialized lists use.
//
// So "smaller" is not automatically "cheaper" here, and someone adding a field
// could take Value to 40 bytes, watch the slice benchmarks improve over 48, and
// never notice that every boxed Value still costs 48. Pin the size to a real
// class so that regression has to be deliberate.
func TestValueSizeIsAGoSizeClass(t *testing.T) {
	// The small-object size classes, up to well past any plausible Value.
	classes := []uintptr{8, 16, 24, 32, 48, 64, 80, 96, 112, 128}
	got := unsafe.Sizeof(Value{})
	for _, c := range classes {
		if got == c {
			return
		}
	}
	t.Errorf("unsafe.Sizeof(Value{}) = %d, which is not a Go size class (%v): a boxed Value would be rounded up and the extra bytes paid for but unused", got, classes)
}

// TestValuePointerWords pins the number of pointer-shaped words in a Value, which
// is what the garbage collector must scan for every live Value -- in a VM stack,
// in a materialized list, in a program's const pool. It went from three (string
// data, *ClassAd, *listData) to two, and then to one once the payload moved onto
// strVal's data word. Size alone would not catch a regression here: a future
// field could keep the struct at 32 bytes while adding a word for the GC to
// chase, so the count is walked out of the type rather than asserted alongside
// the size.
func TestValuePointerWords(t *testing.T) {
	const want = 1
	if got := pointerWords(reflect.TypeOf(Value{})); got != want {
		t.Errorf("Value has %d pointer-shaped words, want %d", got, want)
	}
}

// pointerWords counts the words in t that the collector must treat as pointers.
func pointerWords(t reflect.Type) int {
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.Map, reflect.Chan, reflect.Func:
		return 1
	case reflect.String:
		return 1 // {data pointer, length}: only the first word is a pointer
	case reflect.Slice:
		return 1 // {data pointer, length, capacity}: likewise
	case reflect.Interface:
		return 2 // {type/itab pointer, data pointer}: both are scanned
	case reflect.Struct:
		n := 0
		for i := range t.NumField() {
			n += pointerWords(t.Field(i).Type)
		}
		return n
	case reflect.Array:
		return t.Len() * pointerWords(t.Elem())
	default:
		return 0
	}
}

// TestRealBitsRoundTrip checks that folding a real's IEEE-754 bits into the
// shared scalar slot is exact for the values that a float64 -> int64 -> float64
// trip through a NUMERIC conversion would destroy: NaN (which is not equal to
// itself, so it has to be compared by bits), both infinities, and negative zero
// (which compares equal to +0, so it too has to be compared by bits).
func TestRealBitsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		f    float64
	}{
		{"NaN", math.NaN()},
		{"quiet NaN payload", math.Float64frombits(0x7FF8000000000001)},
		{"signaling NaN payload", math.Float64frombits(0x7FF0000000000001)},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"-0", math.Copysign(0, -1)},
		{"+0", 0},
		{"smallest subnormal", math.SmallestNonzeroFloat64},
		{"MaxFloat64", math.MaxFloat64},
		{"-MaxFloat64", -math.MaxFloat64},
		{"1/3", 1.0 / 3.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewRealValue(tc.f)
			if v.Type() != RealValue {
				t.Fatalf("Type() = %v, want RealValue", v.Type())
			}
			got, err := v.RealValue()
			if err != nil {
				t.Fatalf("RealValue() error: %v", err)
			}
			if math.Float64bits(got) != math.Float64bits(tc.f) {
				t.Errorf("RealValue() bits = %#016x, want %#016x", math.Float64bits(got), math.Float64bits(tc.f))
			}
			// NumberValue goes through the same slot by a different path.
			n, err := v.NumberValue()
			if err != nil {
				t.Fatalf("NumberValue() error: %v", err)
			}
			if math.Float64bits(n) != math.Float64bits(tc.f) {
				t.Errorf("NumberValue() bits = %#016x, want %#016x", math.Float64bits(n), math.Float64bits(tc.f))
			}
		})
	}
}

// TestPayloadPointerDiscrimination checks the one hazard the merged payload
// pointer introduces: the compiler no longer type-checks what comes back out of
// it, so a value of one type must never be read as the other. Each accessor is
// reached only through its own valueType, and the typed accessors must reject a
// mismatch rather than reinterpret the pointer.
func TestPayloadPointerDiscrimination(t *testing.T) {
	ad, err := Parse(`[A = 1]`)
	if err != nil {
		t.Fatal(err)
	}
	adVal := NewClassAdValue(ad)
	listVal := NewListValue([]Value{NewIntValue(1), NewIntValue(2)})

	if _, lerr := adVal.ListValue(); lerr == nil {
		t.Error("ListValue() on a ClassAd value: want error, got nil")
	}
	if _, cerr := listVal.ClassAdValue(); cerr == nil {
		t.Error("ClassAdValue() on a list value: want error, got nil")
	}
	got, err := adVal.ClassAdValue()
	if err != nil {
		t.Fatalf("ClassAdValue() error: %v", err)
	}
	if got != ad {
		t.Errorf("ClassAdValue() = %p, want %p", got, ad)
	}
	elems, err := listVal.ListValue()
	if err != nil {
		t.Fatalf("ListValue() error: %v", err)
	}
	if len(elems) != 2 {
		t.Errorf("ListValue() len = %d, want 2", len(elems))
	}

	// A zero Value is undefined and carries a nil payload; neither accessor may
	// be reachable on it.
	var zero Value
	if zero.Type() != UndefinedValue {
		t.Errorf("zero Value Type() = %v, want UndefinedValue", zero.Type())
	}
	if _, err := zero.ListValue(); err == nil {
		t.Error("ListValue() on the zero Value: want error, got nil")
	}
	if _, err := zero.ClassAdValue(); err == nil {
		t.Error("ClassAdValue() on the zero Value: want error, got nil")
	}
}

// TestPayloadPointerKeepsReferentAlive is the GC check, and it matters more in
// this layout than it did when the payload had a field of its own: the only
// reference to a nested ad or a list's payload is now the data word of a
// ZERO-LENGTH string. A reader could reasonably assume the collector skips the
// data word of an empty string -- it does not, because the pointer map is static
// and that word is a pointer word in every string. If that were ever untrue the
// object would be freed while a live Value still addressed it, and the corruption
// would surface far from here. Force several collections with no other reference
// alive and read the payload back.
func TestPayloadPointerKeepsReferentAlive(t *testing.T) {
	makeList := func() Value {
		// Built inside a function so no caller frame keeps the listData alive.
		return NewListValue([]Value{NewStringValue("alice"), NewIntValue(42)})
	}
	makeAd := func() Value {
		ad, err := Parse(`[Owner = "alice"; Cpus = 8]`)
		if err != nil {
			t.Fatal(err)
		}
		return NewClassAdValue(ad)
	}
	lv, av := makeList(), makeAd()
	for range 5 {
		gcAndChurn()
	}
	elems, err := lv.ListValue()
	if err != nil {
		t.Fatalf("ListValue() after GC: %v", err)
	}
	if len(elems) != 2 {
		t.Fatalf("ListValue() len = %d, want 2", len(elems))
	}
	if s, serr := elems[0].StringValue(); serr != nil || s != "alice" {
		t.Errorf("elems[0] = %q, %v; want \"alice\", nil", s, serr)
	}
	ad, err := av.ClassAdValue()
	if err != nil {
		t.Fatalf("ClassAdValue() after GC: %v", err)
	}
	if v, ok := ad.EvaluateAttrString("Owner"); !ok || v != "alice" {
		t.Errorf("Owner after GC = %q, %v; want \"alice\", true", v, ok)
	}
}

// TestZeroLengthAliasRoundTrip guards the one assumption this layout makes about
// the Go runtime that the language does not promise: that a zero-length string
// built by unsafe.String keeps the data pointer it was given instead of being
// canonicalized to the empty string. The standard library documents the data
// pointer of an empty string as unspecified, so if a future release ever does
// canonicalize, classAd() and list() would hand back nil and the failure would
// look like an unrelated nil dereference deep in evaluation. This test turns
// that into a loud, obvious failure at the layout instead.
func TestZeroLengthAliasRoundTrip(t *testing.T) {
	ad, err := Parse(`[Owner = "alice"]`)
	if err != nil {
		t.Fatal(err)
	}
	s := payloadString(unsafe.Pointer(ad))
	if s != "" {
		t.Fatalf("payloadString length = %d, want 0 (a nonzero length makes == read the ad's raw memory)", len(s))
	}
	if got := (*ClassAd)(unsafe.Pointer(unsafe.StringData(s))); got != ad {
		t.Fatalf("aliased pointer round-trip: got %p, want %p -- the Go runtime canonicalized a zero-length string's data pointer", got, ad)
	}
	// A nil payload must survive too: NewClassAdValue(nil) is legal and String()
	// renders it "[]". unsafe.String panics on a nil pointer only when len > 0.
	if nilAd := NewClassAdValue(nil); nilAd.classAd() != nil {
		t.Errorf("NewClassAdValue(nil).classAd() = %p, want nil", nilAd.classAd())
	}
}

// TestEqualitySemantics pins what Go's `==` does for every pair of Value types.
// Value is an exported comparable struct, so `==` and use as a map key are part
// of its API whether or not this repo writes them, and this layout deliberately
// changes one cell of the table: aliasing the payload onto a ZERO-LENGTH string
// means Go compares ad and list values on the length alone and never reaches the
// pointers, so two DIFFERENT ads now compare equal. That is the price of the
// fourth word. It is pinned here, per cell, so it is a documented decision rather
// than something a reader discovers in production.
func TestEqualitySemantics(t *testing.T) {
	adA, err := Parse(`[A = 1]`)
	if err != nil {
		t.Fatal(err)
	}
	adB, err := Parse(`[B = 2]`)
	if err != nil {
		t.Fatal(err)
	}
	// Built from separate backing arrays on purpose: the whole reason strVal
	// stays a real Go string is that these must still compare equal.
	aliceLit := "alice"
	aliceBuilt := string([]byte{'a', 'l', 'i', 'c', 'e'})

	cases := []struct {
		name string
		a, b Value
		want bool
	}{
		// Strings: CONTENT equality, the property the 24-byte layout gave up.
		{"string/string same content, different backing", NewStringValue(aliceLit), NewStringValue(aliceBuilt), true},
		{"string/string different content", NewStringValue("alice"), NewStringValue("bob"), false},
		{"string/string differing only in case", NewStringValue("Alice"), NewStringValue("alice"), false},
		{"string/string both empty", NewStringValue(""), NewStringValue(""), true},
		// Scalars: unchanged.
		{"int/int same", NewIntValue(7), NewIntValue(7), true},
		{"int/int different", NewIntValue(7), NewIntValue(8), false},
		{"real/real same", NewRealValue(1.5), NewRealValue(1.5), true},
		{"bool/bool same", NewBoolValue(true), NewBoolValue(true), true},
		{"bool/bool different", NewBoolValue(true), NewBoolValue(false), false},
		{"undefined/undefined", NewUndefinedValue(), NewUndefinedValue(), true},
		{"error/error", NewErrorValue(), NewErrorValue(), true},
		// THE CHANGED CELLS: ad/ad and list/list are now always equal within
		// their type, where they used to compare by pointer identity.
		{"ad/ad same pointer", NewClassAdValue(adA), NewClassAdValue(adA), true},
		{"ad/ad DIFFERENT pointers (was false)", NewClassAdValue(adA), NewClassAdValue(adB), true},
		{"list/list DIFFERENT pointers (was false)", NewListValue([]Value{NewIntValue(1)}), NewListValue([]Value{NewIntValue(2)}), true},
		// Cross-type: still separated by valueType, including the cases that
		// would otherwise collide now that ad/list carry a zero-length string.
		{"ad vs empty string", NewClassAdValue(adA), NewStringValue(""), false},
		{"list vs empty string", NewListValue(nil), NewStringValue(""), false},
		{"ad vs list", NewClassAdValue(adA), NewListValue(nil), false},
		{"undefined vs error", NewUndefinedValue(), NewErrorValue(), false},
		{"int vs real same numeric", NewIntValue(1), NewRealValue(1), false},
		{"string vs int", NewStringValue("1"), NewIntValue(1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a == tc.b; got != tc.want {
				t.Errorf("a == b = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValueAsMapKey is the other half of comparability: a Value must still work
// as a map key, which is what would have broken outright had strVal stopped
// being a real string. Separately built equal strings must hit the same bucket.
func TestValueAsMapKey(t *testing.T) {
	m := map[Value]string{}
	m[NewStringValue("alice")] = "first"
	m[NewStringValue(string([]byte{'a', 'l', 'i', 'c', 'e'}))] = "second"
	if len(m) != 1 {
		t.Errorf("map has %d entries, want 1: separately built equal strings must be one key", len(m))
	}
	if got := m[NewStringValue("alice")]; got != "second" {
		t.Errorf("lookup = %q, want %q", got, "second")
	}
	m[NewIntValue(1)] = "int"
	m[NewRealValue(1)] = "real"
	m[NewBoolValue(true)] = "bool"
	if len(m) != 4 {
		t.Errorf("map has %d entries, want 4: the scalar types must stay distinct keys", len(m))
	}
}
