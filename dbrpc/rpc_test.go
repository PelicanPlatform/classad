package dbrpc

import (
	"fmt"
	"sync"
	"testing"

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

	tx, err := c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd("1.0", "ProcId = 0\nClusterId = 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetAttribute("1.0", "JobStatus", "1"); err != nil {
		t.Fatal(err)
	}
	// Read-your-writes over the wire.
	if v, ok, err := tx.LookupAttr("1.0", "JobStatus"); err != nil || !ok || v != "1" {
		t.Fatalf("LookupAttr = %q,%v,%v want 1", v, ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// New transaction sees the committed state.
	tx2, _ := c.Begin()
	v, ok, err := tx2.LookupAttr("1.0", "JobStatus")
	if err != nil || !ok || v != "1" {
		t.Fatalf("committed LookupAttr = %q,%v,%v want 1", v, ok, err)
	}
	_ = tx2.Abort()
}

func TestRPCConflict(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seed, _ := c.Begin()
	_ = seed.NewClassAd("j", "JobStatus = 1")
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	a, _ := c.Begin()
	b, _ := c.Begin()
	_, _, _ = a.LookupAttr("j", "JobStatus") // snapshot
	_, _, _ = b.LookupAttr("j", "JobStatus")
	_ = a.SetAttribute("j", "JobStatus", "2")
	_ = b.SetAttribute("j", "JobStatus", "3")
	if err := a.Commit(); err != nil {
		t.Fatalf("first commit should win: %v", err)
	}
	err := b.Commit()
	ce, ok := err.(*db.ConflictError)
	if !ok || len(ce.Keys) != 1 || ce.Keys[0] != "j" {
		t.Fatalf("second commit = %v, want ConflictError on j", err)
	}
}

// TestRPCConcurrentCalls issues many calls concurrently over ONE connection; they all
// complete correctly, exercising the out-of-order mux (each response demuxed by id).
func TestRPCConcurrentCalls(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	const n = 200
	tx, _ := c.Begin()
	for i := 0; i < n; i++ {
		if err := tx.NewClassAd(fmt.Sprintf("k%d", i), fmt.Sprintf("N = %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rtx, err := c.Begin() // independent transactions, concurrent over one conn
			if err != nil {
				errs[i] = err
				return
			}
			v, ok, err := rtx.LookupAttr(fmt.Sprintf("k%d", i), "N")
			_ = rtx.Abort()
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
