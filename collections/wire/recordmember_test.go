package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// recordNodeOf encodes ad in both key encodings and returns the node stored under attr,
// together with the table the id-keyed form was interned with -- which is the only table
// whose ids mean anything to RecordMemberID on that node.
func recordNodeOf(t *testing.T, src, attr string) (idKeyed, inlineKeyed []byte, table *InternTable) {
	t.Helper()
	ad, err := classad.Parse(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	table = NewInternTable()
	a := Ad(Encode(nil, ad.AST(), table))
	id, ok := table.LookupID(attr)
	if !ok {
		t.Fatalf("%s not interned", attr)
	}
	idKeyed, ok = a.Lookup(id)
	if !ok {
		t.Fatalf("%s absent from the id-keyed ad", attr)
	}
	inlineKeyed, ok = Ad(EncodeInline(nil, ad.AST())).LookupByName(attr)
	if !ok {
		t.Fatalf("%s absent from the inline-keyed ad", attr)
	}
	return idKeyed, inlineKeyed, table
}

func TestRecordMemberLookup(t *testing.T) {
	// Last comes AFTER the nested record, so finding it proves the walk skipped Deep in the
	// right key encoding: a nested record's members are keyed like the enclosing ad's, and
	// reading them the other way mis-measures the node and loses everything past it.
	const src = `[ Res = [ Mem = 4096; tag = "a"; Deep = [ X = 7 ]; Last = 9 ] ]`
	idRec, inlineRec, table := recordNodeOf(t, src, "Res")

	memID, interned := table.LookupID("Mem")
	if !interned {
		t.Fatal("Mem not interned")
	}
	if n, ok := RecordMemberID(idRec, memID); !ok {
		t.Error("RecordMemberID did not find Mem")
	} else if lit, ok := LiteralValue(n); !ok || lit.Kind != LitInt || lit.Int != 4096 {
		t.Errorf("RecordMemberID(Mem) = %+v", lit)
	}
	// Member names compare case-insensitively, as attribute names do.
	if n, ok := RecordMemberName(inlineRec, "TAG"); !ok {
		t.Error("RecordMemberName did not find tag under another casing")
	} else if lit, ok := LiteralValue(n); !ok || lit.Kind != LitString || lit.Str != "a" {
		t.Errorf("RecordMemberName(TAG) = %+v", lit)
	}
	// A member that is itself a record comes back whole, so a path walk can descend it.
	deepID, _ := table.LookupID("Deep")
	deep, found := RecordMemberID(idRec, deepID)
	if !found || ClassifyNode(deep) != ClassRecord {
		t.Fatalf("RecordMemberID(Deep) ok=%v class=%v", found, ClassifyNode(deep))
	}
	xID, _ := table.LookupID("X")
	if n, ok := RecordMemberID(deep, xID); !ok {
		t.Error("the second level did not resolve")
	} else if lit, _ := LiteralValue(n); lit.Int != 7 {
		t.Errorf("Deep.X = %+v", lit)
	}
	if n, ok := RecordMemberName(inlineRec, "Last"); !ok {
		t.Error("RecordMemberName did not reach the member after the nested record")
	} else if lit, _ := LiteralValue(n); lit.Int != 9 {
		t.Errorf("Last = %+v", lit)
	}
	lastID, _ := table.LookupID("Last")
	if n, ok := RecordMemberID(idRec, lastID); !ok {
		t.Error("RecordMemberID did not reach the member after the nested record")
	} else if lit, _ := LiteralValue(n); lit.Int != 9 {
		t.Errorf("Last = %+v", lit)
	}
	if _, ok := RecordMemberName(inlineRec, "Nope"); ok {
		t.Error("an absent member was found")
	}
}

// TestRecordMemberMalformed: the node comes off the wire, so every refusal has to be a
// (nil, false) rather than a panic or a slice out of the buffer.
func TestRecordMemberMalformed(t *testing.T) {
	const src = `[ Res = [ Mem = 4096; Pad = "0123456789" ] ]`
	idRec, inlineRec, table := recordNodeOf(t, src, "Res")
	// The ids actually in the record, so a truncation that cuts one of them in half is a
	// MATCH on the way out -- which is where a missing bounds check returns half a node.
	var ids []uint32
	for _, name := range []string{"Mem", "Pad", "Res"} {
		if id, ok := table.LookupID(name); ok {
			ids = append(ids, id)
		}
	}
	for _, b := range [][]byte{nil, {}, {0xFF}, []byte("not a node")} {
		if _, ok := RecordMemberID(b, 1); ok {
			t.Errorf("RecordMemberID accepted %x", b)
		}
		if _, ok := RecordMemberName(b, "Mem"); ok {
			t.Errorf("RecordMemberName accepted %x", b)
		}
	}
	// Every truncation of a well-formed record node: none may panic, and whatever a
	// truncation DOES return must be a complete node. A member whose bytes all survived the
	// cut is a legitimate hit; half of one is a value the caller would go on to decode.
	whole := func(what string, n []byte, ok bool) {
		t.Helper()
		if !ok {
			return
		}
		if l, good := NodeLen(n); !good || l != len(n) {
			t.Fatalf("%s returned a %d-byte node measuring %d (ok=%v)", what, len(n), l, good)
		}
	}
	for i := 0; i < len(idRec); i++ {
		for _, id := range ids {
			n, ok := RecordMemberID(idRec[:i], id)
			whole("RecordMemberID", n, ok)
		}
	}
	for i := 0; i < len(inlineRec); i++ {
		for _, name := range []string{"Mem", "Pad"} {
			n, ok := RecordMemberName(inlineRec[:i], name)
			if ok {
				if l, good := NodeLenKeyed(n, true); !good || l != len(n) {
					t.Fatalf("RecordMemberName returned a %d-byte node measuring %d (ok=%v)", len(n), l, good)
				}
			}
		}
	}
	// A node that is not a record literal has no members, whatever its bytes happen to look
	// like when read as a body.
	for _, other := range []string{
		`[ Res = 5 ]`, `[ Res = "0123456789" ]`, `[ Res = true ]`,
		`[ Res = { [ Mem = 4096 ], [ Mem = 1 ] } ]`, `[ Res = Mem + 1 ]`,
	} {
		node, inlineNode, _ := recordNodeOf(t, other, "Res")
		for id := uint32(0); id < 12; id++ {
			if _, ok := RecordMemberID(node, id); ok {
				t.Errorf("%s: RecordMemberID read a member out of a non-record node", other)
			}
		}
		for _, name := range []string{"Mem", "Res", "Pad", "X"} {
			if _, ok := RecordMemberName(inlineNode, name); ok {
				t.Errorf("%s: RecordMemberName read %q out of a non-record node", other, name)
			}
		}
	}
}
