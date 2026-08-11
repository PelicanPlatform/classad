package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// vecExprs are the expressions the vectorized scan is expected to SERVE, spanning the kernels that have
// fast paths (comparison, +-*, logical combines, unary) and ones that route to the parity hook per
// element (division, modulo, identity), so a regression in either shows up here.
var vecExprs = []string{
	"RequestMemory > 4096",
	"RequestMemory <= 4096",
	"RequestCpus == 4",
	"JobStatus != 1",
	"RequestMemory > 4096 && RequestCpus >= 4",
	"RequestMemory > 4096 || RequestCpus >= 4",
	"JobStatus == 1 || JobStatus == 4",
	"RequestMemory > 4096 && RequestCpus >= 4 && JobStatus == 1",
	"RequestMemory > 4096 && (JobStatus == 1 || JobStatus == 4)",
	"RequestMemory > RequestCpus * 512",
	"ProcId + ClusterId > 100",
	"RequestMemory - 1024 > RequestCpus * 256",
	"RequestMemory / 1024 > 4", // division: hook per element
	"ProcId % 3 == 0",          // modulo: hook per element
	"RequestMemory =?= 4096",   // identity: hook per element
	"!(JobStatus == 1)",
	"-ProcId < 0",
	"WantCheckpoint",
	"WantCheckpoint && RequestCpus > 2",
	"WantCheckpoint || JobStatus == 4",
	"!WantCheckpoint",
	"RequestMemory > 4096 && WantCheckpoint",
	"true",
	"ProcId >= 0 && ProcId < 10",
	// strings: the shape colScope already served zero-copy, now a column at a time
	`Owner == "user3"`,
	`Owner == "USER3"`, // == folds case
	`Owner != "user3"`,
	`Owner =?= "user3"`,
	`Owner =!= "USER3"`, // =?= / =!= do not fold
	`Owner < "user5"`,
	`Owner == "user3" || Owner == "user10"`,
	`Owner == "user3" && RequestCpus > 2`,
	`Cmd == "/home/user3/run.sh"`,
	`Owner == "user3" || RequestMemory > 8192`,
	// Results that are NOT boolean. A constraint whose value is a number never matches -- the store takes a
	// boolean or nothing -- which is a different rule from the truthiness a logical OPERAND gets, and the
	// two live in different places now (Vec.IsTrue vs dataStateToMask). These pin both.
	"RequestMemory",  // data-form result: no match, ever
	"ProcId + 1",     // likewise
	"!RequestMemory", // truthy operand, boolean result: 42 is true, so this is false
	"!ProcId",        // ProcId is 0 for a tenth of records, where !0 is TRUE
	"WantCheckpoint", // a bool column, delivered as a mask with no data form in between
	"!WantCheckpoint && RequestCpus > 1",
	"RequestMemory > 0 && WantCheckpoint",
}

// rowCount is the independent reference: the ordinary row path, which decodes ads and evaluates the
// expression the ordinary way.
func rowCount(tb testing.TB, c *Collection, expr string) int {
	tb.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		tb.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}

// TestVectorEvalCountAgrees is the correctness gate: for every expression, the vectorized count equals
// the row path's count, AND the vectorized path actually served the blocks.
//
// The second half matters as much as the first. A scan that declined every block would agree perfectly
// -- because it would be colScope -- and prove nothing about the vector executor.
func TestVectorEvalCountAgrees(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()

	for _, expr := range vecExprs {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		got, ok := c.VectorEvalCount(q)
		if !ok {
			t.Fatalf("%q: no columnar state", expr)
		}
		if want := rowCount(t, c, expr); got != want {
			t.Errorf("%q: vectorized %d != row %d", expr, got, want)
		}
		// Every block must be accounted for, and at least one must have been vectorized OR pruned. A
		// fully pruned query is a legitimate outcome -- `!RequestMemory` is prunable because the fixture
		// never stores a zero there -- but "nothing was vectorized and nothing was pruned" means the
		// comparison ran entirely on the fallback and proves nothing about this executor.
		split := lastVecSplit.Load()
		if split.vecBlocks == 0 && split.prunedBlocks == 0 {
			t.Errorf("%q: nothing vectorized and nothing pruned (%d declined, %d churn, %d empty, %d row "+
				"windows); the comparison is vacuous", expr, split.scopeBlocks, split.churnBlocks,
				split.emptyBlocks, split.rowWindows)
		}
		if split.scopeBlocks != 0 {
			t.Logf("%q: %d/%d blocks vectorized, %d declined",
				expr, split.vecBlocks, split.vecBlocks+split.scopeBlocks, split.scopeBlocks)
		}
	}
}

