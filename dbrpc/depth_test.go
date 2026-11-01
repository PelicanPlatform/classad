package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// chainOld is an old-ClassAd ad whose Requirements is an n-term `||` chain, which reaches depth n-1:
// wire.MaxDepth+1 terms is the longest an attribute may hold.
func chainOld(n int) string { return "Id = 1\nRequirements = " + chainExpr(n) + "\n" }

func chainExpr(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString(" || ")
		}
		fmt.Fprintf(&b, "a%d", i)
	}
	return b.String()
}

const (
	limitTerms = wire.MaxDepth + 1
	overTerms  = limitTerms + 1
)

// mustRefusedTooDeep fails unless err is the server's too-deep refusal: a deterministic
// *ServerError that errors.Is ErrTooDeep (the same value as db.ErrTooDeep) and no other refusal.
func mustRefusedTooDeep(t *testing.T, what string, err error) {
	t.Helper()
	var se *ServerError
	if !errors.As(err, &se) || !errors.Is(err, ErrTooDeep) || !errors.Is(err, db.ErrTooDeep) {
		t.Fatalf("%s: err = %v, want a ServerError that is ErrTooDeep", what, err)
	}
	if errors.Is(err, ErrInternal) || errors.Is(err, ErrTableReadOnly) || errors.Is(err, ErrConnClosed) {
		t.Fatalf("%s: a too-deep refusal also matched another class: %v", what, err)
	}
	if !strings.Contains(err.Error(), `"Requirements"`) {
		t.Fatalf("%s: %v does not name the attribute", what, err)
	}
}

// TestTooDeepRefusalOverRPC: every write a client can send refuses a too-deep value as ErrTooDeep --
// at the op, before anything is buffered, so the rest of the transaction commits -- and nothing else
// is classified as one.
func TestTooDeepRefusalOverRPC(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	ctx := context.Background()

	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustRefusedTooDeep(t, "NewClassAd", tx.NewClassAd(ctx, "deep", chainOld(overTerms)))
	if err = tx.NewClassAd(ctx, "ok", chainOld(limitTerms)); err != nil {
		t.Fatalf("NewClassAd at the limit: %v", err)
	}
	mustRefusedTooDeep(t, "SetAttribute", tx.SetAttribute(ctx, "ok", "Requirements", chainExpr(overTerms)))
	rejects, err := tx.NewClassAdBatch(ctx, []AdKV{{"b0", chainOld(2)}, {"b1", chainOld(overTerms)}, {"b2", "not = = an ad"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejects) != 2 || rejects[0].Index != 1 || rejects[1].Index != 2 {
		t.Fatalf("batch rejects = %+v, want indexes 1 and 2", rejects)
	}
	mustRefusedTooDeep(t, "NewClassAdBatch reject", rejects[0].Cause())
	if errors.Is(rejects[1].Cause(), ErrTooDeep) {
		t.Fatalf("an unparseable ad's reject is classified too-deep: %v", rejects[1].Err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit after the refusals: %v", err)
	}
	for key, want := range map[string]bool{"ok": true, "b0": true, "deep": false, "b1": false} {
		tx, _ := c.Begin(ctx)
		v, ok, err := tx.LookupAttr(ctx, key, "Requirements")
		_ = tx.Abort(ctx)
		if err != nil || ok != want {
			t.Fatalf("%s: present=%v err=%v, want present=%v", key, ok, err, want)
		}
		if key == "ok" && !strings.HasSuffix(v, fmt.Sprintf("a%d)", limitTerms-1)) {
			t.Fatalf("ok: a refused SetAttribute changed Requirements")
		}
	}

	c, cleanupCat := catServerPair(t, ServeOptions{})
	defer cleanupCat()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{}); err != nil {
		t.Fatal(err)
	}
	mustRefusedTooDeep(t, "ArchiveAppend", c.ArchiveAppend(ctx, "history", chainOld(overTerms)))
	if err := c.ArchiveAppend(ctx, "history", chainOld(limitTerms)); err != nil {
		t.Fatalf("ArchiveAppend at the limit: %v", err)
	}

	// An ordinary server error is not mistaken for one.
	if err := c.ArchiveAppend(ctx, "nosuch", chainOld(2)); err == nil || errors.Is(err, ErrTooDeep) {
		t.Fatalf("ArchiveAppend to a missing table: err = %v", err)
	}
}

// TestCommitTooDeepRefusalTagged: the error Commit returns for a whole-ad write it refused -- one the
// ops above never let through, but an in-process writer to the same store can buffer -- is tagged so
// a client classifies it as ErrTooDeep, and an unapplied write for another reason is not.
func TestCommitTooDeepRefusalTagged(t *testing.T) {
	de := &wire.DepthError{Attr: "Requirements", Depth: overTerms - 1}
	err := &db.UnappliedError{Keys: []string{"k"}, Reasons: []string{"too-deep: " + de.Error()}, Errs: []error{de}}
	se := &ServerError{Msg: refusalMsg(err)}
	mustRefusedTooDeep(t, "Commit", se)
	plain := &db.UnappliedError{Keys: []string{"k"}, Reasons: []string{"delta-no-base"}, Errs: []error{nil}}
	if errors.Is(&ServerError{Msg: refusalMsg(plain)}, ErrTooDeep) {
		t.Fatal("an unapplied write with another reason is classified too-deep")
	}
}
