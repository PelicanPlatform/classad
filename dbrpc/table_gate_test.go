package dbrpc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// ownedTables is a TableWritable gate over a mutable set of owned (read-only) names.
type ownedTables struct {
	mu    sync.Mutex
	names map[string]bool
}

func newOwnedTables(names ...string) *ownedTables {
	o := &ownedTables{names: map[string]bool{}}
	for _, n := range names {
		o.names[n] = true
	}
	return o
}

func (o *ownedTables) set(name string, owned bool) {
	o.mu.Lock()
	o.names[name] = owned
	o.mu.Unlock()
}

func (o *ownedTables) writable(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.names[name]
}

// gateFixture is a catalog with an owned and a free mutable table and an owned and a free
// archive, each seeded in-process (the owner's path, not dbrpc), served over one connection.
type gateFixture struct {
	c     *Client
	s     *Server
	cat   *db.Catalog
	owned *ownedTables
}

func newGateFixture(t *testing.T, opts ServeOptions) *gateFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owned", "free"} {
		d, err := cat.CreateTable(name)
		if err != nil {
			t.Fatal(err)
		}
		tx := d.Begin()
		for _, k := range []string{"a", "b"} {
			tx.NewClassAd(k, mustAd(t, "N = 1\nOwner = \"alice\""))
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ownedhist", "freehist"} {
		a, err := cat.CreateArchiveTable(name, db.ArchiveConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.AppendOld("ClusterId = 1"); err != nil {
			t.Fatal(err)
		}
	}
	f := &gateFixture{s: NewServerCatalog(cat), cat: cat, owned: newOwnedTables("owned", "ownedhist", "ownedview", "ownednew")}
	t.Cleanup(func() { f.s.Close(); cat.Close() })
	opts.TableWritable = f.owned.writable
	f.c = f.serve(t, opts)
	return f
}

// serve opens another connection to the fixture's server with exactly opts.
func (f *gateFixture) serve(t *testing.T, opts ServeOptions) *Client {
	t.Helper()
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	return c
}

// daemonOpts is the most privileged connection: TableWritable must hold even here.
var daemonOpts = ServeOptions{IncludePrivate: true, Privileged: true}

// wantTableReadOnly asserts err is the per-table refusal: ErrTableReadOnly and a
// *ServerError (the non-retryable class), naming the table.
func wantTableReadOnly(t *testing.T, what string, err error, table string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s on owned table %q: succeeded, want ErrTableReadOnly", what, table)
	}
	if !errors.Is(err, ErrTableReadOnly) {
		t.Fatalf("%s on %q: err = %v, want errors.Is(ErrTableReadOnly)", what, table, err)
	}
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("%s on %q: err = %T %v, want a *ServerError", what, table, err, err)
	}
	if !strings.Contains(se.Msg, `"`+table+`"`) {
		t.Fatalf("%s: refusal %q does not name table %q", what, se.Msg, table)
	}
}

func wantOK(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s on free table: %v", what, err)
	}
}

// adAttr returns key's attribute name in table as committed, read in-process.
func (f *gateFixture) adAttr(t *testing.T, table, key, name string) (string, bool) {
	t.Helper()
	d, ok := f.cat.Table(table)
	if !ok {
		t.Fatalf("no table %q", table)
	}
	tx := d.Begin()
	defer tx.Abort()
	return tx.LookupAttr(key, name)
}

