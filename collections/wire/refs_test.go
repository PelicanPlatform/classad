package wire

import (
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// nestedRecordCases are expressions holding record literals in every position a walker has to step
// into: directly, inside a list, inside another record, and with members that reach back out to the
// enclosing ad. refs is the reference set every walker must report -- unscoped and MY. names, nested
// members included (the walkers are supersets, and a member such as Foo below is evaluated in the
// record's scope, which falls through to the enclosing ad). TARGET. references name another ad and
// are never included.
var nestedRecordCases = []struct {
	name string
	expr string
	refs []string
}{
	{"record", `[alpha = Foo; beta = Bar].alpha + Baz`, []string{"Bar", "Baz", "Foo"}},
	{"record in list", `{ 1, [a = Foo; b = Bar], Baz }`, []string{"Bar", "Baz", "Foo"}},
	{"record in record", `[outer = [inner = Foo; z = 1].inner; w = Bar].outer + Baz`, []string{"Bar", "Baz", "Foo"}},
	{"outer references", `[m = MY.Foo; n = TARGET.Qux; o = Bar * 2].m + Baz`, []string{"Bar", "Baz", "Foo"}},
	{"record after refs", `Baz + [k = Foo].k`, []string{"Baz", "Foo"}},
}

// nestedRecordNode encodes [X = expr; Pad = 1] in the given key encoding and returns X's node, with
// the interned table for an id-keyed ad (nil for an inline one).
func nestedRecordNode(t *testing.T, expr string, inline bool) ([]byte, *InternTable) {
	t.Helper()
	ad, err := parser.ParseClassAd(`[X = ` + expr + `; Pad = 1]`)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	if inline {
		node, ok := Ad(EncodeInline(nil, ad)).LookupByName("X")
		if !ok {
			t.Fatal("X not found in the inline ad")
		}
		return node, nil
	}
	tab := NewInternTable()
	enc := Encode(nil, ad, tab)
	id, _ := tab.LookupID("X")
	node, ok := Ad(enc).Lookup(id)
	if !ok {
		t.Fatal("X not found in the interned ad")
	}
	return node, tab
}

func sortedNames(names []string) []string {
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return slices.Compact(names)
}

// TestNodeWalkersNestedRecords holds every walker that steps into a nested record literal to the
// enclosing ad's key encoding, in both encodings. A walker that read an inline-keyed record's
// member names as ids mis-sized the node and lost every reference from the record onward.
func TestNodeWalkersNestedRecords(t *testing.T) {
	for _, tc := range nestedRecordCases {
		for _, inline := range []bool{true, false} {
			mode := "id"
			if inline {
				mode = "inline"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				node, tab := nestedRecordNode(t, tc.expr, inline)

				// The node's length, alone and followed by more bytes (a cold tail is a run of
				// entries split by exactly this measurement).
				if n, ok := NodeLenKeyed(node, inline); !ok || n != len(node) {
					t.Errorf("NodeLenKeyed = (%d, %v), want (%d, true)", n, ok, len(node))
				}
				trailing := append(append([]byte(nil), node...), nInt, 2)
				if n, ok := NodeLenKeyed(trailing, inline); !ok || n != len(node) {
					t.Errorf("NodeLenKeyed with a trailing node = (%d, %v), want (%d, true)", n, ok, len(node))
				}
				if !inline {
					if n, ok := NodeLen(node); !ok || n != len(node) {
						t.Errorf("NodeLen = (%d, %v), want (%d, true)", n, ok, len(node))
					}
				}

				// The lenient walk for this encoding.
				var lenient []string
				if inline {
					for _, b := range AppendNodeRefNames(node, nil) {
						lenient = append(lenient, string(b))
					}
				} else {
					for _, id := range AppendNodeRefIDs(node, nil) {
						nm, ok := tab.Name(id)
						if !ok {
							t.Fatalf("reference id %d does not resolve", id)
						}
						lenient = append(lenient, nm)
					}
				}
				if got := sortedNames(lenient); !slices.Equal(got, tc.refs) {
					t.Errorf("lenient walk = %v, want %v", got, tc.refs)
				}

				// The exact walk must agree with it and report the set complete.
				var r NodeRefs
				if !AppendNodeRefsExact(node, inline, &r, nil) {
					t.Fatal("AppendNodeRefsExact reported an incomplete set")
				}
				var exact []string
				for _, b := range r.Names {
					exact = append(exact, string(b))
				}
				for _, id := range r.IDs {
					nm, _ := tab.Name(id)
					exact = append(exact, nm)
				}
				if got := sortedNames(exact); !slices.Equal(got, tc.refs) {
					t.Errorf("exact walk = %v, want %v", got, tc.refs)
				}
				if inline && len(r.IDs) != 0 || !inline && len(r.Names) != 0 {
					t.Errorf("exact walk mixed key encodings: ids=%v names=%q", r.IDs, r.Names)
				}
			})
		}
	}
}

// TestNodeLenKeyedRejectsWrongEncoding pins that the key encoding is not inferable: an inline
// record measured as id-keyed does not come back as its own length. If it ever did, the walkers
// would not need to be told the encoding, and this test would be the one to say so.
func TestNodeLenKeyedRejectsWrongEncoding(t *testing.T) {
	node, _ := nestedRecordNode(t, `[alpha = Foo; beta = Bar].alpha + Baz`, true)
	if n, ok := NodeLenKeyed(node, false); ok && n == len(node) {
		t.Fatalf("an inline-keyed record measured as id-keyed came back as its own length %d", n)
	}
}
