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
// merging the mutually exclusive *ClassAd and *listData fields into one pointer
// discriminated by valueType; growing it back silently would undo that, so the
// number is asserted rather than left to a benchmark to notice.
//
// Five words is the floor for a Value that keeps a real Go string: the type tag,
// the scalar slot, the two-word string header, and the payload pointer. Going
// lower means hand-rolling the string header (see the comment on Value.strVal
// for why that trade is refused).
func TestValueSize(t *testing.T) {
	const want = 40
	if got := unsafe.Sizeof(Value{}); got != want {
		t.Errorf("unsafe.Sizeof(Value{}) = %d, want %d", got, want)
	}
}

// TestValuePointerWords pins the number of pointer-shaped words in a Value, which
// is what the garbage collector must scan for every live Value -- in a VM stack,
// in a materialized list, in a program's const pool. Merging the two payload
// pointers took it from three (string data, *ClassAd, *listData) to two (string
// data, payload). Size alone would not catch a regression here: a future field
// could keep the struct at 40 bytes while adding a word for the GC to chase, so
// the count is walked out of the type rather than asserted alongside the size.
func TestValuePointerWords(t *testing.T) {
	const want = 2
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

	if _, err := adVal.ListValue(); err == nil {
		t.Error("ListValue() on a ClassAd value: want error, got nil")
	}
	if _, err := listVal.ClassAdValue(); err == nil {
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

// TestPayloadPointerKeepsReferentAlive is the GC check. ptr is an unsafe.Pointer,
// so the compiler cannot tell what it addresses -- but the runtime still treats
// it as a pointer word, which is the whole reason it is unsafe.Pointer and not
// uintptr. If it were not in Value's pointer map, the only reference to a list's
// payload or to a nested ad would be invisible to the collector and the object
// would be freed under us. Force several collections with no other reference
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
	if s, err := elems[0].StringValue(); err != nil || s != "alice" {
		t.Errorf("elems[0] = %q, %v; want \"alice\", nil", s, err)
	}
	ad, err := av.ClassAdValue()
	if err != nil {
		t.Fatalf("ClassAdValue() after GC: %v", err)
	}
	if v, ok := ad.EvaluateAttrString("Owner"); !ok || v != "alice" {
		t.Errorf("Owner after GC = %q, %v; want \"alice\", true", v, ok)
	}
}
