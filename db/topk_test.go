package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestArchiveTopK verifies the server-side ORDER BY <col> LIMIT k over an archive: it returns
// exactly k rows in sorted order, for both directions, and when the order column is not projected.
func TestArchiveTopK(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	match := 0
	for i := 0; i < n; i++ {
		proc := i % 4
		status := 2
		if i%5 == 0 {
			status = 4 // the filtered subset
			match++
		}
		if err := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d", i, proc, status)); err != nil {
			t.Fatal(err)
		}
	}

	// ORDER BY ClusterId DESC LIMIT 3 over JobStatus==4: the 3 highest ClusterIds that are ==4.
	// JobStatus==4 at i%5==0, i.e. ClusterId 0,5,10,...,495 -> top three are 495,490,485.
	rows, err := hist.TopK("JobStatus == 4", []string{"ClusterId", "ProcId"}, "ClusterId", true, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := clusterIds(t, rows)
	if want := []int64{495, 490, 485}; !eqI64(got, want) {
		t.Fatalf("DESC top3 = %v, want %v", got, want)
	}

	// ASC LIMIT 4: the 4 lowest.
	rows, err = hist.TopK("JobStatus == 4", []string{"ClusterId"}, "ClusterId", false, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := clusterIds(t, rows), []int64{0, 5, 10, 15}; !eqI64(got, want) {
		t.Fatalf("ASC bottom4 = %v, want %v", got, want)
	}

	// Order column NOT in the projection: it must be used for ordering and trimmed from results.
	rows, err = hist.TopK("JobStatus == 4", []string{"ProcId"}, "ClusterId", true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if len(r) != 1 { // only ProcId; the appended ClusterId was trimmed
			t.Fatalf("row has %d cols, want 1 (order col trimmed)", len(r))
		}
	}

	// k larger than the match set returns all matches, sorted.
	rows, err = hist.TopK("JobStatus == 4", []string{"ClusterId"}, "ClusterId", true, match+50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != match {
		t.Fatalf("k>matches returned %d rows, want %d", len(rows), match)
	}
}

// TestMutableTopK smoke-tests db.DB.TopK (the wrapper over the mutable QueryProject path).
func TestMutableTopK(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 50; i++ {
		st := 2
		if i%3 == 0 {
			st = 4
		}
		tx.NewClassAd(fmt.Sprintf("%d.0", i), mustAd(t, fmt.Sprintf("ClusterId = %d\nJobStatus = %d", i, st)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := d.TopK("JobStatus == 4", []string{"ClusterId"}, "ClusterId", true, 2)
	if err != nil {
		t.Fatal(err)
	}
	// JobStatus==4 at i%3==0: 0,3,...,48 -> top two are 48,45.
	if got, want := clusterIds(t, rows), []int64{48, 45}; !eqI64(got, want) {
		t.Fatalf("mutable DESC top2 = %v, want %v", got, want)
	}
}

func clusterIds(t *testing.T, rows [][]classad.Value) []int64 {
	t.Helper()
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		v, err := r[0].NumberValue()
		if err != nil {
			t.Fatalf("ClusterId not numeric: %v", err)
		}
		out = append(out, int64(v))
	}
	return out
}

func eqI64(a, b []int64) bool {
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
