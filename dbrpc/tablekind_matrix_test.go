package dbrpc

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// The table-resolution matrix: every opcode that names a table, crossed with every kind a
// name can have (mutable table, materialized view, append-only archive, nonexistent).
//
// It exists because table resolution is spread across the handlers -- some resolve all
// three kinds, some two, some one -- and nothing else states, in one place, what each op
// answers for each kind. Without it, a change to one resolution path cannot be shown to
// have left the others alone, and a refusal's wording cannot be changed without guessing
// who else emits it.
//
// Each cell records the FIRST response frame's status and, for a refusal, its exact
// message. That is what a client sees and what a client may match on, so it is what the
// matrix pins.

// The four names under test. A catalog holds all four; the cells of an op that can change
// one get a catalog each, the rest share one (see mxCase.mutates).
const (
	mxTable   = "mut"
	mxView    = "vw"
	mxArchive = "hist"
	mxMissing = "nope"
)

// mxKind is a column of the matrix.
type mxKind string

const (
	kTable   mxKind = "table"
	kView    mxKind = "view"
	kArchive mxKind = "archive"
	kMissing mxKind = "missing"
)

var mxKinds = []mxKind{kTable, kView, kArchive, kMissing}

func (k mxKind) name() string {
	switch k {
	case kTable:
		return mxTable
	case kView:
		return mxView
	case kArchive:
		return mxArchive
	}
	return mxMissing
}

// mxFixture is a catalog with one name of each kind, served on one DAEMON connection --
// the most privileged, so an authorization refusal never stands in for a resolution one.
type mxFixture struct {
	c   *Client
	s   *Server
	cat *db.Catalog
}

func newMXFixture(t *testing.T) *mxFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := cat.CreateTable(mxTable)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b"} {
		ad, perr := classad.ParseOld(`Owner = "alice"` + "\n" + `N = 1`)
		if perr != nil {
			t.Fatal(perr)
		}
		if err := base.Put(k, ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.CreateView(mxView, db.ViewSpec{
		BaseTable:   mxTable,
		Groups:      []db.ViewGroupCol{{Attr: "Owner", Alias: "label_owner"}},
		Metrics:     []db.ViewMetric{{Func: db.ViewCount, Arg: "*", Alias: "metric_n"}},
		Cardinality: 100,
	}); err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable(mxArchive, db.ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendOld(`Owner = "alice"` + "\n" + `N = 1`); err != nil {
		t.Fatal(err)
	}
	f := &mxFixture{s: NewServerCatalog(cat), cat: cat}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	f.c = NewClient(cconn)
	return f
}

func (f *mxFixture) close() { f.c.Close(); f.s.Close(); f.cat.Close() }

// mxProbe sends one request and reports the first response frame as a short string:
// "ok" / "stream" for a success, "err: <message>" for a refusal, "badreq", "missing", or
// "no reply" for an op that answers nothing until a later frame (opRestore).
func mxProbe(t *testing.T, c *Client, build func(id uint64) []byte, wait time.Duration, longLived bool) string {
	t.Helper()
	if wait == 0 {
		wait = 2 * time.Second
	}
	id, ch, _, err := c.callStream(build)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	cell, streaming := mxFirst(ch, wait)
	// A watch that started runs until it is stopped. Abandoning one leaves a server
	// goroutine reading the store after the fixture closes its catalog, so stop it and
	// wait for the stream to end. A watch that was REFUSED never started, and its reqID
	// is not in the server's watch set, so there is nothing to stop and nothing to wait
	// for -- waiting would hang.
	if longLived && streaming {
		if _, _, serr := c.callCtx(context.Background(), func(rid uint64) []byte {
			return putU64(req(rid, opWatchStop), id)
		}); serr != nil {
			t.Fatalf("watch stop: %v", serr)
		}
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return cell
				}
			case <-time.After(wait):
				t.Fatal("watch did not end after opWatchStop")
			}
		}
	}
	c.cancelPending(id)
	return cell
}

// mxFirst reads the first response frame and renders it as a cell, reporting whether it
// was a streamed result (so the request is still running).
func mxFirst(ch <-chan []byte, wait time.Duration) (cell string, streaming bool) {
	select {
	case frame, ok := <-ch:
		if !ok {
			// stStreamEnd closes the channel without delivering a frame, so an empty
			// stream arrives as a close: a success that matched nothing.
			return "stream (empty)", false
		}
		_, status, body, ok := respHeader(frame)
		if !ok {
			return "short frame", false
		}
		switch status {
		case stOK:
			return "ok", false
		case stStream, stStreamStats, stStreamCursor:
			return "stream", true
		case stStreamEnd:
			return "stream", false
		case stMissing:
			return "missing", false
		case stBadReq:
			return "badreq", false
		case stErr:
			return "err: " + body.str(), false
		default:
			return fmt.Sprintf("status %d", status), false
		}
	case <-time.After(wait):
		return "no reply", false
	}
}

