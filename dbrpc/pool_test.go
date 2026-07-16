package dbrpc

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

func TestPool(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	defer func() { s.Close(); d.Close() }()
	dial := func() (MsgConn, error) {
		cc, sc := netPipe()
		go func() { _ = s.ServeConn(sc) }()
		return cc, nil
	}

	p, err := NewPool(dial, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	tx, err := p.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.NewClassAd("k", "N = 1")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Concurrent queries spread round-robin across the pool's 3 connections; each
	// connection also muxes many of them at once.
	const n = 60
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rows, err := p.Query("N == 1")
			switch {
			case err != nil:
				errs[i] = err
			case len(rows) != 1:
				errs[i] = fmt.Errorf("got %d rows, want 1", len(rows))
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("query %d: %v", i, e)
		}
	}
}
