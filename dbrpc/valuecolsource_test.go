package dbrpc

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// colSourceFixture builds a table the column relay can actually serve, and captures the query
// log so a test can see WHICH source answered.
//
// Small segments on purpose. The accelerator covers sealed segments only, and at the default
// size a few thousand small ads never fill one -- the relay then declines, the op falls through
// to the record scan, and a test of the column source passes without ever reaching it. Which is
// what the fixtures already here do, and why this one is separate.
func colSourceFixture(t *testing.T, n int) (*Client, *[]QueryLog, *sync.Mutex, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// An ARCHIVE, because a catalog's mutable table takes no segment size and at the default
	// a few thousand small ads never fill one. An archive does take it, and history is the
	// table whose readers most want this anyway -- it is what gets scanned in bulk.
	mut, err := cat.CreateArchiveTable("jobs", db.ArchiveConfig{SegmentSize: 1 << 13})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", fmt.Sprintf("u%d", i%4))
		ad.InsertAttr("ResidentSetSize", int64(1024*(1+i%5)))
		ad.InsertAttrFloat("Load", float64(i%97)/7)
		ad.InsertAttrBool("Held", i%3 == 0)
		if err := mut.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	if !mut.BuildAndEnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}

	var mu sync.Mutex
	logs := []QueryLog{}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() {
		_ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, QueryLog: func(q QueryLog) {
			mu.Lock()
			logs = append(logs, q)
			mu.Unlock()
		}})
	}()
	c := NewClient(cconn)
	return c, &logs, &mu, func() { c.Close(); s.Close(); cat.Close() }
}

// TestValueStreamColumnSourceAgreesWithTheRecordSource: the op now fills its reply from the
// table's columns where it can, and from the record scan where it cannot. The two must hand back
// the same rows -- same values, same packing -- or this is a different op depending on how it
// was asked.
//
// THE LIMIT IS THE SWITCH, which is what makes this a differential rather than two runs of one
// path. An unlimited read takes the column source; a limited one takes the record scan, because
// under a LIMIT the walk order decides which rows come back. Asking for a limit of exactly the
// row count therefore runs the SAME query down the other path.
func TestValueStreamColumnSourceAgreesWithTheRecordSource(t *testing.T) {
	const n = 6000
	c, logs, mu, cleanup := colSourceFixture(t, n)
	defer cleanup()
	attrs := []string{"ClusterId", "Owner", "ResidentSetSize", "Load", "Held", "Absent"}

	columnar := collectValues(t, c, "jobs", attrs, 0)
	records := collectValues(t, c, "jobs", attrs, n)

	mu.Lock()
	var tookColumns, tookRecords int
	for _, q := range *logs {
		if q.Op != "QueryValues" {
			continue
		}
		if q.Columns.Rows > 0 {
			tookColumns++
		} else {
			tookRecords++
		}
	}
	mu.Unlock()
	if tookColumns == 0 {
		t.Fatalf("no read was served from the columns, so this compares one path with itself "+
			"(%d record reads logged)", tookRecords)
	}
	if tookRecords == 0 {
		t.Fatal("no read was served from the records, so there is nothing to compare against")
	}

	if len(columnar) != len(records) {
		t.Fatalf("the column source returned %d rows, the record source %d", len(columnar), len(records))
	}
	if len(columnar) != n {
		t.Fatalf("the column source returned %d rows, want %d", len(columnar), n)
	}
	keys := make([]string, 0, len(records))
	for k := range records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := records[k]
		got, ok := columnar[k]
		if !ok {
			t.Fatalf("the column source omitted ClusterId=%s", k)
		}
		for col, name := range attrs {
			if g, same := sameValue(got[col], want[col]); !same {
				t.Errorf("ClusterId=%s %s: columns %q, records %q", k, name, g, want[col].String())
			}
		}
	}
}

// TestValueStreamColumnStatsReportTheSplit: the query log is the only place that says a read was
// served from the columns, and how much of it really was. A projection naming one attribute the
// schema does not carry is answered correctly and reads every row the slow way, and nothing else
// in the reply mentions it.
func TestValueStreamColumnStatsReportTheSplit(t *testing.T) {
	const n = 6000
	c, logs, mu, cleanup := colSourceFixture(t, n)
	defer cleanup()

	collectValues(t, c, "jobs", []string{"ClusterId", "Owner", "Load"}, 0)
	collectValues(t, c, "jobs", []string{"ClusterId", "Owner", "NoSuchAttributeAnywhere"}, 0)

	mu.Lock()
	defer mu.Unlock()
	var hot, mixed *QueryLog
	for i := range *logs {
		q := &(*logs)[i]
		if q.Op != "QueryValues" {
			continue
		}
		if q.Projection == 3 && hot == nil {
			hot = q
		} else if q.Projection == 3 {
			mixed = q
		}
	}
	if hot == nil || mixed == nil {
		t.Fatalf("expected two logged reads, got %d", len(*logs))
	}
	if hot.Columns.ColumnCells == 0 {
		t.Errorf("the wholly columnar read reports no cells from the columns: %+v", hot.Columns)
	}
	if mixed.Columns.RecordCells == 0 {
		t.Errorf("the read naming an unknown attribute reports no cells from records: %+v", mixed.Columns)
	}
	if mixed.Columns.Rows != n || hot.Columns.Rows != n {
		t.Errorf("rows logged: columnar %d, mixed %d, want %d", hot.Columns.Rows, mixed.Columns.Rows, n)
	}
}

// TestValueStreamLimitKeepsTheRecordSource pins the carve-out rather than leaving it to the
// comment. A LIMIT must not reach the column source: it walks records block by block rather than
// as stored, so under a limit it would return a different SUBSET, not merely a different order.
func TestValueStreamLimitKeepsTheRecordSource(t *testing.T) {
	c, logs, mu, cleanup := colSourceFixture(t, 6000)
	defer cleanup()
	ctx := context.Background()
	_ = ctx
	collectValues(t, c, "jobs", []string{"ClusterId", "Owner"}, 10)
	mu.Lock()
	defer mu.Unlock()
	for _, q := range *logs {
		if q.Op == "QueryValues" && q.Limit > 0 && q.Columns.Rows > 0 {
			t.Errorf("a read with LIMIT %d was served from the columns: %+v", q.Limit, q.Columns)
		}
	}
}
