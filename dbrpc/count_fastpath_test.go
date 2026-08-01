package dbrpc

import (
	"context"
	"fmt"
	"strconv"
	"testing"
)

// TestAggregateCountStarFastPath verifies the unconstrained COUNT(*) fast path (Len(), no
// scan) returns the right count for empty and "true" constraints, that a constrained COUNT(*)
// still takes the scan path and stays correct, and that the fast path reflects deletes.
func TestAggregateCountStarFastPath(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, _ := c.Begin(ctx)
	for i := 0; i < 50; i++ {
		_ = tx.NewClassAd(ctx, strconv.Itoa(i), fmt.Sprintf("Cpus = %d", i))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	count := func(constraint string) string {
		rows, err := c.Aggregate(ctx, constraint, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || len(rows[0].Values) != 1 {
			t.Fatalf("count rows = %+v, want a single one-value row", rows)
		}
		return rows[0].Values[0]
	}

	// Unconstrained COUNT(*): the fast path. Both spellings of match-all.
	if got := count("true"); got != "50" {
		t.Errorf(`COUNT(*) WHERE true = %s, want 50`, got)
	}
	if got := count(""); got != "50" {
		t.Errorf(`COUNT(*) (empty constraint) = %s, want 50`, got)
	}
	// Constrained COUNT(*): the scan path stays correct.
	if got := count("Cpus >= 25"); got != "25" {
		t.Errorf(`COUNT(*) WHERE Cpus >= 25 = %s, want 25`, got)
	}

	// The fast path tracks deletes (Len decrements on delete).
	n, err := c.DeleteWhere(ctx, "Cpus < 10")
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("DeleteWhere removed %d, want 10", n)
	}
	if got := count("true"); got != "40" {
		t.Errorf(`COUNT(*) after deleting 10 = %s, want 40`, got)
	}
}