func (f *gateFixture) count(t *testing.T, table string) int {
	t.Helper()
	d, ok := f.cat.Table(table)
	if !ok {
		t.Fatalf("no table %q", table)
	}
	seq, err := d.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// TestTableWritableTxnWrites: every ad write through a transaction on an owned table is
// refused, the same write on a free table in the same connection is accepted, reads through
// the owned transaction still work, and committing it applies nothing.
func TestTableWritableTxnWrites(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	writes := []struct {
		name string
		do   func(tx *Tx) error
	}{
		{"NewClassAd", func(tx *Tx) error { return tx.NewClassAd(ctx, "new", "N = 9") }},
		{"NewClassAdBatch", func(tx *Tx) error {
			_, err := tx.NewClassAdBatch(ctx, []AdKV{{Key: "nb1", Ad: "N = 9"}, {Key: "nb2", Ad: "N = 9"}})
			return err
		}},
		{"NewClassAdBatchPipelined", func(tx *Tx) error {
			_, err := tx.NewClassAdBatchPipelined(ctx, []AdKV{{Key: "np1", Ad: "N = 9"}, {Key: "np2", Ad: "N = 9"}}, 1)
			return err
		}},
		{"SetAttribute", func(tx *Tx) error { return tx.SetAttribute(ctx, "a", "N", "2") }},
		{"DeleteAttribute", func(tx *Tx) error { return tx.DeleteAttribute(ctx, "a", "Owner") }},
		{"DestroyClassAd", func(tx *Tx) error { return tx.DestroyClassAd(ctx, "b") }},
	}

	otx, err := f.c.BeginTable(ctx, "owned")
	if err != nil {
		t.Fatalf("Begin on an owned table must succeed (read snapshot): %v", err)
	}
	ftx, err := f.c.BeginTable(ctx, "free")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range writes {
		wantTableReadOnly(t, w.name, w.do(otx), "owned")
		wantOK(t, w.name, w.do(ftx))
	}
	if v, ok, err := otx.LookupAttr(ctx, "a", "N"); err != nil || !ok || v != "1" {
		t.Fatalf("read through owned txn: %q %v %v, want 1", v, ok, err)
	}
	if rows, err := otx.Query(ctx, "true", 0); err != nil || len(rows) != 2 {
		t.Fatalf("txn query on owned: %d rows, %v; want 2", len(rows), err)
	}
	if err := otx.Commit(ctx); err != nil {
		t.Fatalf("commit of a write-free txn on an owned table: %v", err)
	}
	if err := ftx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if n := f.count(t, "owned"); n != 2 {
		t.Fatalf("owned table has %d ads after refused writes, want 2", n)
	}
	if v, _ := f.adAttr(t, "owned", "a", "N"); v != "1" {
		t.Fatalf("owned a.N = %q, want 1", v)
	}
	if _, ok := f.adAttr(t, "owned", "a", "Owner"); !ok {
		t.Fatal("owned a.Owner was deleted")
	}
	// free: a, new, nb1, nb2, np1, np2 (b destroyed).
	if n := f.count(t, "free"); n != 6 {
		t.Fatalf("free table has %d ads, want 6", n)
	}
	if v, _ := f.adAttr(t, "free", "a", "N"); v != "2" {
		t.Fatalf("free a.N = %q, want 2", v)
	}
}

// TestTableWritableCommitIdempotent: CommitIdempotent writes a durable marker into the
// table, so it is refused on an owned table even with no data writes.
func TestTableWritableCommitIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	otx, err := f.c.BeginTable(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	wantTableReadOnly(t, "CommitIdempotent", otx.CommitIdempotent(ctx, "k1"), "owned")
	_ = otx.Abort(ctx)
	if n := f.count(t, "owned"); n != 2 {
		t.Fatalf("owned table has %d ads, want 2 (no idempotency marker)", n)
	}

	ftx, err := f.c.BeginTable(ctx, "free")
	if err != nil {
		t.Fatal(err)
	}
	wantOK(t, "NewClassAd", ftx.NewClassAd(ctx, "c", "N = 3"))
	wantOK(t, "CommitIdempotent", ftx.CommitIdempotent(ctx, "k1"))
	if v, _ := f.adAttr(t, "free", "c", "N"); v != "3" {
		t.Fatalf("free c.N = %q, want 3", v)
	}
}

// TestTableWritableCommitAfterGateCloses: writes accepted while a table was writable are not
// applied if the gate closes before Commit -- the whole transaction is refused, nothing lands.
// Two transactions interleaved on one connection, one per table, stay independent.
func TestTableWritableCommitAfterGateCloses(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	ftx, err := f.c.BeginTable(ctx, "free")
	if err != nil {
		t.Fatal(err)
	}
	otx, err := f.c.BeginTable(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	wantOK(t, "NewClassAd", ftx.NewClassAd(ctx, "x1", "N = 1"))
	wantTableReadOnly(t, "NewClassAd", otx.NewClassAd(ctx, "x1", "N = 1"), "owned")
	wantOK(t, "SetAttribute", ftx.SetAttribute(ctx, "a", "N", "5"))
	wantTableReadOnly(t, "SetAttribute", otx.SetAttribute(ctx, "a", "N", "5"), "owned")
	wantOK(t, "NewClassAd", ftx.NewClassAd(ctx, "x2", "N = 2"))

	// The owner takes over "free" while ftx holds buffered writes.
	f.owned.set("free", true)
	wantTableReadOnly(t, "Commit", ftx.Commit(ctx), "free")
	if cerr := otx.Commit(ctx); cerr != nil {
		t.Fatalf("commit of write-free owned txn: %v", cerr)
	}
	for _, table := range []string{"free", "owned"} {
		if n := f.count(t, table); n != 2 {
			t.Fatalf("%s has %d ads, want 2 (nothing applied)", table, n)
		}
		if v, _ := f.adAttr(t, table, "a", "N"); v != "1" {
			t.Fatalf("%s a.N = %q, want 1 (nothing applied)", table, v)
		}
	}

	// Gate reopened: a new transaction commits normally.
	f.owned.set("free", false)
	ftx2, err := f.c.BeginTable(ctx, "free")
	if err != nil {
		t.Fatal(err)
	}
	wantOK(t, "NewClassAd", ftx2.NewClassAd(ctx, "x3", "N = 3"))
	wantOK(t, "Commit", ftx2.Commit(ctx))
	if n := f.count(t, "free"); n != 3 {
		t.Fatalf("free has %d ads after reopen, want 3", n)
	}
}

// TestTableWritableDeleteWhere: the server-side bulk delete is refused on an owned table.
func TestTableWritableDeleteWhere(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)
	_, err := f.c.DeleteWhereTable(ctx, "owned", "true")
	wantTableReadOnly(t, "DeleteWhere", err, "owned")
	n, err := f.c.DeleteWhereTable(ctx, "free", "true")
	wantOK(t, "DeleteWhere", err)
	if n != 2 || f.count(t, "free") != 0 {
		t.Fatalf("free DeleteWhere removed %d, %d left; want 2, 0", n, f.count(t, "free"))
	}
	if f.count(t, "owned") != 2 {
		t.Fatal("owned table lost ads")
	}
}

// TestTableWritableArchive: append, rotate, and the data-removing admin actions are refused
// on an owned archive; layout tuning is still allowed; queries still work.
func TestTableWritableArchive(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	wantTableReadOnly(t, "ArchiveAppend", f.c.ArchiveAppend(ctx, "ownedhist", "ClusterId = 2"), "ownedhist")
	wantOK(t, "ArchiveAppend", f.c.ArchiveAppend(ctx, "freehist", "ClusterId = 2"))
	_, err := f.c.ArchiveRotate(ctx, "ownedhist")
	wantTableReadOnly(t, "ArchiveRotate", err, "ownedhist")
	_, err = f.c.ArchiveRotate(ctx, "freehist")
	wantOK(t, "ArchiveRotate", err)

	for _, a := range [][]string{{"rotate"}, {"retention.set", "0", "0"}, {"truncate"}} {
		_, err = f.c.AdminTable(ctx, "ownedhist", a[0], a[1:]...)
		wantTableReadOnly(t, "admin "+a[0], err, "ownedhist")
		_, err = f.c.AdminTable(ctx, "freehist", a[0], a[1:]...)
		wantOK(t, "admin "+a[0], err)
	}
	_, err = f.c.AdminTable(ctx, "ownedhist", "index.add.value", "ClusterId")
	wantOK(t, "admin index.add.value on owned archive (row-preserving)", err)

	rows, err := f.c.ArchiveQuery(ctx, "ownedhist", "true", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("owned archive query: %d rows, %v; want 1", len(rows), err)
	}
	if rows, _ := f.c.ArchiveQuery(ctx, "freehist", "true", 0); len(rows) != 0 {
		t.Fatalf("free archive has %d rows after truncate, want 0", len(rows))
	}
}

// TestTableWritableCatalog: creating, dropping, or changing the durability of an owned name
// is refused -- for tables, archives, and views alike.
func TestTableWritableCatalog(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	wantTableReadOnly(t, "CreateTable", f.c.CreateTable(ctx, "ownednew"), "ownednew")
	wantTableReadOnly(t, "CreateTableInMemory", f.c.CreateTableInMemory(ctx, "ownednew"), "ownednew")
	wantTableReadOnly(t, "CreateArchiveTable", f.c.CreateArchiveTable(ctx, "ownednew", db.ArchiveConfig{}), "ownednew")
	if _, ok := f.cat.Table("ownednew"); ok {
		t.Fatal("refused CreateTable created the table")
	}
	if _, ok := f.cat.ArchiveTable("ownednew"); ok {
		t.Fatal("refused CreateArchiveTable created the archive")
	}
	wantOK(t, "CreateTable", f.c.CreateTable(ctx, "freenew"))
	wantOK(t, "CreateTableInMemory", f.c.CreateTableInMemory(ctx, "freemem"))
	wantOK(t, "CreateArchiveTable", f.c.CreateArchiveTable(ctx, "freearch", db.ArchiveConfig{}))

	wantTableReadOnly(t, "ConvertTableToMemory", f.c.ConvertTableToMemory(ctx, "owned"), "owned")
	wantOK(t, "ConvertTableToMemory", f.c.ConvertTableToMemory(ctx, "freenew"))

	spec := db.ViewSpec{
		BaseTable:   "owned", // a view only reads its base, so an owned base is fine
		Groups:      []db.ViewGroupCol{{Attr: "Owner", Alias: "label_owner"}},
		Metrics:     []db.ViewMetric{{Func: db.ViewCount, Arg: "*", Alias: "metric_n"}},
		Cardinality: 10,
	}
	wantTableReadOnly(t, "CreateView", f.c.CreateView(ctx, "ownedview", spec), "ownedview")
	wantOK(t, "CreateView", f.c.CreateView(ctx, "freeview", spec))
	f.owned.set("freeview", true)
	wantTableReadOnly(t, "DropView", f.c.DropView(ctx, "freeview"), "freeview")
	f.owned.set("freeview", false)
	wantOK(t, "DropView", f.c.DropView(ctx, "freeview"))

	wantTableReadOnly(t, "DropTable", f.c.DropTable(ctx, "owned"), "owned")
	if _, ok := f.cat.Table("owned"); !ok {
		t.Fatal("refused DropTable dropped the table")
	}
	wantOK(t, "DropTable", f.c.DropTable(ctx, "free"))
}

// TestTableWritableAdmin: truncate is refused on an owned mutable table even for a DAEMON
// peer; row-preserving tuning is allowed. An unprivileged peer keeps getting the DAEMON
// authorization refusal (authorization is decided before the gate).
func TestTableWritableAdmin(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	_, err := f.c.AdminTable(ctx, "owned", "truncate")
	wantTableReadOnly(t, "admin truncate", err, "owned")
	if f.count(t, "owned") != 2 {
		t.Fatal("refused truncate emptied the owned table")
	}
	for _, a := range [][]string{{"index.add.value", "N"}, {"compact"}, {"hot.add", "N"}, {"index.reindex"}} {
		_, err = f.c.AdminTable(ctx, "owned", a[0], a[1:]...)
		wantOK(t, "admin "+a[0]+" on owned (row-preserving)", err)
	}
	_, err = f.c.AdminTable(ctx, "free", "truncate")
	wantOK(t, "admin truncate", err)
	if f.count(t, "free") != 0 {
		t.Fatal("truncate on free left ads")
	}

	u := newGateFixture(t, ServeOptions{})
	for _, a := range []string{"truncate", "index.reindex"} {
		_, err = u.c.AdminTable(ctx, "owned", a)
		if err == nil || errors.Is(err, ErrTableReadOnly) || !strings.Contains(err.Error(), "DAEMON") {
			t.Fatalf("unprivileged admin %s on owned: err = %v, want the DAEMON authorization refusal", a, err)
		}
	}
	err = u.c.ConvertTableToMemory(ctx, "owned")
	if err == nil || errors.Is(err, ErrTableReadOnly) || !strings.Contains(err.Error(), "DAEMON") {
		t.Fatalf("unprivileged ConvertTableToMemory on owned: err = %v, want the DAEMON authorization refusal", err)
	}
}

// TestTableWritableRestore: restoring over an owned table is refused; a free one restores.
func TestTableWritableRestore(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)

	var snap bytes.Buffer
	if err := f.c.SnapshotTable(ctx, "owned", &snap); err != nil {
		t.Fatalf("snapshot of an owned table must work: %v", err)
	}
	wantTableReadOnly(t, "Restore", f.c.RestoreTable(ctx, "owned", bytes.NewReader(snap.Bytes())), "owned")
	if _, err := f.c.DeleteWhereTable(ctx, "free", "true"); err != nil {
		t.Fatal(err)
	}
	wantOK(t, "Restore", f.c.RestoreTable(ctx, "free", bytes.NewReader(snap.Bytes())))
	if n := f.count(t, "free"); n != 2 {
		t.Fatalf("free has %d ads after restore, want 2", n)
	}
}

