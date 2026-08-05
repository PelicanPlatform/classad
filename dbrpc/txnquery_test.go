package dbrpc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// adText builds the old-ClassAd text one row, so tests can write through Tx.NewClassAd.
func adText(owner string, cpus int) string {
	return "Owner = \"" + owner + "\"\nCpus = " + itoa(cpus) + "\n"
}

func itoa(n int) string {
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

// ownersOf pulls the Owner value out of each returned ad's text, sorted. The server
// renders a query result in new-ClassAd form -- one bracketed line, attributes separated
// by ';' -- so split on both that and newlines rather than assuming either.
func ownersOf(rows []string) []string {
	var out []string
	for _, row := range rows {
		body := strings.Trim(strings.TrimSpace(row), "[]")
		for _, field := range strings.FieldsFunc(body, func(r rune) bool { return r == ';' || r == '\n' }) {
			if name, value, ok := strings.Cut(field, "="); ok && strings.TrimSpace(name) == "Owner" {
				out = append(out, strings.Trim(strings.TrimSpace(value), "\""))
			}
		}
	}
	slices.Sort(out)
	return out
}

// A transaction's own uncommitted writes are visible to Tx.Query and invisible to
// Client.Query -- the whole point of the transaction-scoped read opcodes.
func TestTxQuerySeesUncommittedWrites(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, "a", adText("alice", 8)); err != nil {
		t.Fatal(err)
	}

	rows, err := tx.Query(ctx, "Cpus >= 4", 0)
	if err != nil {
		t.Fatalf("Tx.Query: %v", err)
	}
	if got, want := ownersOf(rows), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("Tx.Query got %v, want %v", got, want)
	}

	// The committed store still knows nothing about it.
	committed, err := c.Query(ctx, "Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(committed); len(got) != 0 {
		t.Errorf("Client.Query got %v, want no rows before commit", got)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	committed, err = c.Query(ctx, "Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ownersOf(committed), []string{"alice"}; !slices.Equal(got, want) {
		t.Errorf("after commit got %v, want %v", got, want)
	}
}

func TestTxQueryOverlaysDeletesAndRewrites(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	seed, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for key, ad := range map[string]string{"a": adText("alice", 8), "b": adText("bob", 8)} {
		if err := seed.NewClassAd(ctx, key, ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort(ctx)
	if err := tx.DestroyClassAd(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, "c", adText("carol", 8)); err != nil {
		t.Fatal(err)
	}

	rows, err := tx.Query(ctx, "Cpus >= 4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ownersOf(rows), []string{"alice", "carol"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTxQueryLimit(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort(ctx)
	for _, key := range []string{"a", "b", "c", "d"} {
		if err := tx.NewClassAd(ctx, key, adText(key, 8)); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := tx.Query(ctx, "Cpus >= 4", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2 (LIMIT was not applied)", len(rows))
	}
}

// KeysWhere is what lets an UPDATE or DELETE inside a transaction address a row the
// transaction itself created, which the committed-state QueryKeys cannot see.
func TestTxKeysWhereSeesUncommittedWrites(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort(ctx)
	if err := tx.NewClassAd(ctx, "staged", adText("alice", 8)); err != nil {
		t.Fatal(err)
	}

	keys, err := tx.KeysWhere(ctx, "Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"staged"}; !slices.Equal(keys, want) {
		t.Errorf("got %v, want %v", keys, want)
	}

	// The key is addressable within the same transaction: set an attribute on the row
	// the transaction just created and read it back through the transaction.
	if err := tx.SetAttribute(ctx, keys[0], "JobStatus", "2"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "JobStatus == 2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1 (the in-transaction update is not visible)", len(rows))
	}
}

func TestTxQueryBadConstraint(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort(ctx)

	if _, err := tx.Query(ctx, "this is not ( a constraint", 0); err == nil {
		t.Error("a malformed constraint was accepted")
	} else if errors.Is(err, ErrTxnReadUnsupported) {
		t.Errorf("a malformed constraint reported as unsupported: %v", err)
	}
}

// A transaction id the server does not know is an error, not a silent empty result --
// otherwise a client whose transaction was reaped would read an empty table as truth.
func TestTxQueryUnknownTransaction(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	stale := &Tx{c: c, id: 999999}
	if _, err := stale.Query(ctx, "true", 0); err == nil {
		t.Error("querying an unknown transaction succeeded")
	}
	if _, err := stale.KeysWhere(ctx, "true"); err == nil {
		t.Error("KeysWhere on an unknown transaction succeeded")
	}
}

// An aborted transaction's writes are visible to nobody, including a later reader.
func TestTxQueryAfterAbort(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, "a", adText("alice", 8)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Abort(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := c.Query(ctx, "Cpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(rows); len(got) != 0 {
		t.Errorf("got %v after abort, want no rows", got)
	}
}

// stBadReq maps to ErrBadRequest so a client can distinguish "the server does not
// implement this opcode" from a genuine failure without matching message text. This is
// what makes the ErrTxnReadUnsupported fallback possible.
func TestBadRequestIsASentinel(t *testing.T) {
	if !errors.Is(statusErr(stBadReq, &reader{}), ErrBadRequest) {
		t.Error("stBadReq did not map to ErrBadRequest")
	}
	if errors.Is(statusErr(stErr, &reader{b: putStr(nil, "boom")}), ErrBadRequest) {
		t.Error("stErr wrongly mapped to ErrBadRequest")
	}
	if got := txnReadErr(ErrBadRequest); !errors.Is(got, ErrTxnReadUnsupported) {
		t.Errorf("txnReadErr(ErrBadRequest) = %v, want ErrTxnReadUnsupported", got)
	}
	if got := txnReadErr(nil); got != nil {
		t.Errorf("txnReadErr(nil) = %v, want nil", got)
	}
}
