package dbrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// seedExprRow stores an ad whose Req attribute is an expression over a sibling, the shape
// HTCondor data takes (Requirements, Rank, WithinResourceLimits, ...).
func seedExprRow(t *testing.T, c *Client, key string, memory int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ad := "Memory = " + itoaTest(memory) + "\nReq = Memory > 1024\n"
	if err := tx.NewClassAd(ctx, key, ad); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// evalReq parses a returned ad and evaluates its Req attribute, the way any client that
// projects and then reads a value must.
func evalReq(t *testing.T, adText string) classad.Value {
	t.Helper()
	ad, err := classad.ParseOld(adText)
	if err != nil {
		t.Fatalf("parsing %q: %v", adText, err)
	}
	return ad.EvaluateAttr("Req")
}

// Projecting to exactly the requested attributes drops the sibling an expression
// attribute reads, so it evaluates to undefined at the far end. This is the existing
// contract -- correct for a relay reproducing HTCondor's query protocol -- and it is
// pinned so the new op's behaviour reads as a deliberate difference rather than a change.
func TestQueryRawProjectDropsExpressionDependencies(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedExprRow(t, c, "a", 2048)

	rows, err := c.QueryRawProject(context.Background(), DefaultTable, "true", []string{"Req"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// Not a substring check: the expression TEXT mentions Memory. What must be absent is
	// a Memory attribute of its own, which is what evaluation would need.
	ad, err := classad.ParseOld(rows[0])
	if err != nil {
		t.Fatalf("parsing %q: %v", rows[0], err)
	}
	if _, ok := ad.Lookup("Memory"); ok {
		t.Errorf("plain projection carried a Memory attribute; it should send exactly what was asked for: %q", rows[0])
	}
	if v := evalReq(t, rows[0]); !v.IsUndefined() {
		t.Errorf("Req evaluated to %v, want undefined (the sibling was projected away)", v)
	}
}

// The refs-chasing op carries the referenced sibling, so the same projection evaluates to
// the answer SELECT * would give.
func TestQueryRawProjectRefsKeepsExpressionDependencies(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedExprRow(t, c, "a", 2048)

	rows, err := c.QueryRawProjectRefs(context.Background(), DefaultTable, "true", []string{"Req"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	v := evalReq(t, rows[0])
	b, err := v.BoolValue()
	if err != nil || !b {
		t.Errorf("Req evaluated to %v, want true (the referenced sibling should have come along)", v)
	}
}

// The chased result must track the data, not a constant: a row whose sibling fails the
// test evaluates false.
func TestQueryRawProjectRefsEvaluatesPerRow(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedExprRow(t, c, "big", 2048)
	seedExprRow(t, c, "small", 512)

	rows, err := c.QueryRawProjectRefs(context.Background(), DefaultTable, "true", []string{"Req"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	var sawTrue, sawFalse bool
	for _, row := range rows {
		b, err := evalReq(t, row).BoolValue()
		if err != nil {
			t.Fatalf("Req did not evaluate to a bool in %q", row)
		}
		sawTrue = sawTrue || b
		sawFalse = sawFalse || !b
	}
	if !sawTrue || !sawFalse {
		t.Errorf("expected one true and one false row; sawTrue=%v sawFalse=%v", sawTrue, sawFalse)
	}
}

// A projection of plain literal attributes behaves identically under both ops -- chasing
// references costs nothing when there are none to chase.
func TestQueryRawProjectRefsMatchesPlainForLiterals(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, "a", "Owner = \"alice\"\nMemory = 2048\n"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	plain, err := c.QueryRawProject(ctx, DefaultTable, "true", []string{"Owner"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	chased, err := c.QueryRawProjectRefs(ctx, DefaultTable, "true", []string{"Owner"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 || len(chased) != 1 {
		t.Fatalf("got %d/%d rows, want 1/1", len(plain), len(chased))
	}
	if plain[0] != chased[0] {
		t.Errorf("literal projection differs:\n plain  = %q\n chased = %q", plain[0], chased[0])
	}
}

func TestQueryRawProjectRefsLimitAndStream(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		seedExprRow(t, c, k, 2048)
	}

	rows, err := c.QueryRawProjectRefs(ctx, DefaultTable, "true", []string{"Req"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 (LIMIT was not applied)", len(rows))
	}

	n := 0
	err = c.QueryRawProjectRefsStream(ctx, DefaultTable, "true", []string{"Req"}, 0, func(string) bool {
		n++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("streamed %d rows, want 3", n)
	}
}

// A server that does not implement the opcode is reported as such, so a caller can fall
// back to the plain projection rather than treating it as a query failure.
func TestProjectRefsUnsupportedSentinel(t *testing.T) {
	if got := projectRefsErr(ErrBadRequest); !errors.Is(got, ErrProjectRefsUnsupported) {
		t.Errorf("projectRefsErr(ErrBadRequest) = %v, want ErrProjectRefsUnsupported", got)
	}
	if got := projectRefsErr(nil); got != nil {
		t.Errorf("projectRefsErr(nil) = %v, want nil", got)
	}
	other := errors.New("some other failure")
	if got := projectRefsErr(other); !errors.Is(got, other) {
		t.Errorf("projectRefsErr passed through the wrong error: %v", got)
	}
}
