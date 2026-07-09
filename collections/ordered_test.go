package collections

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// collectPartition snapshots oi and returns the keys of one partition in order,
// resuming after `resume` when non-nil.
func collectPartition(oi *orderedIndex, part orderVal, resume *orderEntry) []string {
	var got []string
	oi.ascendPartition(oi.snapshot(), part, resume, func(key string) bool {
		got = append(got, key)
		return true
	})
	return got
}

func eqStrings(a, b []string) bool { return reflect.DeepEqual(a, b) }

// TestOrderedIndexOrder: ascending numeric key, insertion order breaks ties.
func TestOrderedIndexOrder(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Keys: []SortKey{{Expr: "p"}}})
	p := strVal("only")
	oi.upsert("c", p, []orderVal{numVal(5)})
	oi.upsert("a", p, []orderVal{numVal(1)})
	oi.upsert("b1", p, []orderVal{numVal(3)}) // tie on 3, inserted first
	oi.upsert("b2", p, []orderVal{numVal(3)}) // tie on 3, inserted second
	got := collectPartition(oi, p, nil)
	if want := []string{"a", "b1", "b2", "c"}; !eqStrings(got, want) {
		t.Fatalf("order = %v, want %v (ties broken by insertion seq)", got, want)
	}
}

// TestOrderedIndexDesc: a descending key reverses the order; ties still ascend by seq.
func TestOrderedIndexDesc(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Keys: []SortKey{{Expr: "prio", Desc: true}}})
	p := strVal("u")
	oi.upsert("lo", p, []orderVal{numVal(1)})
	oi.upsert("hi", p, []orderVal{numVal(9)})
	oi.upsert("mid", p, []orderVal{numVal(5)})
	got := collectPartition(oi, p, nil)
	if want := []string{"hi", "mid", "lo"}; !eqStrings(got, want) {
		t.Fatalf("desc order = %v, want %v", got, want)
	}
}

// TestOrderedIndexPartitions: partitions are independent runs.
func TestOrderedIndexPartitions(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Partition: "Owner", Keys: []SortKey{{Expr: "p"}}})
	alice, bob := strVal("alice"), strVal("bob")
	oi.upsert("a2", alice, []orderVal{numVal(2)})
	oi.upsert("b1", bob, []orderVal{numVal(1)})
	oi.upsert("a1", alice, []orderVal{numVal(1)})
	oi.upsert("b2", bob, []orderVal{numVal(2)})
	if got, want := collectPartition(oi, alice, nil), []string{"a1", "a2"}; !eqStrings(got, want) {
		t.Fatalf("alice = %v, want %v", got, want)
	}
	if got, want := collectPartition(oi, bob, nil), []string{"b1", "b2"}; !eqStrings(got, want) {
		t.Fatalf("bob = %v, want %v", got, want)
	}
}

// TestOrderedIndexUpdatePreservesSeq: repositioning a member keeps its insertion
// sequence, so it does not jump ahead of earlier members it ties with.
func TestOrderedIndexUpdatePreservesSeq(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Keys: []SortKey{{Expr: "p"}}})
	p := strVal("u")
	oi.upsert("first", p, []orderVal{numVal(10)})  // seq 1
	oi.upsert("second", p, []orderVal{numVal(20)}) // seq 2
	// Move "second" to tie "first" on 10: it must sort AFTER "first" (later seq).
	oi.upsert("second", p, []orderVal{numVal(10)})
	if got, want := collectPartition(oi, p, nil), []string{"first", "second"}; !eqStrings(got, want) {
		t.Fatalf("after retie = %v, want %v (seq preserved)", got, want)
	}
	// A no-op update (same key/values) must not change anything.
	oi.upsert("first", p, []orderVal{numVal(10)})
	if got, want := collectPartition(oi, p, nil), []string{"first", "second"}; !eqStrings(got, want) {
		t.Fatalf("after no-op = %v, want %v", got, want)
	}
}

// TestOrderedIndexRemove: removed members disappear; a re-added key gets a fresh seq.
func TestOrderedIndexRemove(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Keys: []SortKey{{Expr: "p"}}})
	p := strVal("u")
	oi.upsert("x", p, []orderVal{numVal(1)})
	oi.upsert("y", p, []orderVal{numVal(2)})
	oi.remove("x")
	if got, want := collectPartition(oi, p, nil), []string{"y"}; !eqStrings(got, want) {
		t.Fatalf("after remove = %v, want %v", got, want)
	}
	oi.remove("missing") // no-op
	if got, want := collectPartition(oi, p, nil), []string{"y"}; !eqStrings(got, want) {
		t.Fatalf("after remove-missing = %v, want %v", got, want)
	}
}

// TestOrderedIndexSnapshotIsolation: a snapshot is unaffected by later writes.
func TestOrderedIndexSnapshotIsolation(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Keys: []SortKey{{Expr: "p"}}})
	p := strVal("u")
	oi.upsert("a", p, []orderVal{numVal(1)})
	oi.upsert("b", p, []orderVal{numVal(2)})
	snap := oi.snapshot()
	// Mutate the master after snapshotting.
	oi.upsert("c", p, []orderVal{numVal(3)})
	oi.remove("a")
	var got []string
	oi.ascendPartition(snap, p, nil, func(k string) bool { got = append(got, k); return true })
	if want := []string{"a", "b"}; !eqStrings(got, want) {
		t.Fatalf("snapshot = %v, want %v (isolated from later writes)", got, want)
	}
	if want := []string{"b", "c"}; !eqStrings(collectPartition(oi, p, nil), want) {
		t.Fatalf("live index did not reflect the writes")
	}
}

// TestOrderedIndexResume: iteration resumes strictly after a cursor entry.
func TestOrderedIndexResume(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Keys: []SortKey{{Expr: "p"}}})
	p := strVal("u")
	for i, k := range []string{"a", "b", "c", "d"} {
		oi.upsert(k, p, []orderVal{numVal(float64(i))})
	}
	// Resume after the entry for "b".
	cursor := oi.byKey["b"]
	if got, want := collectPartition(oi, p, &cursor), []string{"c", "d"}; !eqStrings(got, want) {
		t.Fatalf("resume-after-b = %v, want %v", got, want)
	}
}

// TestOrderedIndexConcurrent stresses the copy-on-write model: many writers churn
// the master (upsert/remove) while readers repeatedly snapshot and fully iterate.
// A snapshot must always iterate cleanly (no torn reads / races). Run with -race.
func TestOrderedIndexConcurrent(t *testing.T) {
	t.Parallel()
	oi := newOrderedIndex(OrderSpec{Partition: "Owner", Keys: []SortKey{{Expr: "p"}}})
	part := strVal("u")
	const keys = 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Writers.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				k := fmt.Sprintf("k%d", (i*7+w)%keys)
				if i%5 == 0 {
					oi.remove(k)
				} else {
					oi.upsert(k, part, []orderVal{numVal(float64(i % keys))})
				}
			}
		}(w)
	}
	// Readers: snapshot and iterate to exhaustion, checking per-partition order.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				oi.ascendPartition(oi.snapshot(), part, nil, func(string) bool { return true })
			}
		}()
	}
	// Let it run briefly, then stop the writers.
	for i := 0; i < 200; i++ {
		oi.snapshot()
	}
	close(stop)
	wg.Wait()
}
