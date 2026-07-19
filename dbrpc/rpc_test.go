package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

func testPair(t *testing.T) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

func TestRPCRoundTrip(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(context.Background(), "1.0", "ProcId = 0\nClusterId = 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetAttribute(context.Background(), "1.0", "JobStatus", "1"); err != nil {
		t.Fatal(err)
	}
	// Read-your-writes over the wire.
	if v, ok, err := tx.LookupAttr(context.Background(), "1.0", "JobStatus"); err != nil || !ok || v != "1" {
		t.Fatalf("LookupAttr = %q,%v,%v want 1", v, ok, err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// New transaction sees the committed state.
	tx2, _ := c.Begin(context.Background())
	v, ok, err := tx2.LookupAttr(context.Background(), "1.0", "JobStatus")
	if err != nil || !ok || v != "1" {
		t.Fatalf("committed LookupAttr = %q,%v,%v want 1", v, ok, err)
	}
	_ = tx2.Abort(context.Background())
}

func TestRPCQueryLimit(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	tx, _ := c.Begin(context.Background())
	for i := 0; i < 100; i++ {
		_ = tx.NewClassAd(context.Background(), fmt.Sprintf("k%d", i), "Cpus = 4")
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows, err := c.QueryLimit(context.Background(), "Cpus == 4", 5); err != nil || len(rows) != 5 {
		t.Fatalf("QueryLimit(5) = %d rows, %v; want 5", len(rows), err)
	}
	if rows, err := c.QueryLimit(context.Background(), "Cpus == 4", 0); err != nil || len(rows) != 100 {
		t.Fatalf("QueryLimit(0) = %d rows, %v; want 100", len(rows), err)
	}
	// A limit larger than the match count returns all matches.
	if rows, err := c.QueryLimit(context.Background(), "Cpus == 4", 500); err != nil || len(rows) != 100 {
		t.Fatalf("QueryLimit(500) = %d rows, %v; want 100", len(rows), err)
	}
}

func TestRPCConflict(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seed, _ := c.Begin(context.Background())
	_ = seed.NewClassAd(context.Background(), "j", "JobStatus = 1")
	if err := seed.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	a, _ := c.Begin(context.Background())
	b, _ := c.Begin(context.Background())
	_, _, _ = a.LookupAttr(context.Background(), "j", "JobStatus") // snapshot
	_, _, _ = b.LookupAttr(context.Background(), "j", "JobStatus")
	_ = a.SetAttribute(context.Background(), "j", "JobStatus", "2")
	_ = b.SetAttribute(context.Background(), "j", "JobStatus", "3")
	if err := a.Commit(context.Background()); err != nil {
		t.Fatalf("first commit should win: %v", err)
	}
	err := b.Commit(context.Background())
	ce, ok := err.(*db.ConflictError)
	if !ok || len(ce.Keys) != 1 || ce.Keys[0] != "j" {
		t.Fatalf("second commit = %v, want ConflictError on j", err)
	}
}

func TestRPCStreamQueryAndMatch(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "s1", "Cpus = 4\nRequirements = true")
	_ = tx.NewClassAd(context.Background(), "s2", "Cpus = 16\nRequirements = true")
	_ = tx.NewClassAd(context.Background(), "s3", "Cpus = 8\nRequirements = true")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := c.Query(context.Background(), "Cpus >= 8")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("Query streamed %d rows, want 2", len(rows))
	}

	got, err := c.MatchSorted(context.Background(), "Requirements = TARGET.Cpus >= 4\nRank = TARGET.Cpus", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("MatchSorted streamed %d, want 2", len(got))
	}
	// Best-ranked first (Cpus 16).
	if !strings.Contains(got[0], "Cpus = 16") {
		t.Fatalf("top match = %q, want the Cpus=16 ad", got[0])
	}
}

func TestRPCOrdered(t *testing.T) {
	d, err := db.OpenConfig(db.Config{Ordered: []db.OrderSpec{{
		Partition: "Owner",
		Keys:      []db.SortKey{{Expr: "JobPrio", Desc: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "1", "Owner = \"alice\"\nJobPrio = 5")
	_ = tx.NewClassAd(context.Background(), "2", "Owner = \"alice\"\nJobPrio = 10")
	_ = tx.NewClassAd(context.Background(), "3", "Owner = \"bob\"\nJobPrio = 1")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := c.Ordered(context.Background(), 0, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("Ordered streamed %d rows, want 2", len(rows))
	}
	// JobPrio descending: the prio-10 ad first.
	if !strings.Contains(rows[0].AdText, "JobPrio = 10") {
		t.Fatalf("first ordered row = %q, want JobPrio 10", rows[0].AdText)
	}
}

func TestRPCWatch(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	events, stop, err := c.Watch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	time.Sleep(30 * time.Millisecond) // let the server-side watch subscribe

	// A commit over the SAME connection: its ops mux with the streaming watch.
	tx, _ := c.Begin(context.Background())
	_ = tx.NewClassAd(context.Background(), "k", "N = 1")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("watch channel closed before the k upsert")
			}
			if ev.Kind == 0 && ev.Key == "k" && strings.Contains(ev.AdText, "N = 1") {
				return
			}
		case <-deadline:
			t.Fatal("did not receive the k upsert over the watch")
		}
	}
}

// TestRPCConcurrentCalls issues many calls concurrently over ONE connection; they all
// complete correctly, exercising the out-of-order mux (each response demuxed by id).
func TestRPCConcurrentCalls(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	const n = 200
	tx, _ := c.Begin(context.Background())
	for i := 0; i < n; i++ {
		if err := tx.NewClassAd(context.Background(), fmt.Sprintf("k%d", i), fmt.Sprintf("N = %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rtx, err := c.Begin(context.Background()) // independent transactions, concurrent over one conn
			if err != nil {
				errs[i] = err
				return
			}
			v, ok, err := rtx.LookupAttr(context.Background(), fmt.Sprintf("k%d", i), "N")
			_ = rtx.Abort(context.Background())
			if err != nil {
				errs[i] = err
			} else if !ok || v != fmt.Sprintf("%d", i) {
				errs[i] = fmt.Errorf("k%d N = %q,%v", i, v, ok)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("call %d: %v", i, e)
		}
	}
}
