package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// copyPair builds a two-table catalog -- a job source and an empty target -- served over a
// pipe, which is the shape a row copy is for.
func copyPair(t *testing.T) (c *Client, src, dst *db.DB, cleanup func()) {
	t.Helper()
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	src, err = cat.CreateTable("history")
	if err != nil {
		t.Fatal(err)
	}
	dst, err = cat.CreateTable("recent")
	if err != nil {
		t.Fatal(err)
	}
	tx := src.Begin()
	for i := 0; i < 6; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"Jid = %d\nOwner = %q\nRequestCpus = %d\nJobStatus = %d",
			i, fmt.Sprintf("u%d", i%2), i+1, 4))
		if perr != nil {
			t.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("job%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c = NewClient(cconn)
	return c, src, dst, func() { c.Close(); s.Close() }
}

func TestInsertRowsCopiesWholeAds(t *testing.T) {
	c, _, dst, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "recent", []string{"Jid"}); err != nil {
		t.Fatal(err)
	}
	n, err := c.InsertRows(ctx, "recent", "history", "true", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("copied %d rows, want 6", n)
	}
	if got := len(dst.Keys()); got != 6 {
		t.Fatalf("target holds %d rows, want 6", got)
	}
}

func TestInsertRowsProjects(t *testing.T) {
	c, _, dst, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "recent", []string{"Id"}); err != nil {
		t.Fatal(err)
	}
	n, err := c.InsertRows(ctx, "recent", "history", `Owner == "u1"`,
		[]string{"Jid", "Owner"}, []string{"Id", "User"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("copied %d rows, want 3", n)
	}
	ad, ok := dst.LookupClassAd("1")
	if !ok {
		t.Fatalf("no row at derived key 1, keys are %v", dst.Keys())
	}
	if ad.Size() != 2 {
		t.Fatalf("row has %d attributes, want the 2 projected", ad.Size())
	}
	if v := ad.EvaluateAttr("User"); !v.IsString() {
		t.Fatalf("User = %v, want a string", v)
	}
}

func TestInsertRowsReRunConverges(t *testing.T) {
	c, _, dst, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "recent", []string{"Jid"}); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := c.InsertRows(ctx, "recent", "history", "true", nil, nil); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if got := len(dst.Keys()); got != 6 {
			t.Fatalf("after run %d the target holds %d rows, want 6", run, got)
		}
	}
}

func TestInsertRowsRefusesSameTable(t *testing.T) {
	c, _, _, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	_, err := c.InsertRows(ctx, "history", "history", "true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "read and write the same table") {
		t.Fatalf("error = %v, want a same-table refusal", err)
	}
}

func TestInsertRowsRefusesNoPrimaryKey(t *testing.T) {
	c, _, _, cleanup := copyPair(t)
	defer cleanup()
	_, err := c.InsertRows(context.Background(), "recent", "history", "true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "declares no primary key") {
		t.Fatalf("error = %v, want a no-primary-key refusal", err)
	}
}

