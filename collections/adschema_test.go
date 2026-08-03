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
		`Cpus = -3` + "\n" + `Memory = 4096` + "\n" + `Big = false` + "\n" + `Load = 0.0` + "\n" + `Arch = "ARM"` + "\n" + `Machine = "neg.example.org"`,        // negative -> signed
		`Cpus = 2` + "\n" + `Memory = "unknown"` + "\n" + `Big = true` + "\n" + `Load = 2.0` + "\n" + `Arch = "X86_64"` + "\n" + `Machine = "typo.example.org"`, // Memory string -> type exception
		`Memory = 8192` + "\n" + `Big = true` + "\n" + `Load = 3.0` + "\n" + `Arch = "PPC"` + "\n" + `Machine = "nocpus.example.org"`,                           // Cpus missing
		`Cpus = 4` + "\n" + `Memory = 512` + "\n" + `Big = false` + "\n" + `Load = -1.25` + "\n" + `Arch = "s"` + "\n" + `Machine = "x"` + "\n" + `Extra = 7`,   // short strings (<=7) inline; a rare attr
	}
	for _, s := range edge {
		add(s)
	}

	for _, strings := range []bool{false, true} {
		s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: strings})
		for i, w := range wires {
			assertRoundTrip(t, s, w, fmt.Sprintf("strings=%v ad[%d]", strings, i))
		}
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
