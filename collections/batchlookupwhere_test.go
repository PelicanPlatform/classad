package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// eqInt is `attr == n` as an AST node, which is what LookupKeysWhere takes.
func eqInt(attr string, n int64) ast.Expr {
	return &ast.BinaryOp{
		Op:    "==",
		Left:  ast.NewAttributeReference(attr, ast.NoScope),
		Right: &ast.IntegerLiteral{Value: n},
	}
}

// eqStr is `attr == "s"`.
func eqStr(attr, s string) ast.Expr {
	return &ast.BinaryOp{
		Op:    "==",
		Left:  ast.NewAttributeReference(attr, ast.NoScope),
		Right: &ast.StringLiteral{Value: s},
	}
}

// whereArm is one constraint and the predicate that decides, in the test, which records it
// admits. The predicate is written out rather than evaluated through the VM so that the
// expectation is independent of the thing under test: deriving it from the same evaluator
// would let a constraint that silently admits everything agree with itself.
type whereArm struct {
	name  string
	where ast.Expr
	keep  func(ad *classad.ClassAd) bool
}

// whereArms covers the three ways a constraint relates to the archive's indexes, because that
// is what decides whether the candidate intersection can narrow on it: ProcId is a ValueAttr,
// Owner is a CategoricalAttr, RunInstanceId is indexed not at all and can only be matched
// record by record. All three must return the same rows; only their cost differs.
func whereArms() []whereArm {
	return []whereArm{
		{"value-indexed", eqInt("ProcId", 0), func(ad *classad.ClassAd) bool {
			v, ok := ad.EvaluateAttrInt("ProcId")
			return ok && v == 0
		}},
		{"categorical", eqStr("Owner", "u5"), func(ad *classad.ClassAd) bool {
			v, ok := ad.EvaluateAttrString("Owner")
			return ok && v == "u5"
		}},
		{"unindexed", eqInt("RunInstanceId", 1), func(ad *classad.ClassAd) bool {
			v, ok := ad.EvaluateAttrInt("RunInstanceId")
			return ok && v == 1
		}},
	}
}

// TestLookupKeysWhereIsLookupKeysFiltered is the correctness bar: the narrowed lookup returns
// exactly the records the plain lookup returns that satisfy the constraint, and no others.
//
// Checked against the PLAIN LOOKUP's output rather than against a count, because the failure
// this guards is a constraint applied to the wrong record set -- a batch whose span or
// candidate intersection dropped records before the constraint ever saw them. That reads as a
// smaller, entirely self-consistent answer.
func TestLookupKeysWhereIsLookupKeysFiltered(t *testing.T) {
	f := buildBatchFixture(t)
	for _, arm := range whereArms() {
		t.Run(arm.name, func(t *testing.T) {
			for _, n := range []int{8, 64, 256, 1024} {
				keys := batchKeysAt(0, n)
				var want []batchRow
				for ad := range f.a.LookupKeys("ClusterId", keys) {
					if arm.keep(ad) {
						want = append(want, batchRowOf(t, ad))
					}
				}
				if len(want) == 0 {
					t.Fatalf("n=%d: the constraint admits nothing, so the arm proves nothing", n)
				}
				got := collectBatch(t, f.a.LookupKeysWhere("ClusterId", keys, arm.where))
				sortRows(want)
				sortRows(got)
				if len(got) != len(want) {
					t.Fatalf("n=%d: narrowed lookup returned %d records, the plain lookup "+
						"filtered returns %d", n, len(got), len(want))
				}
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("n=%d: record %d is %+v, want %+v", n, i, got[i], want[i])
					}
				}
			}
		})
	}
}

