package collections

import (
	"fmt"
	"math"
	"testing"
)

// profileOf indexes a profile report by attribute name, dropping the sample count.
func profileOf(t *testing.T, c *Collection, sampleMax int) map[string]AttrTypeProfile {
	t.Helper()
	profs, sampled := c.AttrTypeProfiles(sampleMax)
	if sampled == 0 {
		t.Fatal("AttrTypeProfiles sampled nothing")
	}
	m := map[string]AttrTypeProfile{}
	for _, p := range profs {
		m[p.Name] = p
	}
	return m
}

// TestAttrTypeProfilesClassifiesEachKind is the baseline: one attribute per class, so a
// misclassification shows up as the wrong class name rather than as a shifted count.
func TestAttrTypeProfilesClassifiesEachKind(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := 0; i < 50; i++ {
		ad := mustAdOld(t, "Cpus=4\nRatio=0.5\nOwner=\"bbockelm\"\nDone=true\n"+
			"Req=Cpus > 2\nTags={1, 2}\nNested=[a = 1]\nNothing=undefined")
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	got := profileOf(t, c, 1000)

	want := map[string]string{
		"Cpus": "int", "Ratio": "real", "Owner": "string", "Done": "bool",
		"Req": "expr", "Tags": "list", "Nested": "classad", "Nothing": "undefined",
	}
	for name, class := range want {
		p, ok := got[name]
		if !ok {
			t.Errorf("%s: no profile", name)
			continue
		}
		if p.Dominant != class {
			t.Errorf("%s: dominant %q, want %q (classes %v)", name, p.Dominant, class, p.Classes)
		}
		if !p.Uniform() {
			t.Errorf("%s: classes %v, want a single class", name, p.Classes)
		}
		if p.Present != 50 {
			t.Errorf("%s: present %d, want 50", name, p.Present)
		}
	}
}

// TestAttrTypeProfilesExactIntBounds asserts the bounds are the real extremes, not the
// extremes of a width or of a zone-map float64 summary. This is what a consumer needs to
// decide whether an integer attribute can be narrowed or widened.
func TestAttrTypeProfilesExactIntBounds(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := 0; i < 100; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Small=%d\nSigned=%d", i, i-50))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	got := profileOf(t, c, 1000)

	if p := got["Small"]; p.IntMin != 0 || p.IntMax != 99 || p.Ints != 100 {
		t.Errorf("Small: ints=%d [%d,%d], want 100 [0,99]", p.Ints, p.IntMin, p.IntMax)
	}
	if p := got["Signed"]; p.IntMin != -50 || p.IntMax != 49 {
		t.Errorf("Signed: [%d,%d], want [-50,49]", p.IntMin, p.IntMax)
	}
	if p := got["Small"]; p.IntsLossyAsFloat != 0 {
		t.Errorf("Small: %d values flagged lossy as float, want 0", p.IntsLossyAsFloat)
	}
}

// TestAttrTypeProfilesFlagsIntsLossyAsFloat is the measurement that decides whether a mixed
// int/real attribute may be declared as a float type. An integer past 2^53 shares a float64
// with its neighbour, so declaring the column floating-point would silently alter it.
func TestAttrTypeProfilesFlagsIntsLossyAsFloat(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	// 2^53 itself is exact; one above it is not. Include the exact boundary so an
	// off-by-one in the comparison is caught rather than hidden.
	vals := []int64{0, 1 << 53, (1 << 53) + 1, -(1 << 53), -(1 << 53) - 1}
	for i, v := range vals {
		ad := mustAdOld(t, fmt.Sprintf("Big=%d", v))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	p := profileOf(t, c, 1000)["Big"]
	if p.Ints != len(vals) {
		t.Fatalf("Big: ints=%d, want %d", p.Ints, len(vals))
	}
	if p.IntsLossyAsFloat != 2 {
		t.Errorf("Big: lossy=%d, want 2 (only the two past 2^53)", p.IntsLossyAsFloat)
	}
	if p.IntMin != -(1<<53)-1 || p.IntMax != (1<<53)+1 {
		t.Errorf("Big: [%d,%d], want [%d,%d]", p.IntMin, p.IntMax, -(int64(1)<<53)-1, (int64(1)<<53)+1)
	}
}

// TestAttrTypeProfilesHeterogeneous is the case the whole measurement exists for: an
// attribute that is not one type. A consumer that declared a type from the first value it saw
// would get this wrong, which is what two of our shipping clients do today.
func TestAttrTypeProfilesHeterogeneous(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := 0; i < 100; i++ {
		src := fmt.Sprintf("Mixed=%d", i)
		if i%10 == 0 {
			src = `Mixed="n/a"` // one in ten is a string
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	p := profileOf(t, c, 1000)["Mixed"]
	if p.Uniform() {
		t.Fatalf("Mixed: reported uniform, classes %v", p.Classes)
	}
	if p.Classes["int"] != 90 || p.Classes["string"] != 10 {
		t.Errorf("Mixed: classes %v, want 90 int / 10 string", p.Classes)
	}
	if p.Dominant != "int" {
		t.Errorf("Mixed: dominant %q, want int", p.Dominant)
	}
	// The bounds describe the integers that ARE there, ignoring the strings.
	if p.IntMin != 1 || p.IntMax != 99 {
		t.Errorf("Mixed: int bounds [%d,%d], want [1,99]", p.IntMin, p.IntMax)
	}
}

// TestAttrTypeProfilesAbsence separates "absent" from "present but some other type".
// Present < sampled is what decides whether a declared type must admit nulls, and it is a
// different question from which classes appeared.
func TestAttrTypeProfilesAbsence(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := 0; i < 100; i++ {
		src := "Always=1"
		if i%4 == 0 {
			src += "\nSometimes=2"
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	profs, sampled := c.AttrTypeProfiles(1000)
	if sampled != 100 {
		t.Fatalf("sampled %d, want 100", sampled)
	}
	m := map[string]AttrTypeProfile{}
	for _, p := range profs {
		m[p.Name] = p
	}
	if p := m["Always"]; p.Present != 100 {
		t.Errorf("Always: present %d, want 100", p.Present)
	}
	// Uniform despite being absent most of the time: absence is not a class.
	if p := m["Sometimes"]; p.Present != 25 || !p.Uniform() {
		t.Errorf("Sometimes: present %d uniform %v classes %v, want 25 and uniform",
			p.Present, p.Uniform(), p.Classes)
	}
}

// TestAttrTypeProfilesNaNExcludedFromBounds pins the NaN handling. A NaN is a real by kind and
// a member of no order, so including it in the bounds would make them describe a range the NaN
// provably sits outside of -- the exact mistake the columnar layer escapes NaNs to avoid.
func TestAttrTypeProfilesNaNExcludedFromBounds(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i, v := range []float64{1.5, math.NaN(), -2.5, math.NaN(), 10.0} {
		ad := mustAdOld(t, "Tag=1")
		ad.InsertAttrFloat("Measure", v)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	p := profileOf(t, c, 1000)["Measure"]
	if p.Reals != 5 {
		t.Errorf("Measure: reals=%d, want 5 (NaNs included in the count)", p.Reals)
	}
	if p.RealNaNs != 2 {
		t.Errorf("Measure: NaNs=%d, want 2", p.RealNaNs)
	}
	if p.RealMin != -2.5 || p.RealMax != 10.0 {
		t.Errorf("Measure: bounds [%v,%v], want [-2.5,10]", p.RealMin, p.RealMax)
	}
	if math.IsNaN(p.RealMin) || math.IsNaN(p.RealMax) {
		t.Error("Measure: a NaN reached the bounds")
	}
}

// TestAttrTypeProfilesInfiniteBounds asserts +/-Inf is NOT treated like a NaN: it is ordered,
// so it belongs in the bounds. The columnar layer makes the same distinction deliberately.
func TestAttrTypeProfilesInfiniteBounds(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i, v := range []float64{0, math.Inf(1), math.Inf(-1)} {
		ad := mustAdOld(t, "Tag=1")
		ad.InsertAttrFloat("Measure", v)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	p := profileOf(t, c, 1000)["Measure"]
	if p.RealNaNs != 0 {
		t.Errorf("Measure: NaNs=%d, want 0 (an infinity is not a NaN)", p.RealNaNs)
	}
	if !math.IsInf(p.RealMin, -1) || !math.IsInf(p.RealMax, 1) {
		t.Errorf("Measure: bounds [%v,%v], want [-Inf,+Inf]", p.RealMin, p.RealMax)
	}
}

// TestAttrTypeProfilesOrderAndEmpty covers the two contract details a caller depends on: the
// order is stable and most-present-first, and an empty store reports nothing rather than
// panicking or inventing a profile.
func TestAttrTypeProfilesOrderAndEmpty(t *testing.T) {
	empty := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer empty.Close()
	if profs, sampled := empty.AttrTypeProfiles(100); profs != nil || sampled != 0 {
		t.Errorf("empty store: got %d profiles / %d sampled, want nil/0", len(profs), sampled)
	}

	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := 0; i < 100; i++ {
		src := "Common=1"
		if i < 60 {
			src += "\nMiddle=1"
		}
		if i < 20 {
			src += "\nRare=1"
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	profs, _ := c.AttrTypeProfiles(1000)
	var order []string
	for _, p := range profs {
		if p.Name == "Common" || p.Name == "Middle" || p.Name == "Rare" {
			order = append(order, p.Name)
		}
	}
	want := []string{"Common", "Middle", "Rare"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order %v, want %v first", order, want)
		}
	}
}

// TestAttrTypeProfilesWithoutAccelerator asserts the profile does not require the columnar
// accelerator. It measures what the data holds, which is exactly the input to the decision of
// whether to enable a schema at all -- so needing one first would be circular.
func TestAttrTypeProfilesWithoutAccelerator(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := 0; i < 50; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, "Cpus=2")); err != nil {
			t.Fatal(err)
		}
	}
	if fit, _ := c.SchemaFit(1000); fit != nil {
		t.Fatal("precondition: the accelerator is enabled, so this proves nothing")
	}
	if p := profileOf(t, c, 1000)["Cpus"]; p.Ints != 50 {
		t.Errorf("Cpus: ints=%d, want 50 with no accelerator enabled", p.Ints)
	}
}
