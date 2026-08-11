package db

import (
	"fmt"
	"strconv"
	"testing"
)

// TestArchiveCountFastPath verifies an unconstrained COUNT(*) equals the archive's O(1)
// record count (the fast path), and that constrained COUNT and SUM -- which take the
// wire-native projected scan -- stay correct over a larger, multi-segment archive.
func TestArchiveCountFastPath(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize: 1 << 12, // small -> several sealed segments
		ValueAttrs:  []string{"ClusterId"},
		ZoneAttrs:   []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	sumOver := 0
	for i := 0; i < n; i++ {
		if err := hist.AppendOld(fmt.Sprintf("ClusterId = %d\nMemory = %d", i, i*2)); err != nil {
			t.Fatal(err)
		}
		if i >= 300 {
			sumOver += i * 2
		}
	}

	// Unconstrained COUNT(*) -> the O(1) record count.
	rows, err := hist.Aggregate("", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Values[0] != strconv.Itoa(n) {
		t.Fatalf("COUNT(*) = %+v, want %d", rows, n)
	}
	if rows[0].Values[0] != strconv.Itoa(hist.Count()) {
		t.Errorf("COUNT(*) %s != Count() %d", rows[0].Values[0], hist.Count())
	}
	// "true" is also match-all (same fast path).
	rows, _ = hist.Aggregate("true", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if rows[0].Values[0] != strconv.Itoa(n) {
		t.Errorf(`COUNT(*) WHERE true = %s, want %d`, rows[0].Values[0], n)
	}
	// A constant tautology folds to the SAME match-all fast path as "true", so
	// "1 == 1" and "true" agree with COUNT() -- they must never diverge.
	rows, _ = hist.Aggregate("1 == 1", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if rows[0].Values[0] != strconv.Itoa(n) {
		t.Errorf(`COUNT(*) WHERE 1 == 1 = %s, want %d (must equal WHERE true)`, rows[0].Values[0], n)
	}

	// Constrained COUNT(*) goes through the projected scan (not the fast path).
	rows, err = hist.Aggregate("ClusterId >= 300", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Values[0] != strconv.Itoa(n-300) {
		t.Errorf("COUNT(*) WHERE ClusterId>=300 = %s, want %d", rows[0].Values[0], n-300)
	}

	// SUM over a projected attribute (regression: the projected scan reads Memory correctly).
	rows, err = hist.Aggregate("ClusterId >= 300", nil, []AggSpec{{Func: AggSum, Arg: "Memory"}})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Values[0] != strconv.Itoa(sumOver) {
		t.Errorf("SUM(Memory) WHERE ClusterId>=300 = %s, want %d", rows[0].Values[0], sumOver)
	}
}
