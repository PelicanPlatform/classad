package collections

import (
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestTopKOrderThreshold pins the columnar cutoff scan: for ORDER BY ClusterId {DESC|ASC} LIMIT k
// over ProcId<5, it must report the true k-th ClusterId and the exact count of matching numeric
// rows -- combining the columnar sealed segments and the active-segment row fallback -- and must
// decline (ok=false) for an order attribute that is not a numeric schema field.
func TestTopKOrderThreshold(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 15,
		ValueAttrs: []string{"ClusterId"}, ZoneAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	const n = 3000
	var match []int64 // ClusterIds with ProcId < 5
	for i := 0; i < n; i++ {
		cid := int64((i * 7919) % 1000003) // spread; max is not the last row
		proc := i % 7
		ad := classad.New()
		_ = ad.Set("ClusterId", cid)
		_ = ad.Set("ProcId", int64(proc))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
		if proc < 5 {
			match = append(match, cid)
		}
	}
	if !a.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	q, err := vm.Parse("ProcId < 5")
	if err != nil {
		t.Fatal(err)
	}

	check := func(desc bool, k int) {
		cids := append([]int64(nil), match...)
		sort.Slice(cids, func(i, j int) bool {
			if desc {
				return cids[i] > cids[j]
			}
			return cids[i] < cids[j]
		})
		th, seen, ok := a.TopKOrderThreshold(q, "ClusterId", desc, k, nil)
		if !ok {
			t.Fatalf("desc=%v k=%d: columnar cutoff unavailable", desc, k)
		}
		if seen != len(cids) {
			t.Errorf("desc=%v k=%d: seen=%d, want %d", desc, k, seen, len(cids))
		}
		if int64(th) != cids[k-1] {
			t.Errorf("desc=%v k=%d: threshold=%v, want %d (the k-th value)", desc, k, th, cids[k-1])
		}
	}
	check(true, 1)  // max
	check(false, 1) // min
	check(true, 10)
	check(false, 25)

	// An order attribute that is not a numeric schema field: decline, so the caller uses the row path.
	if _, _, ok := a.TopKOrderThreshold(q, "NotAField", true, 1, nil); ok {
		t.Error("expected ok=false for a non-numeric order attribute")
	}
}
