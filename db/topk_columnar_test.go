package db

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// topkRow is one appended record, kept so the test can compute the expected top-K independently of
// the store.
type topkRow struct {
	cid    int64
	proc   int
	status int
	owner  string
}

// buildTopKArchive appends n spread-out (non-monotonic ClusterId) records and returns the archive
// plus the rows, columnarizing when enable is set (small segments so several seal and get columnar
// blocks -- exercising the columnar cutoff path, not just the active-segment fallback).
func buildTopKArchive(t *testing.T, n int, enable bool) (*ArchiveTable, []topkRow) {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize: 1 << 15, // small -> multiple sealed, columnarized segments
		ValueAttrs:  []string{"ClusterId"},
		ZoneAttrs:   []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]topkRow, n)
	for i := 0; i < n; i++ {
		cid := int64((i * 7919) % 1000003) // spread; the max is NOT the last-appended row
		proc := i % 7
		status := 2
		if i%5 == 0 {
			status = 4
		}
		owner := fmt.Sprintf("u%d", i%13)
		rows[i] = topkRow{cid, proc, status, owner}
		if err := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nOwner = %q",
			cid, proc, status, owner)); err != nil {
			t.Fatal(err)
		}
	}
	if enable {
		hist.BuildAndEnableSchemaScan(4096, 8)
		if !hist.SchemaScanInfo().Enabled {
			t.Fatal("schema-scan accelerator did not enable; the columnar top-K path would not be exercised")
		}
	}
	return hist, rows
}

// bruteTopCids returns the ClusterId values of the top-k matching rows, sorted best-first.
func bruteTopCids(rows []topkRow, keep func(topkRow) bool, desc bool, k int) []int64 {
	var cids []int64
	for _, r := range rows {
		if keep(r) {
			cids = append(cids, r.cid)
		}
	}
	sort.Slice(cids, func(i, j int) bool {
		if desc {
			return cids[i] > cids[j]
		}
		return cids[i] < cids[j]
	})
	if len(cids) > k {
		cids = cids[:k]
	}
	return cids
}

func gotCids(t *testing.T, rows [][]classad.Value) []int64 {
	t.Helper()
	out := make([]int64, len(rows))
	for i, r := range rows {
		v, err := r[0].NumberValue()
		if err != nil {
			t.Fatalf("row %d ClusterId not numeric: %v", i, err)
		}
		out[i] = int64(v)
	}
	return out
}

// TestArchiveTopKColumnarMatchesBrute is the correctness guard for the two-pass columnar top-K: for
// every shape it must return exactly what a brute-force top-K over the same data would -- on a
// columnarized archive (the fast cutoff path) AND on a non-columnarized one (the fallback), which
// must agree with each other and with brute. A wrong cutoff would drop or admit the wrong rows.
func TestArchiveTopKColumnarMatchesBrute(t *testing.T) {
	const n = 4000
	fast, rows := buildTopKArchive(t, n, true)
	slow, _ := buildTopKArchive(t, n, false)

	type filter struct {
		name string
		expr string
		keep func(topkRow) bool
	}
	filters := []filter{
		{"all", "true", func(topkRow) bool { return true }},
		{"proc<5", "ProcId < 5", func(r topkRow) bool { return r.proc < 5 }},
		{"status==4", "JobStatus == 4", func(r topkRow) bool { return r.status == 4 }},
		// A filter that excludes the row holding the global max ClusterId, so the cutoff MUST reflect
		// the filtered max, not the global one.
		{"proc==0", "ProcId == 0", func(r topkRow) bool { return r.proc == 0 }},
	}
	for _, f := range filters {
		for _, desc := range []bool{true, false} {
			for _, k := range []int{1, 3, 50, n + 10} {
				want := bruteTopCids(rows, f.keep, desc, k)
				for _, tbl := range []struct {
					label string
					at    *ArchiveTable
				}{{"columnar", fast}, {"fallback", slow}} {
					res, err := tbl.at.TopK(f.expr, []string{"ClusterId"}, "ClusterId", desc, k)
					if err != nil {
						t.Fatalf("%s/%s/desc=%v/k=%d: %v", tbl.label, f.name, desc, k, err)
					}
					got := gotCids(t, res)
					if !eqI64s(got, want) {
						t.Fatalf("%s/%s/desc=%v/k=%d: got %v, want %v", tbl.label, f.name, desc, k, got, want)
					}
				}
			}
		}
	}

	// Argmax reassembly: selecting a NON-order column with LIMIT 1 must return that column from the
	// actual max-ClusterId row (the second pass reassembles the winner, not just its order value).
	maxRow := rows[0]
	for _, r := range rows {
		if r.proc < 5 && r.cid > maxRow.cid {
			maxRow = r
		}
	}
	res, err := fast.TopK("ProcId < 5", []string{"ClusterId", "Owner"}, "ClusterId", true, 1)
	if err != nil || len(res) != 1 {
		t.Fatalf("argmax query: err=%v rows=%d", err, len(res))
	}
	if cid, _ := res[0][0].NumberValue(); int64(cid) != maxRow.cid {
		t.Fatalf("argmax ClusterId = %v, want %d", cid, maxRow.cid)
	}
	if owner, _ := res[0][1].StringValue(); owner != maxRow.owner {
		t.Fatalf("argmax Owner = %q, want %q (the max row's owner, i.e. the winner was reassembled)", owner, maxRow.owner)
	}
}

func eqI64s(a, b []int64) bool {
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
