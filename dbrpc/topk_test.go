package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// topKClusterIds parses each returned old-ClassAd row and pulls out ClusterId, in order.
func topKClusterIds(t *testing.T, rows []string) []int64 {
	t.Helper()
	out := make([]int64, 0, len(rows))
	for i, r := range rows {
		ad, err := classad.ParseOld(r)
		if err != nil {
			t.Fatalf("parse row %d %q: %v", i, r, err)
		}
		cid, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			t.Fatalf("row %d missing ClusterId: %q", i, r)
		}
		out = append(out, cid)
	}
	return out
}

func eqInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestArchiveTopKOverRPC verifies the server-side ORDER BY <col> LIMIT k op over an archive: only
// k rows cross the wire, in sorted order, for both directions, including when the order column is
// not projected (it must not appear in the rows).
func TestArchiveTopKOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	const n = 300
	match := 0
	for i := 0; i < n; i++ {
		status := 2
		if i%5 == 0 {
			status = 4
			match++
		}
		ad := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nOwner = \"u%d\"", i, i%4, status, i%7)
		if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	// DESC LIMIT 3 over JobStatus==4 (ClusterId 0,5,...,295): the three highest are 295,290,285.
	rows, err := c.TopK(ctx, "history", "JobStatus == 4", []string{"ClusterId", "Owner"}, "ClusterId", true, 3)
	if err != nil {
		t.Fatalf("TopK desc: %v", err)
	}
	if got, want := topKClusterIds(t, rows), []int64{295, 290, 285}; !eqInts(got, want) {
		t.Fatalf("DESC top3 = %v, want %v", got, want)
	}
	// The requested Owner must be present.
	if ad, _ := classad.ParseOld(rows[0]); ad != nil {
		if _, ok := ad.Lookup("Owner"); !ok {
			t.Errorf("row missing projected Owner: %q", rows[0])
		}
	}

	// ASC LIMIT 4: the four lowest.
	rows, err = c.TopK(ctx, "history", "JobStatus == 4", []string{"ClusterId"}, "ClusterId", false, 4)
	if err != nil {
		t.Fatalf("TopK asc: %v", err)
	}
	if got, want := topKClusterIds(t, rows), []int64{0, 5, 10, 15}; !eqInts(got, want) {
		t.Fatalf("ASC bottom4 = %v, want %v", got, want)
	}

	// Order column NOT projected: it orders the rows but must not appear in them.
	rows, err = c.TopK(ctx, "history", "JobStatus == 4", []string{"Owner"}, "ClusterId", true, 2)
	if err != nil {
		t.Fatalf("TopK order-col-not-projected: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		ad, perr := classad.ParseOld(r)
		if perr != nil {
			t.Fatalf("parse %q: %v", r, perr)
		}
		if _, ok := ad.Lookup("ClusterId"); ok {
			t.Errorf("un-projected order column ClusterId leaked into the row: %q", r)
		}
		if _, ok := ad.Lookup("Owner"); !ok {
			t.Errorf("row missing projected Owner: %q", r)
		}
	}

	// k larger than the match set returns all matches, sorted.
	rows, err = c.TopK(ctx, "history", "JobStatus == 4", []string{"ClusterId"}, "ClusterId", true, match+50)
	if err != nil {
		t.Fatalf("TopK k>matches: %v", err)
	}
	if len(rows) != match {
		t.Fatalf("k>matches returned %d rows, want %d", len(rows), match)
	}
}

// TestMutableTopKOverRPC smoke-tests the op against a mutable table.
func TestMutableTopKOverRPC(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 40; i++ {
		st := 2
		if i%3 == 0 {
			st = 4
		}
		tx.NewClassAdOld(fmt.Sprintf("%d.0", i), fmt.Sprintf("ClusterId = %d\nJobStatus = %d", i, st))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	// JobStatus==4 at i%3==0: 0,3,...,39 -> top two are 39,36.
	rows, err := c.TopK(context.Background(), DefaultTable, "JobStatus == 4", []string{"ClusterId"}, "ClusterId", true, 2)
	if err != nil {
		t.Fatalf("TopK: %v", err)
	}
	if got, want := topKClusterIds(t, rows), []int64{39, 36}; !eqInts(got, want) {
		t.Fatalf("mutable DESC top2 = %v, want %v", got, want)
	}
}

// TestTopKPrivateAttrRefused verifies the projection/order-key private-attr gate: an unprivileged
// connection must not read or order by a private attribute (TopK's underlying QueryProject does not
// redact, so the op guards it directly).
func TestTopKPrivateAttrRefused(t *testing.T) {
	c, cleanup := privateGatePair(t, true) // read-only, unprivileged
	defer cleanup()
	ctx := context.Background()

	// Private attribute projected.
	if _, err := c.TopK(ctx, DefaultTable, "true", []string{"Cpus", "ClaimId"}, "Cpus", true, 5); err == nil {
		t.Error("TopK projecting a private attribute was allowed")
	} else if !strings.Contains(err.Error(), "private") {
		t.Errorf("wrong refusal reason for private projection: %v", err)
	}

	// Private attribute as the order key.
	if _, err := c.TopK(ctx, DefaultTable, "true", []string{"Cpus"}, "ClaimId", true, 5); err == nil {
		t.Error("TopK ordering by a private attribute was allowed")
	} else if !strings.Contains(err.Error(), "private") {
		t.Errorf("wrong refusal reason for private order key: %v", err)
	}

	// A privileged connection may do both.
	cp, cleanup2 := privateGatePair(t, false)
	defer cleanup2()
	if _, err := cp.TopK(ctx, DefaultTable, "true", []string{"Cpus", "ClaimId"}, "Cpus", true, 5); err != nil {
		t.Errorf("privileged TopK refused: %v", err)
	}
}
