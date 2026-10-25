package collections

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// adAttrs collects a wire ad's attributes as id -> node (copied).
func adAttrs(w []byte) map[uint32][]byte {
	m := map[uint32][]byte{}
	wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
		m[id] = append([]byte(nil), node...)
		return true
	})
	return m
}

// sameValue compares two wire nodes by value: scalar literals by their decoded value (the
// schema re-synthesizes them), computed expressions by raw bytes (they pass through the cold
// tail unchanged).
func sameValue(a, b []byte) bool {
	la, oka := wire.LiteralValue(a)
	lb, okb := wire.LiteralValue(b)
	if oka != okb {
		return false
	}
	if oka {
		return la == lb
	}
	return bytes.Equal(a, b)
}

// assertRoundTrip encodes w under s, decodes it, and checks every attribute survives with the
// same value.
func assertRoundTrip(t *testing.T, s *adSchema, w []byte, what string) {
	t.Helper()
	rec := s.encode(wire.Ad(w))
	dec := map[uint32][]byte{}
	if !s.forEach(rec, func(id uint32, node []byte) bool {
		dec[id] = append([]byte(nil), node...)
		return true
	}) {
		t.Fatalf("%s: forEach reported a malformed record", what)
	}
	orig := adAttrs(w)
	if len(dec) != len(orig) {
		t.Errorf("%s: round-trip has %d attrs, want %d", what, len(dec), len(orig))
	}
	for id, on := range orig {
		dn, ok := dec[id]
		if !ok {
			t.Errorf("%s: attr id=%d lost in round-trip", what, id)
			continue
		}
		if !sameValue(on, dn) {
			lo, _ := wire.LiteralValue(on)
			ld, _ := wire.LiteralValue(dn)
			t.Errorf("%s: attr id=%d value changed: %+v -> %+v", what, id, lo, ld)
		}
	}
}

// TestAdSchemaRoundTripSynthetic exercises every encoding path: fixed slots, bit-packed bools,
// width-escaped ints (a value past the chosen width), missing fields, type exceptions, inline
// and tabled strings, negative ints, reals, and expression pass-through.
func TestAdSchemaRoundTripSynthetic(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	add := func(src string) []byte {
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		w := c.encodeAd(ad.AST())
		wires = append(wires, w)
		return w
	}

	// 16 "normal" slot-like ads: Cpus/Memory fit small widths, common bool/real/strings.
	for i := 0; i < 16; i++ {
		add(fmt.Sprintf("Cpus = %d\nMemory = %d\nBig = %t\nLoad = %f\nArch = \"X86_64\"\nMachine = \"node%02d.example.org\"\nRequirements = (TARGET.Cpus >= Cpus)",
			1+i%8, 1024+i*64, i%2 == 0, float64(i)/3, i))
	}
	// Edge ads (still exercised for round-trip regardless of schema membership):
	edge := []string{
		`Cpus = 99999` + "\n" + `Memory = 2048` + "\n" + `Big = true` + "\n" + `Load = 1.5` + "\n" + `Arch = "X86_64"` + "\n" + `Machine = "huge.example.org"`,  // Cpus past uint8/uint16 -> escapes
		`Cpus = -3` + "\n" + `Memory = 4096` + "\n" + `Big = false` + "\n" + `Load = 0.0` + "\n" + `Arch = "ARM"` + "\n" + `Machine = "neg.example.org"`,        // negative -> signed (asserted below)
		`Cpus = 2` + "\n" + `Memory = "unknown"` + "\n" + `Big = true` + "\n" + `Load = 2.0` + "\n" + `Arch = "X86_64"` + "\n" + `Machine = "typo.example.org"`, // Memory string -> type exception
		`Memory = 8192` + "\n" + `Big = true` + "\n" + `Load = 3.0` + "\n" + `Arch = "PPC"` + "\n" + `Machine = "nocpus.example.org"`,                           // Cpus missing
		`Cpus = 4` + "\n" + `Memory = 512` + "\n" + `Big = false` + "\n" + `Load = -1.25` + "\n" + `Arch = "s"` + "\n" + `Machine = "x"` + "\n" + `Extra = 7`,   // short strings (<=7) inline; a rare attr
	}
	for _, s := range edge {
		add(s)
	}

	for _, strings := range []bool{false, true} {
		s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: strings})
		// The `Cpus = -3` edge ad is in the corpus to make Cpus a SIGNED column. Asserting it
		// rather than only saying so in a comment: the comment was wrong for as long as the
		// encoder left `-3` a unary-minus expression, because the negative value then never
		// reached the sampled ints at all and the width chooser picked unsigned from a sample
		// that looked entirely non-negative. Nothing failed -- the record just escaped the
		// column -- which is exactly why the claim has to be checked rather than asserted in
		// prose.
		assertIntField(t, s, c, "Cpus", false)
		for i, w := range wires {
			assertRoundTrip(t, s, w, fmt.Sprintf("strings=%v ad[%d]", strings, i))
		}
	}
}

// assertIntField checks that name is an int column of the expected signedness.
func assertIntField(t *testing.T, s *adSchema, c *Collection, name string, wantUnsigned bool) {
	t.Helper()
	id, ok := c.intern.LookupID(name)
	if !ok {
		t.Fatalf("%s was never interned", name)
	}
	idx, ok := s.byID[id]
	if !ok {
		t.Fatalf("%s got no columnar field", name)
	}
	f := s.fields[idx]
	if f.kind != akInt {
		t.Fatalf("%s is kind %d, want akInt", name, f.kind)
	}
	if f.unsigned != wantUnsigned {
		t.Errorf("%s column unsigned=%v, want %v", name, f.unsigned, wantUnsigned)
	}
}

// TestAdSchemaRoundTripOSPool round-trips every ad in the real corpus (skips without testdata).
func TestAdSchemaRoundTripOSPool(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	_, wires := encodeOSPool(t, ads)
	for _, strings := range []bool{false, true} {
		s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: strings})
		for i, w := range wires {
			assertRoundTrip(t, s, w, fmt.Sprintf("ospool strings=%v ad[%d]", strings, i))
		}
	}
}

// TestChooseIntWidth checks the fit-percentile width selection: a rare outlier does not widen
// the field (it will escape instead), and signedness follows the sample.
func TestChooseIntWidth(t *testing.T) {
	vals := make([]int64, 0, 100)
	for i := 0; i < 99; i++ {
		vals = append(vals, int64(i)) // 0..98 fit uint8
	}
	vals = append(vals, 1_000_000) // one outlier
	if w, u := chooseIntWidth(vals, 0.95); w != 1 || !u {
		t.Errorf("chooseIntWidth = (%d,%v), want (1,true) -- outlier should escape, not widen", w, u)
	}
	if w, u := chooseIntWidth(vals, 0.999); w != 4 || !u {
		t.Errorf("chooseIntWidth strict = (%d,%v), want (4,true) -- must widen to cover the outlier", w, u)
	}
	if w, _ := chooseIntWidth([]int64{-1, 5, 100}, 0.95); w != 1 {
		t.Errorf("signed small = width %d, want 1", w)
	}
}