// mxWant is the matrix itself: what each table-naming opcode answers for each kind of
// name. "stream" / "stream (empty)" / "ok" are successes; everything else is the exact
// message the server sent, because the message is what a caller sees and may match on.
//
// Reading down the archive column is the point of the table. An op either serves an
// archive, or refuses it saying so, or -- for the ops that resolve only the mutable
// catalog (Begin, DeleteWhere, MatchTables, MatchExplain, Restore) -- still answers "no
// such table". Those last are left alone here: they are writes and matchmaking over live
// ads, they resolve nothing else either (a view gets the same answer), and widening them
// is a change to what they refuse, not to how they resolve.
var mxWant = map[string]map[mxKind]string{
	"Admin": {
		kTable:   "err: unknown admin action \"no.such.action\"",
		kView:    "err: no such table: vw",
		kArchive: "err: unknown archive admin action \"no.such.action\"",
		kMissing: "err: no such table: nope",
	},
	"Aggregate": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"AggregateBucketed": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"AggregateFiltered": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"ArchiveAggregate": {
		kTable:   "err: no such archive: mut",
		kView:    "err: no such archive: vw",
		kArchive: "stream",
		kMissing: "err: no such archive: nope",
	},
	"ArchiveAggregateFiltered": {
		kTable:   "err: no such archive: mut",
		kView:    "err: no such archive: vw",
		kArchive: "stream",
		kMissing: "err: no such archive: nope",
	},
	"ArchiveAggregateStats": {
		kTable:   "err: no such archive: mut",
		kView:    "err: no such archive: vw",
		kArchive: "stream",
		kMissing: "err: no such archive: nope",
	},
	"ArchiveAppend": {
		kTable:   "err: no such archive: mut",
		kView:    "err: no such archive: vw",
		kArchive: "ok",
		kMissing: "err: no such archive: nope",
	},
	"ArchiveCreate": {
		kTable:   "err: catalog: \"mut\" already exists as a mutable table",
		kView:    "ok",
		kArchive: "ok",
		kMissing: "ok",
	},
	"ArchiveQuery": {
		kTable:   "err: no such archive: mut",
		kView:    "err: no such archive: vw",
		kArchive: "stream",
		kMissing: "err: no such archive: nope",
	},
	"ArchiveRotate": {
		kTable:   "err: no such archive: mut",
		kView:    "err: no such archive: vw",
		kArchive: "ok",
		kMissing: "err: no such archive: nope",
	},
	"Begin": {
		kTable:   "ok",
		kView:    "err: no such table: vw",
		kArchive: "err: no such table: hist",
		kMissing: "err: no such table: nope",
	},
	"CreateTable": {
		kTable:   "ok",
		kView:    "err: catalog: \"vw\" already exists as a materialized view",
		kArchive: "err: catalog: \"hist\" already exists as an archive table",
		kMissing: "ok",
	},
	"CreateTableMem": {
		kTable:   "ok",
		kView:    "err: catalog: \"vw\" already exists as a materialized view",
		kArchive: "err: catalog: \"hist\" already exists as an archive table",
		kMissing: "ok",
	},
	"CreateView": {
		kTable:   "err: catalog: \"mut\" already exists as a table",
		kView:    "err: catalog: view \"vw\" already exists",
		kArchive: "err: catalog: \"hist\" already exists as an archive table",
		kMissing: "ok",
	},
	"DeleteWhere": {
		kTable:   "ok",
		kView:    "err: no such table: vw",
		kArchive: "err: no such table: hist",
		kMissing: "err: no such table: nope",
	},
	"Diag": {
		kTable:   "ok",
		kView:    "err: no such table: vw",
		kArchive: "ok",
		kMissing: "err: no such table: nope",
	},
	"DropTable": {
		kTable:   "ok",
		kView:    "err: catalog: no such table \"vw\"",
		kArchive: "err: catalog: no such table \"hist\"",
		kMissing: "err: catalog: no such table \"nope\"",
	},
	"DropView": {
		kTable:   "err: catalog: no such view \"mut\"",
		kView:    "ok",
		kArchive: "err: catalog: no such view \"hist\"",
		kMissing: "err: catalog: no such view \"nope\"",
	},
	"Explain": {
		kTable:   "ok",
		kView:    "err: no such table: vw",
		kArchive: "ok",
		kMissing: "err: no such table: nope",
	},
	"MatchExplain": {
		kTable:   "ok",
		kView:    "err: no such table: vw",
		kArchive: "err: no such table: hist",
		kMissing: "err: no such table: nope",
	},
	"MatchSorted": {
		kTable:   "stream (empty)",
		kView:    "stream (empty)",
		kArchive: "err: archive table \"hist\": matchmaking ranks live ads; an archive holds completed records",
		kMissing: "err: no such table: nope",
	},
	"MatchTables": {
		kTable:   "stream",
		kView:    "err: no such table: vw",
		kArchive: "err: no such table: hist",
		kMissing: "err: no such table: nope",
	},
	"Ordered": {
		kTable:   "stream (empty)",
		kView:    "stream (empty)",
		kArchive: "err: archive table \"hist\": a secondary ordering is maintained over a mutable set; an archive has none",
		kMissing: "err: no such table: nope",
	},
	"Query": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "err: archive table \"hist\": Query is not served from an archive; use ArchiveQuery, which answers newest-first with the limit pushed down",
		kMissing: "err: no such table: nope",
	},
	"QueryAsOf": {
		kTable:   "err: collections: time travel is not enabled on this collection",
		kView:    "err: collections: time travel is not enabled on this collection",
		kArchive: "err: archive table \"hist\": an archive supersedes nothing, so there is no earlier version to read as of an instant",
		kMissing: "err: no such table: nope",
	},
	"QueryKeys": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "err: archive table \"hist\": an archive has no key directory, so its rows have no storage keys to return",
		kMissing: "err: no such table: nope",
	},
	"QueryRaw": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "err: archive table \"hist\": QueryRaw is not served from an archive; use QueryRawProject, which does read one",
		kMissing: "err: no such table: nope",
	},
	"QueryRawProj": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"QueryRawProjRefs": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"QueryRawProjRefsStats": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"QueryRawProjSeq": {
		kTable:   "stream",
		kView:    "err: cursor pagination is not available for a view: vw",
		kArchive: "err: archive table \"hist\": cursor pagination is not needed; an archive answers newest-first with the limit pushed down",
		kMissing: "err: no such table: nope",
	},
	"QueryRawWire": {
		kTable:   "stream",
		kView:    "badreq",
		kArchive: "err: archive table \"hist\": QueryRawWire is not served from an archive; use QueryRawProject, which does read one",
		kMissing: "err: no such table: nope",
	},
	"QueryRawWireRefs": {
		kTable:   "stream",
		kView:    "badreq",
		kArchive: "err: archive table \"hist\": QueryRawWire is not served from an archive; use QueryRawProject, which does read one",
		kMissing: "err: no such table: nope",
	},
	"QueryStats": {
		kTable:   "ok",
		kView:    "ok",
		kArchive: "ok",
		kMissing: "ok",
	},
	"Restore": {
		kTable:   "no reply",
		kView:    "err: no such table: vw",
		kArchive: "err: no such table: hist",
		kMissing: "err: no such table: nope",
	},
	"Snapshot": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "err: archive table \"hist\": Snapshot is not available for an archive",
		kMissing: "err: no such table: nope",
	},
	"TableToMemory": {
		kTable:   "ok",
		kView:    "err: catalog: no such table \"vw\"",
		kArchive: "err: catalog: \"hist\" is an archive table, not a mutable table",
		kMissing: "err: catalog: no such table \"nope\"",
	},
	"TopK": {
		kTable:   "stream",
		kView:    "stream (empty)",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"TopKStats": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"Watch": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
	"WatchHead": {
		kTable:   "ok",
		kView:    "ok",
		kArchive: "ok",
		kMissing: "err: no such table: nope",
	},
	"WatchWire": {
		kTable:   "stream",
		kView:    "stream",
		kArchive: "stream",
		kMissing: "err: no such table: nope",
	},
}