// TestVectorEvalDeclinesGracefully checks the per-block fallback is a slope, not a cliff: an expression
// the executor cannot vectorize at all must still produce the right answer, via colScope.
func TestVectorEvalDeclinesGracefully(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()

	for _, expr := range []string{
		`RequestMemory > 4096 ?: false`, // Elvis
		`size(Args) > 0`,                // delegated subtree
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		got, ok := c.VectorEvalCount(q)
		if !ok {
			continue // a non-native query is legitimately refused outright
		}
		if want := rowCount(t, c, expr); got != want {
			t.Errorf("%q: %d != row %d", expr, got, want)
		}
		if split := lastVecSplit.Load(); split.vecBlocks != 0 {
			t.Logf("%q: unexpectedly vectorized %d blocks", expr, split.vecBlocks)
		}
	}
}

// TestVectorEvalSeesUpdates checks the vector path honours snapshot visibility -- it evaluates every
// record in a block including superseded ones, and must not count them.
func TestVectorEvalSeesUpdates(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()

	q, err := vm.Parse("RequestMemory > 4096")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := c.VectorEvalCount(q)
	if before != rowCount(t, c, "RequestMemory > 4096") {
		t.Fatal("baseline disagrees")
	}
	// Rewrite 500 records to a value that fails the predicate.
	for i := 0; i < 500; i++ {
		src := "ClusterId = 1\nProcId = 0\nJobStatus = 1\nRequestMemory = 1\nRequestCpus = 1\n" +
			"Owner = \"user0\"\nCmd = \"/x\"\nWantCheckpoint = false\nArgs = \"\"\nIwd = \"/x\""
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := c.VectorEvalCount(q)
	if !ok {
		t.Fatal("declined after update")
	}
	if want := rowCount(t, c, "RequestMemory > 4096"); got != want {
		t.Errorf("after update: vectorized %d != row %d", got, want)
	}
	if got >= before {
		t.Errorf("count did not drop after superseding 500 matching records: %d -> %d", before, got)
	}
}

// BenchmarkVectorEval sizes the win against every path that can serve the same expression: the
// per-record columnar evaluator (colScope), the hand-written column scan where its shape applies, and
// the row scan.
func BenchmarkVectorEval(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()

	for _, expr := range []string{
		"RequestMemory > 4096",
		"RequestMemory > 4096 && RequestCpus >= 4",
		"RequestMemory > RequestCpus * 512",
		"RequestMemory > 4096 && (JobStatus == 1 || JobStatus == 4)",
		"WantCheckpoint && RequestCpus > 2",
		"ProcId + ClusterId > 100",
		`Owner == "user3"`,
		`Owner == "user3" || Owner == "user10"`,
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		// Assert the vectorized path serves this shape before timing it.
		if _, ok := c.VectorEvalCount(q); !ok {
			b.Fatalf("%q: declined", expr)
		}
		if split := lastVecSplit.Load(); split.vecBlocks == 0 {
			b.Fatalf("%q: nothing vectorized; the timing would be colScope's", expr)
		}
		b.Run("vector/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.VectorEvalCount(q)
			}
		})
		b.Run("colScope/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.ColumnarEvalCount(q)
			}
		})
		b.Run("handwritten/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.CountQuery(q)
			}
		})
		b.Run("rowScan/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				n := 0
				for range c.Query(q) {
					n++
				}
			}
		})
	}
}

// TestVectorEvalChurnGuard checks the mostly-superseded guard: the vectorized executor evaluates every
// record in a block including ones no snapshot can see, so a block that is almost entirely superseded
// must be handed to the per-record path instead of vectorized for nothing.
func TestVectorEvalChurnGuard(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()

	q, err := vm.Parse("RequestMemory > RequestCpus * 512")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.VectorEvalCount(q); !ok {
		t.Fatal("declined")
	}
	if split := lastVecSplit.Load(); split.churnBlocks != 0 {
		t.Fatalf("a freshly built fixture should have no churned blocks, got %d", split.churnBlocks)
	}
	// Supersede nearly everything, then rebuild so the sealed blocks hold mostly-dead records.
	for i := 0; i < 19000; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = 0\nJobStatus = 1\nRequestMemory = 8192\n"+
			"RequestCpus = 1\nOwner = \"u\"\nCmd = \"/x\"\nWantCheckpoint = false\nArgs = \"\"\nIwd = \"/x\"", i)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := c.VectorEvalCount(q)
	if !ok {
		t.Fatal("declined after churn")
	}
	if want := rowCount(t, c, "RequestMemory > RequestCpus * 512"); got != want {
		t.Errorf("after churn: %d != row %d", got, want)
	}
	split := lastVecSplit.Load()
	t.Logf("after superseding 19000/20000: %d vectorized, %d churn-guarded, %d declined, %d row windows",
		split.vecBlocks, split.churnBlocks, split.scopeBlocks, split.rowWindows)
	if split.churnBlocks == 0 {
		t.Error("no block tripped the churn guard; either the guard is dead or the fixture did not churn")
	}
}
