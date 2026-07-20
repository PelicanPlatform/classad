package dbrpc

import (
	"context"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// privilegedPair is testPair with a DAEMON-privileged connection (for admin actions).
func privilegedPair(t *testing.T) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

// TestRPCTimeTravel: the timetravel.enable admin action turns on point-in-time queries
// and opQueryAsOf resolves the historical state over the wire; disabled it is refused.
func TestRPCTimeTravel(t *testing.T) {
	c, cleanup := privilegedPair(t)
	defer cleanup()
	ctx := context.Background()

	// Disabled by default.
	if _, err := c.QueryAsOfTable(ctx, DefaultTable, "true", 0, time.Now()); err == nil {
		t.Fatal("QueryAsOf before enable should error (time travel disabled)")
	}

	// Enable via the admin channel: 1h window, 1s checkpoint cadence.
	if msg, err := c.AdminTable(ctx, DefaultTable, "timetravel.enable", "3600", "1"); err != nil {
		t.Fatalf("timetravel.enable: %v", err)
	} else {
		t.Logf("enable -> %s", msg)
	}

	put := func(state string) {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = tx.NewClassAd(ctx, "k", `Name = "k"`+"\n"+`State = "`+state+`"`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	put("Idle")
	time.Sleep(1100 * time.Millisecond) // cross the 1s checkpoint boundary
	mid := time.Now()
	time.Sleep(1100 * time.Millisecond)
	put("Claimed")

	// AS OF mid: the ad was Idle then.
	rows, err := c.QueryAsOfTable(ctx, DefaultTable, `State == "Idle"`, 0, mid)
	if err != nil {
		t.Fatalf("QueryAsOf mid: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("AS OF mid: State==Idle rows = %d, want 1", len(rows))
	}
	// AS OF now: it is Claimed.
	rows, err = c.QueryAsOfTable(ctx, DefaultTable, `State == "Claimed"`, 0, time.Now())
	if err != nil {
		t.Fatalf("QueryAsOf now: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("AS OF now: State==Claimed rows = %d, want 1", len(rows))
	}

	// Disable and confirm it is refused again.
	if _, err := c.AdminTable(ctx, DefaultTable, "timetravel.disable"); err != nil {
		t.Fatalf("timetravel.disable: %v", err)
	}
	if _, err := c.QueryAsOfTable(ctx, DefaultTable, "true", 0, time.Now()); err == nil {
		t.Fatal("QueryAsOf after disable should error")
	}
}
