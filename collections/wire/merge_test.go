package wire

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// mergeRef is the reference merge: decode every participant and insert the overlays'
// attributes over the base's, which is what the splice has to agree with. Insert semantics are
// last-write-wins, so overlays are applied in order.
func mergeRef(t *testing.T, base *ast.ClassAd, overlays []*ast.ClassAd) map[string]string {
	t.Helper()
	out := map[string]string{}
	put := func(a *ast.ClassAd) {
		for _, at := range a.Attributes {
			out[strings.ToLower(at.Name)] = at.Value.String()
		}
	}
	put(base)
	for _, ov := range overlays {
		put(ov)
	}
	return out
}

// decodedMap renders an encoded ad as name(lowered) -> unparsed value, resolving duplicate names
// the way a decode does.
func decodedMap(t *testing.T, b []byte) map[string]string {
	t.Helper()
	a, err := DecodeInline(b)
	if err != nil {
		t.Fatalf("decoding merged ad: %v", err)
	}
	out := map[string]string{}
	for _, at := range a.Attributes {
		out[strings.ToLower(at.Name)] = at.Value.String()
	}
	return out
}

func adOf(pairs ...[2]string) *ast.ClassAd {
	a := &ast.ClassAd{}
	for _, p := range pairs {
		a.Attributes = append(a.Attributes, &ast.AttributeAssignment{Name: p[0], Value: &ast.StringLiteral{Value: p[1]}})
	}
	return a
}

// TestMergedInlineMatchesDecodeMerge is the correctness claim the splice rests on: splicing entry
// BYTES must produce an ad that decodes to exactly what decoding and re-inserting produces.
func TestMergedInlineMatchesDecodeMerge(t *testing.T) {
	cases := []struct {
		name     string
		base     *ast.ClassAd
		overlays []*ast.ClassAd
	}{
		{"replace one", adOf([2]string{"A", "1"}, [2]string{"B", "2"}), []*ast.ClassAd{adOf([2]string{"B", "9"})}},
		{"add one", adOf([2]string{"A", "1"}), []*ast.ClassAd{adOf([2]string{"C", "3"})}},
		{"replace first", adOf([2]string{"A", "1"}, [2]string{"B", "2"}), []*ast.ClassAd{adOf([2]string{"A", "9"})}},
		{"replace last", adOf([2]string{"A", "1"}, [2]string{"B", "2"}), []*ast.ClassAd{adOf([2]string{"B", "9"})}},
		{"case differs", adOf([2]string{"JobStatus", "1"}), []*ast.ClassAd{adOf([2]string{"JOBSTATUS", "5"})}},
		{"chain, later wins", adOf([2]string{"A", "1"}), []*ast.ClassAd{adOf([2]string{"A", "2"}), adOf([2]string{"A", "3"})}},
		{"chain, disjoint", adOf([2]string{"A", "1"}), []*ast.ClassAd{adOf([2]string{"B", "2"}), adOf([2]string{"C", "3"})}},
		{"overlay repeats a name", adOf([2]string{"A", "1"}), []*ast.ClassAd{adOf([2]string{"A", "2"}, [2]string{"A", "3"})}},
		{"empty overlay", adOf([2]string{"A", "1"}), []*ast.ClassAd{adOf()}},
		{"empty base", adOf(), []*ast.ClassAd{adOf([2]string{"A", "1"})}},
		{"no overlays", adOf([2]string{"A", "1"}), nil},
	}
	var sc MergeScratch
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnc := EncodeInline(nil, tc.base)
			ovEnc := make([]Ad, len(tc.overlays))
			for i, ov := range tc.overlays {
				ovEnc[i] = Ad(EncodeInline(nil, ov))
			}
			got, ok := AppendAdMergedInline(nil, Ad(baseEnc), ovEnc, &sc)
			if !ok {
				t.Fatal("AppendAdMergedInline refused a well-formed inline ad")
			}
			want := mergeRef(t, tc.base, tc.overlays)
			if g := decodedMap(t, got); fmt.Sprint(g) != fmt.Sprint(want) {
				t.Fatalf("spliced merge disagrees with decode-merge:\n got %v\nwant %v", g, want)
			}
		})
	}
}

// TestMergedInlineRandomAgrees is the same claim over random shapes: wide bases, overlays that
// overlap partially, mixed value types, names differing only in case.
func TestMergedInlineRandomAgrees(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var sc MergeScratch
	names := []string{"ClusterId", "ProcId", "Owner", "JobStatus", "Cmd", "Args", "Env", "Pad0", "Pad1", "Pad2", "LastJobLeaseRenewal", "BytesSent"}
	valueOf := func() ast.Expr {
		switch r.Intn(5) {
		case 0:
			return &ast.IntegerLiteral{Value: int64(r.Intn(1 << 20))}
		case 1:
			return &ast.RealLiteral{Value: r.Float64()}
		case 2:
			return &ast.BooleanLiteral{Value: r.Intn(2) == 0}
		case 3:
			return &ast.StringLiteral{Value: strings.Repeat("x", r.Intn(40))}
		default:
			return ast.NewAttributeReference("Other", ast.NoScope)
		}
	}
	for iter := 0; iter < 400; iter++ {
		base := &ast.ClassAd{}
		for _, n := range names {
			if r.Intn(4) > 0 {
				base.Attributes = append(base.Attributes, &ast.AttributeAssignment{Name: n, Value: valueOf()})
			}
		}
		nOv := r.Intn(4)
		overlays := make([]*ast.ClassAd, nOv)
		for i := range overlays {
			ov := &ast.ClassAd{}
			for j := 0; j < 1+r.Intn(3); j++ {
				n := names[r.Intn(len(names))]
				if r.Intn(3) == 0 { // exercise case-insensitive matching against the base
					n = strings.ToUpper(n)
				}
				ov.Attributes = append(ov.Attributes, &ast.AttributeAssignment{Name: n, Value: valueOf()})
			}
			overlays[i] = ov
		}
		baseEnc := EncodeInline(nil, base)
		ovEnc := make([]Ad, len(overlays))
		for i, ov := range overlays {
			ovEnc[i] = Ad(EncodeInline(nil, ov))
		}
		got, ok := AppendAdMergedInline(nil, Ad(baseEnc), ovEnc, &sc)
		if !ok {
			t.Fatalf("iter %d: refused", iter)
		}
		want := mergeRef(t, base, overlays)
		if g := decodedMap(t, got); fmt.Sprint(g) != fmt.Sprint(want) {
			t.Fatalf("iter %d: spliced merge disagrees\n got %v\nwant %v", iter, g, want)
		}
	}
}

// TestMergedInlineRefusesInterned: an interned ad's entries name their attributes by an id into a
// table the other participants do not share, so entries cannot be copied between ads. The splice
// must refuse rather than emit entries whose names resolve elsewhere.
func TestMergedInlineRefusesInterned(t *testing.T) {
	tbl := NewInternTable()
	interned := Encode(nil, adOf([2]string{"A", "1"}), tbl)
	inline := EncodeInline(nil, adOf([2]string{"B", "2"}))
	var sc MergeScratch
	if _, ok := AppendAdMergedInline(nil, Ad(interned), []Ad{Ad(inline)}, &sc); ok {
		t.Error("spliced an interned base: its entries carry ids, not names")
	}
	if _, ok := AppendAdMergedInline(nil, Ad(inline), []Ad{Ad(interned)}, &sc); ok {
		t.Error("spliced an interned overlay")
	}
	if _, ok := AppendAdMergedInline(nil, Ad("not an ad"), nil, &sc); ok {
		t.Error("spliced a malformed base")
	}
}
