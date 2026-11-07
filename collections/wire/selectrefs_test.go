package wire

import (
	"slices"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A `.`-path select's member is a reference to the ENCLOSING ad, not only to the record
// it is written against: a member the record does not define chains out to the scope the
// select appears in. `X = Res.Mem` therefore reads Mem from the ad whenever Res has none,
// and a reference walk that stopped at the record shipped Res without Mem -- leaving X
// undefined on the projected row while it is 4096 on the whole ad.
//
// These hold all three walks to that: the interned-id collector, the inline-name
// collector, and the exact (completeness-reporting) one.

func selectRefsAd(t *testing.T) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(`[ Mem = 4096; Res = [ Other = 1 ]; X = Res.Mem; Y = Res.Mem + Slack; Z = A.B.Deep ]`)
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

func TestNodeRefsIncludeSelectedMemberInterned(t *testing.T) {
	tbl := NewInternTable()
	enc := Encode(nil, selectRefsAd(t).AST(), tbl)
	for _, tc := range []struct {
		attr string
		want []string
	}{
		{"X", []string{"Mem", "Res"}},
		{"Y", []string{"Mem", "Res", "Slack"}},
		{"Z", []string{"A", "B", "Deep"}},
	} {
		id, ok := tbl.LookupID(tc.attr)
		if !ok {
			t.Fatalf("%s not interned", tc.attr)
		}
		node, ok := Ad(enc).Lookup(id)
		if !ok {
			t.Fatalf("%s not in the encoded ad", tc.attr)
		}

		var got []string
		for _, rid := range AppendNodeRefIDs(node, nil) {
			n, ok := tbl.Name(rid)
			if !ok {
				t.Fatalf("%s: collected id %d names nothing", tc.attr, rid)
			}
			got = append(got, n)
		}
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("AppendNodeRefIDs(%s) = %v, want %v", tc.attr, got, want)
		}

		var r NodeRefs
		if !AppendNodeRefsExact(node, false, &r, func(uint32) bool { return false }) {
			t.Fatalf("AppendNodeRefsExact(%s) reported incomplete", tc.attr)
		}
		got = got[:0]
		for _, rid := range r.IDs {
			n, _ := tbl.Name(rid)
			got = append(got, n)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("AppendNodeRefsExact(%s).IDs = %v, want %v", tc.attr, got, want)
		}
	}
}

func TestNodeRefsIncludeSelectedMemberInline(t *testing.T) {
	enc := EncodeInline(nil, selectRefsAd(t).AST())
	for _, tc := range []struct {
		attr string
		want []string
	}{
		{"X", []string{"Mem", "Res"}},
		{"Y", []string{"Mem", "Res", "Slack"}},
		{"Z", []string{"A", "B", "Deep"}},
	} {
		node, ok := Ad(enc).LookupByName(tc.attr)
		if !ok {
			t.Fatalf("%s not in the encoded ad", tc.attr)
		}
		var got []string
		for _, n := range AppendNodeRefNames(node, nil) {
			got = append(got, string(n))
		}
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("AppendNodeRefNames(%s) = %v, want %v", tc.attr, got, want)
		}

		var r NodeRefs
		if !AppendNodeRefsExact(node, true, &r, nil) {
			t.Fatalf("AppendNodeRefsExact(%s) reported incomplete", tc.attr)
		}
		got = got[:0]
		for _, n := range r.Names {
			got = append(got, string(n))
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("AppendNodeRefsExact(%s).Names = %v, want %v", tc.attr, got, want)
		}
	}
}