// mxCase is one row of the matrix: an opcode and a frame builder parameterized by the
// table name under test.
type mxCase struct {
	op    string
	build func(name string) func(uint64) []byte
	// wait overrides the response deadline, for the one op that answers nothing on
	// success (opRestore replies only at opRestoreEnd).
	wait time.Duration
	// longLived marks an op whose success is an open-ended stream (the watches), so the
	// probe stops it rather than abandoning it.
	longLived bool
	// mutates marks an op that can change the catalog or a table, so its cells each get
	// their own fixture. The rest share one, which is most of why this test is seconds
	// rather than a minute: closing a catalog costs ~150ms and there are 160-odd cells.
	mutates bool
}

// aggTail appends the group/aggregate tail shared by the aggregate opcodes.
func aggTail(b []byte, widths, filters bool) []byte {
	b = putI32(b, 1) // one group column
	b = putStr(b, "Owner")
	if widths {
		b = putU64(b, 0)
	}
	b = putI32(b, 1) // one aggregate
	b = putU8(b, byte(db.AggCount))
	b = putStr(b, "*")
	if filters {
		b = putStr(b, "")
	}
	return b
}

func projTail(b []byte) []byte {
	b = putI32(b, 1)
	return putStr(b, "Owner")
}

func mxCases() []mxCase {
	// An admin action that exists for neither kind, so the cell reports what the op did
	// with the NAME rather than what it did with the action.
	const bogusAction = "no.such.action"
	return []mxCase{
		{op: "Begin", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opBegin), n) }
		}},
		{op: "Query", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putI32(putStr(req(id, opQuery), n), 0), "true")
			}
		}},
		{op: "MatchSorted", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putI32(putStr(req(id, opMatchSorted), n), 1), "Requirements = true")
			}
		}},
		{op: "Watch", longLived: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putBytes(putStr(req(id, opWatch), n), nil) }
		}},
		{op: "WatchWire", longLived: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putBytes(putStr(req(id, opWatchWire), n), nil) }
		}},
		{op: "Ordered", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putI32(putStr(req(id, opOrdered), n), 0), "")
			}
		}},
		{op: "Aggregate", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return aggTail(putStr(putStr(req(id, opAggregate), n), "true"), false, false)
			}
		}},
		{op: "AggregateBucketed", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return aggTail(putStr(putStr(req(id, opAggregateBucketed), n), "true"), true, false)
			}
		}},
		{op: "AggregateFiltered", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return aggTail(putStr(putStr(req(id, opAggregateFiltered), n), "true"), true, true)
			}
		}},
		{op: "Diag", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opDiag), n) }
		}},
		{op: "Explain", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(putStr(req(id, opExplain), n), "true") }
		}},
		{op: "Admin", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(putStr(req(id, opAdmin), n), bogusAction)
				return putI32(b, 0)
			}
		}},
		{op: "CreateTable", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opCreateTable), n) }
		}},
		{op: "CreateTableMem", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opCreateTableMem), n) }
		}},
		{op: "TableToMemory", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opTableToMemory), n) }
		}},
		{op: "DropTable", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opDropTable), n) }
		}},
		{op: "MatchTables", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(req(id, opMatchTables), n)
				b = putStr(b, mxTable)
				b = putStr(b, "Owner")
				b = putStr(b, "true")
				b = putStr(b, "true")
				b = putI32(b, 1)
				return putI32(b, 0)
			}
		}},
		{op: "MatchExplain", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(req(id, opMatchExplain), n)
				b = putStr(b, "true")
				b = putStr(b, mxTable)
				return putStr(b, "true")
			}
		}},
		{op: "WatchHead", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opWatchHead), n) }
		}},
		{op: "Snapshot", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opSnapshot), n) }
		}},
		{op: "Restore", mutates: true, wait: 400 * time.Millisecond, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opRestore), n) }
		}},
		{op: "DeleteWhere", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putStr(req(id, opDeleteWhere), n), "false")
			}
		}},
		{op: "QueryRaw", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putI32(putStr(req(id, opQueryRaw), n), 0), "true")
			}
		}},
		{op: "QueryRawProj", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return projTail(putStr(putI32(putStr(req(id, opQueryRawProj), n), 0), "true"))
			}
		}},
		{op: "QueryRawProjRefs", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return projTail(putStr(putI32(putStr(req(id, opQueryRawProjRefs), n), 0), "true"))
			}
		}},
		{op: "QueryRawProjRefsStats", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return projTail(putStr(putI32(putStr(req(id, opQueryRawProjRefsStats), n), 0), "true"))
			}
		}},
		{op: "QueryRawProjSeq", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(putI32(putStr(req(id, opQueryRawProjSeq), n), 0), "true")
				b = putU64(b, 0)
				b = putU64(b, 0)
				b = putU64(b, 0)
				b = putStr(b, "")
				return projTail(b)
			}
		}},
		{op: "QueryRawWire", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(req(id, opQueryRawWire), n)
				b = putI32(b, 0)
				b = putU8(b, 0)
				b = putStr(b, "true")
				return projTail(b)
			}
		}},
		{op: "QueryRawWireRefs", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(req(id, opQueryRawWireOpts), n)
				b = putI32(b, 0)
				b = putI32(b, int32(wireFlagRefs)) // the opts frame carries its flags word
				b = putU8(b, 0)
				b = putStr(b, "true")
				return projTail(b)
			}
		}},
		{op: "QueryAsOf", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putI32(putStr(req(id, opQueryAsOf), n), 0)
				b = putU64(b, uint64(time.Now().UnixNano()))
				return putStr(b, "true")
			}
		}},
		{op: "QueryKeys", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(putStr(req(id, opQueryKeys), n), "true") }
		}},
		{op: "TopK", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(putStr(req(id, opTopK), n), "true")
				b = putStr(b, "N")
				b = putU8(b, 1)
				b = putI32(b, 1)
				return projTail(b)
			}
		}},
		{op: "TopKStats", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				b := putStr(putStr(req(id, opTopKStats), n), "true")
				b = putStr(b, "N")
				b = putU8(b, 1)
				b = putI32(b, 1)
				return projTail(b)
			}
		}},
		{op: "QueryStats", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putU8(putStr(req(id, opQueryStats), n), 0) }
		}},
		{op: "ArchiveCreate", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putBytes(putStr(req(id, opArchiveCreate), n), []byte(`{}`))
			}
		}},
		{op: "ArchiveAppend", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putStr(req(id, opArchiveAppend), n), "N = 1")
			}
		}},
		{op: "ArchiveQuery", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putStr(putI32(putStr(req(id, opArchiveQuery), n), 0), "true")
			}
		}},
		{op: "ArchiveRotate", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opArchiveRotate), n) }
		}},
		{op: "ArchiveAggregate", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return aggTail(putStr(putStr(req(id, opArchiveAggregate), n), "true"), false, false)
			}
		}},
		{op: "ArchiveAggregateFiltered", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return aggTail(putStr(putStr(req(id, opArchiveAggregateFiltered), n), "true"), true, true)
			}
		}},
		{op: "ArchiveAggregateStats", build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return aggTail(putStr(putStr(req(id, opArchiveAggregateStats), n), "true"), true, true)
			}
		}},
		{op: "CreateView", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte {
				return putBytes(putStr(req(id, opCreateView), n),
					[]byte(`{"baseTable":"`+mxTable+`","groups":[{"attr":"Owner","alias":"g"}],"metrics":[{"func":"count","arg":"*","alias":"m"}],"cardinality":10}`))
			}
		}},
		{op: "DropView", mutates: true, build: func(n string) func(uint64) []byte {
			return func(id uint64) []byte { return putStr(req(id, opDropView), n) }
		}},
	}
}