// TestLookupKeysWhereKeepsTheBatchedAccessPath is the whole reason this entry point exists.
//
// A caller could conjoin the constraint itself and call Query, and would get the right
// records. What it would lose is the access path: that read is planned through planRead,
// whose collection-wide gate is the one this file exists to get past, so a join narrowing its
// inner side would surrender the batched path precisely by asking for less data.
//
// The property is that carrying the constraint INTO the batch costs nothing in segments: the
// span hoist prunes the same segments it prunes without one, and the index still answers the
// survivors. It must never cost MORE records than the unnarrowed lookup either -- a
// constraint that made the scan read more than it reads without it would be a pessimization
// wearing a filter's clothes.
func TestLookupKeysWhereKeepsTheBatchedAccessPath(t *testing.T) {
	f := buildBatchFixture(t)
	keys := batchKeysAt(0, 1024)
	var plain ScanStats
	plainRows := collectBatch(t, f.a.LookupKeysStats("ClusterId", keys, &plain))
	if plain.SegmentsPruned == 0 || plain.SegmentsIndexed == 0 {
		t.Fatalf("the plain lookup pruned %d segments and indexed %d; without both, the "+
			"comparison below cannot show the constraint preserved anything",
			plain.SegmentsPruned, plain.SegmentsIndexed)
	}
	for _, arm := range whereArms() {
		t.Run(arm.name, func(t *testing.T) {
			var st ScanStats
			rows := collectBatch(t, f.a.LookupKeysWhereStats("ClusterId", keys, arm.where, &st))
			if len(rows) >= len(plainRows) {
				t.Fatalf("the constraint admitted %d of %d records; it is not narrowing, so "+
					"the access path below is not being tested under one",
					len(rows), len(plainRows))
			}
			if st.SegmentsPruned != plain.SegmentsPruned {
				t.Errorf("pruned %d segments with the constraint and %d without; the span "+
					"hoist is not surviving it", st.SegmentsPruned, plain.SegmentsPruned)
			}
			if st.SegmentsIndexed == 0 {
				t.Errorf("no segment was answered from its index (%d walked unindexed); the "+
					"constraint cost the batched access path", st.SegmentsUnindexed)
			}
			if st.RecordsVisited > plain.RecordsVisited {
				t.Errorf("visited %d records with the constraint and %d without",
					st.RecordsVisited, plain.RecordsVisited)
			}
		})
	}
}

// TestLookupKeysWhereNilIsLookupKeys pins the documented equivalence, so a caller with no
// constraint need not hold a second code path for it.
func TestLookupKeysWhereNilIsLookupKeys(t *testing.T) {
	f := buildBatchFixture(t)
	keys := batchKeysAt(0, 64)
	want := collectBatch(t, f.a.LookupKeys("ClusterId", keys))
	got := collectBatch(t, f.a.LookupKeysWhere("ClusterId", keys, nil))
	if len(want) == 0 {
		t.Fatal("the batch matched nothing, so the comparison is vacuous")
	}
	if len(got) != len(want) {
		t.Fatalf("a nil constraint returned %d records, LookupKeys %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("record %d is %+v, want %+v (order must match too)", i, got[i], want[i])
		}
	}
}

// TestLookupKeysWhereAdmitsNothingWhenTheConstraintDoes checks the empty answer is empty for
// the right reason: the records are reached and rejected, not missed.
func TestLookupKeysWhereAdmitsNothingWhenTheConstraintDoes(t *testing.T) {
	f := buildBatchFixture(t)
	keys := batchKeysAt(0, 64)
	var st ScanStats
	got := collectBatch(t, f.a.LookupKeysWhereStats("ClusterId", keys,
		eqInt("ProcId", 1<<40), &st))
	if len(got) != 0 {
		t.Fatalf("a constraint no record satisfies returned %d records", len(got))
	}
	if st.SegmentsPruned == 0 {
		t.Errorf("no segment was pruned, so the empty answer is not the span hoist's")
	}
}

// TestBatchKeyQueryWhereConjoins pins the query the narrowed lookup plans from, since a
// caller explaining a batch has to be able to build the same one.
//
// Checked through the attributes the query READS rather than through its text: the text is
// the compiler's business and would pin a spelling, while the read set is what the planner
// and the demand recorder act on, and a conjunct that never reached the program reads
// nothing.
func TestBatchKeyQueryWhereConjoins(t *testing.T) {
	keys := []int64{1, 2}
	reads := func(q interface{ ReadAttrs() []string }) map[string]bool {
		out := map[string]bool{}
		for _, a := range q.ReadAttrs() {
			out[a] = true
		}
		return out
	}
	plain := reads(BatchKeyQuery("ClusterId", keys))
	if !plain["ClusterId"] {
		t.Fatalf("the plain batch query reads %v, not the key", plain)
	}
	if plain["ProcId"] {
		t.Fatalf("the plain batch query already reads ProcId: %v", plain)
	}
	nilWhere := reads(BatchKeyQueryWhere("ClusterId", keys, nil))
	if len(nilWhere) != len(plain) || !nilWhere["ClusterId"] {
		t.Errorf("a nil constraint reads %v, BatchKeyQuery reads %v", nilWhere, plain)
	}
	withWhere := reads(BatchKeyQueryWhere("ClusterId", keys, eqInt("ProcId", 0)))
	if !withWhere["ProcId"] {
		t.Errorf("the constraint did not reach the query: it reads %v", withWhere)
	}
	if !withWhere["ClusterId"] {
		t.Errorf("the conjoined query stopped reading the key: %v", withWhere)
	}
}