// TestTableWritableReadsUnaffected: queries, diagnostics, and watches on an owned table work,
// and a watch sees the owner's in-process writes.
func TestTableWritableReadsUnaffected(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, ServeOptions{})

	if rows, err := f.c.QueryTable(ctx, "owned", "true", 0); err != nil || len(rows) != 2 {
		t.Fatalf("QueryTable owned: %d rows, %v", len(rows), err)
	}
	if _, err := f.c.DiagnosticsTable(ctx, "owned"); err != nil {
		t.Fatalf("Diagnostics owned: %v", err)
	}
	cur, err := f.c.WatchHead(ctx, "owned")
	if err != nil {
		t.Fatalf("WatchHead owned: %v", err)
	}
	ch, stop, err := f.c.WatchTable(ctx, "owned", cur)
	if err != nil {
		t.Fatalf("WatchTable owned: %v", err)
	}
	defer stop()
	d, _ := f.cat.Table("owned")
	tx := d.Begin()
	tx.NewClassAd("fromowner", mustAd(t, "N = 7"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("watch closed before the owner's write arrived")
			}
			if ev.Key == "fromowner" {
				return
			}
		case <-deadline:
			t.Fatal("no watch event for the owner's write")
		}
	}
}

// TestTableWritableNilGate: with no gate every table is writable (the historical behavior),
// and the connection-wide read-only refusal is not reported as ErrTableReadOnly.
func TestTableWritableNilGate(t *testing.T) {
	ctx := context.Background()
	f := newGateFixture(t, daemonOpts)
	c := f.serve(t, ServeOptions{Privileged: true})

	tx, err := c.BeginTable(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	wantOK(t, "NewClassAd (nil gate)", tx.NewClassAd(ctx, "z", "N = 1"))
	wantOK(t, "Commit (nil gate)", tx.Commit(ctx))
	wantOK(t, "ArchiveAppend (nil gate)", c.ArchiveAppend(ctx, "ownedhist", "ClusterId = 3"))
	_, err = c.AdminTable(ctx, "owned", "truncate")
	wantOK(t, "admin truncate (nil gate)", err)
	if n := f.count(t, "owned"); n != 0 {
		t.Fatalf("owned has %d ads after nil-gate truncate, want 0", n)
	}

	rc := f.serve(t, ServeOptions{ReadOnly: true})
	_, err = rc.DeleteWhereTable(ctx, "free", "true")
	var se *ServerError
	if !errors.As(err, &se) || errors.Is(err, ErrTableReadOnly) || !strings.Contains(se.Msg, "read-only connection") {
		t.Fatalf("read-only connection refusal = %v, want a read-only-connection ServerError, not ErrTableReadOnly", err)
	}
}
