package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

func TestEncodeWithHot(t *testing.T) {
	src := `[Owner="alice"; Cpus=4; Memory=8192; Rank=Cpus*2; Arch="X86_64"; Extra="z"; Deep=[k=1]]`
	orig, err := parser.ParseClassAd(src)
	if err != nil {
		t.Fatal(err)
	}
	tbl := NewInternTable()
	hot := map[uint32]struct{}{}
	for _, n := range []string{"Cpus", "Arch", "Memory"} {
		hot[tbl.Intern(n)] = struct{}{}
	}
	enc := Ad(EncodeWithHot(nil, orig, tbl, hot))

	// The hot header must be transparent to Decode.
	got, err := Decode(enc, tbl)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !toClassAd(orig).Equal(toClassAd(got)) {
		t.Fatalf("round-trip mismatch: %s vs %s", orig.String(), got.String())
	}

	// Every attribute resolves to the right node — hot ones via the O(1) header,
	// non-hot ones via the linear scan.
	byName := map[string]ast.Expr{}
	for _, a := range orig.Attributes {
		byName[a.Name] = a.Value
	}
	for _, n := range []string{"Owner", "Cpus", "Memory", "Rank", "Arch", "Extra", "Deep"} {
		id, ok := tbl.LookupID(n)
		if !ok {
			t.Fatalf("%s not interned", n)
		}
		node, ok := enc.Lookup(id)
		if !ok {
			t.Fatalf("%s not found", n)
		}
		e, err := DecodeNode(node, tbl)
		if err != nil {
			t.Fatalf("%s decode: %v", n, err)
		}
		if e.String() != byName[n].String() {
			t.Errorf("%s = %s, want %s", n, e.String(), byName[n].String())
		}
	}
	if _, ok := enc.Lookup(99999); ok {
		t.Error("absent id reported found")
	}
}