func TestInsertRowsRefusesMissingSource(t *testing.T) {
	c, _, _, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "recent", []string{"Jid"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.InsertRows(ctx, "recent", "nope", "true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("error = %v, want a missing-source refusal", err)
	}
}

func TestInsertRowsRefusesMissingTarget(t *testing.T) {
	c, _, _, cleanup := copyPair(t)
	defer cleanup()
	_, err := c.InsertRows(context.Background(), "nope", "history", "true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not a writable table") {
		t.Fatalf("error = %v, want a non-writable-target refusal", err)
	}
}

func TestInsertRowsRefusesBadConstraint(t *testing.T) {
	c, _, _, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "recent", []string{"Jid"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.InsertRows(ctx, "recent", "history", "Owner ==", nil, nil)
	if err == nil {
		t.Fatal("a malformed constraint was accepted")
	}
}

func TestInsertRowsRefusesCollidingKeys(t *testing.T) {
	c, _, dst, cleanup := copyPair(t)
	defer cleanup()
	ctx := context.Background()
	// Two owners over six jobs: Owner does not identify a row.
	if err := c.SetPrimaryKey(ctx, "recent", []string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.InsertRows(ctx, "recent", "history", "true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "both derive primary key") {
		t.Fatalf("error = %v, want a colliding-key refusal", err)
	}
	if got := len(dst.Keys()); got != 0 {
		t.Fatalf("target holds %d rows after a refused copy, want 0", got)
	}
}

// The archive is a legitimate SOURCE for a copy even though it cannot be a target.
func TestInsertRowsFromArchive(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ Jid = %d; Owner = "u" ]`, i))
		if perr != nil {
			t.Fatal(perr)
		}
		if appendErr := arch.Append(ad); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	dst, err := cat.CreateTable("recent")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	ctx := context.Background()
	if keyErr := c.SetPrimaryKey(ctx, "recent", []string{"Jid"}); keyErr != nil {
		t.Fatal(keyErr)
	}
	n, err := c.InsertRows(ctx, "recent", "history", "true", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("copied %d rows from the archive, want 4", n)
	}
	if got := len(dst.Keys()); got != 4 {
		t.Fatalf("target holds %d rows, want 4", got)
	}
}

// An archive cannot be a target: it is append-only, so it has no derived-key write.
func TestInsertRowsRefusesArchiveTarget(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, archErr := cat.CreateArchiveTable("sealed", db.ArchiveConfig{SegmentSize: 1 << 12}); archErr != nil {
		t.Fatal(archErr)
	}
	if _, tblErr := cat.CreateTable("live"); tblErr != nil {
		t.Fatal(tblErr)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	_, err = c.InsertRows(context.Background(), "sealed", "live", "true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not a writable table") {
		t.Fatalf("error = %v, want an archive-target refusal", err)
	}
}

func TestInsertRowsIsMutatingAndGated(t *testing.T) {
	if !opInsertRows.isMutating() {
		t.Fatal("opInsertRows is not marked mutating, so a read-only peer could run it")
	}
	if got := opInsertRows.String(); got != "InsertRows" {
		t.Fatalf("opInsertRows.String() = %q, want InsertRows", got)
	}
}

// The write gate must name the TARGET, and must not name the source.
//
// Both halves matter and they fail in opposite directions: gating on the source would let a
// peer write a table it was never granted, and failing to gate the target at all would let
// it write an OWNED one. The copy and the roll-up are checked together because they are the
// two opcodes carrying two table names, which is what makes the mistake possible.
func TestInsertWriteGateNamesTarget(t *testing.T) {
	f := newGateFixture(t, daemonOpts)
	ctx := context.Background()
	if err := f.c.SetPrimaryKey(ctx, "free", []string{"N"}); err != nil {
		t.Fatal(err)
	}

	// Target owned: refused, even on the most privileged connection.
	_, err := f.c.InsertRows(ctx, "owned", "free", "true", nil, nil)
	wantTableReadOnly(t, "InsertRows", err, "owned")
	_, err = f.c.InsertSelect(ctx, "owned", "free", "true",
		[]db.GroupCol{{Attr: "Owner"}}, []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "N"})
	wantTableReadOnly(t, "InsertSelect", err, "owned")

	// Source owned: allowed. Reading an owned table is a read, and a read is authorized as
	// one -- so this must NOT come back as ErrTableReadOnly.
	if _, err := f.c.InsertRows(ctx, "free", "owned", "true", nil, nil); errors.Is(err, ErrTableReadOnly) {
		t.Fatalf("copying FROM an owned table was refused as a write: %v", err)
	}
}

// Declaring a primary key is a write: it decides how every later derived key is formed.
func TestSetPrimaryKeyIsGated(t *testing.T) {
	f := newGateFixture(t, daemonOpts)
	err := f.c.SetPrimaryKey(context.Background(), "owned", []string{"N"})
	wantTableReadOnly(t, "SetPrimaryKey", err, "owned")
}

func TestInsertRowsAdvertised(t *testing.T) {
	found := false
	for _, f := range serverFeatures {
		if f == FeatInsertRows {
			found = true
		}
	}
	if !found {
		t.Fatal("FeatInsertRows is not advertised, so no client will send the opcode")
	}
}