// TestTableKindMatrix runs every table-naming opcode against every kind of name.
//
// The cells run in parallel. Those of a mutating opcode (create, drop, append, rotate,
// restore, admin) get a catalog each so they cannot reach another cell; the read-only ones
// share one, which keeps the test seconds rather than a minute. Setting MATRIX_DUMP prints
// the whole matrix, which is how it is re-read after a deliberate change.
func TestTableKindMatrix(t *testing.T) {
	t.Parallel()
	shared := newMXFixture(t)
	t.Cleanup(shared.close)
	var mu sync.Mutex
	got := map[string]map[mxKind]string{}
	for _, tc := range mxCases() {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()
			for _, k := range mxKinds {
				t.Run(string(k), func(t *testing.T) {
					t.Parallel()
					f := shared
					if tc.mutates {
						f = newMXFixture(t)
						defer f.close()
					}
					cell := mxProbe(t, f.c, tc.build(k.name()), tc.wait, tc.longLived)
					mu.Lock()
					if got[tc.op] == nil {
						got[tc.op] = map[mxKind]string{}
					}
					got[tc.op][k] = cell
					mu.Unlock()
					if want, ok := mxWant[tc.op][k]; ok && cell != want {
						t.Errorf("%s on a %s name:\n got %q\nwant %q", tc.op, k, cell, want)
					} else if !ok {
						t.Errorf("%s on a %s name: no expectation recorded (got %q)", tc.op, k, cell)
					}
				})
			}
		})
	}
	t.Cleanup(func() {
		if os.Getenv("MATRIX_DUMP") == "" {
			return
		}
		ops := make([]string, 0, len(got))
		for op := range got {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		var sb strings.Builder
		for _, op := range ops {
			for _, k := range mxKinds {
				fmt.Fprintf(&sb, "%s|%s|%s\n", op, k, got[op][k])
			}
		}
		t.Log("\n" + sb.String())
	})
}
