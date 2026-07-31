package changefeed

import (
	"testing"
	"time"
)

// TestRegistryFloorAndEvict: the GC floor is the min ack over LIVE subscribers, and an expired
// lease neither holds the floor nor survives Evict.
func TestRegistryFloorAndEvict(t *testing.T) {
	r := &MemRegistry{LeaseTTL: 100 * time.Millisecond}
	t0 := time.Unix(1_700_000_000, 0)

	r.Ack("history", "a", 100_000, t0)
	r.Ack("history", "b", 50_000, t0)
	if f, held := r.Floor("history", t0); !held || f != 50_000 {
		t.Fatalf("floor = %d held=%v, want 50000 true (min of a,b)", f, held)
	}

	// b advances past a; the floor rises to a's ack.
	r.Ack("history", "b", 200_000, t0)
	if f, _ := r.Floor("history", t0); f != 100_000 {
		t.Fatalf("floor = %d, want 100000 (a now lowest)", f)
	}

	// Time passes beyond the lease; b renews, a goes silent. a no longer holds the floor.
	later := t0.Add(150 * time.Millisecond)
	r.Renew("history", "b", later)
	if f, held := r.Floor("history", later); !held || f != 200_000 {
		t.Fatalf("floor = %d held=%v, want 200000 true (a's expired lease released)", f, held)
	}

	// Evict drops the expired a; b remains.
	if n := r.Evict(later); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if subs := r.Subscribers("history", later); len(subs) != 1 || subs[0].Subscriber != "b" {
		t.Fatalf("subscribers = %+v, want just b", subs)
	}

	// With no live subscribers at all, nothing holds the floor.
	r.Evict(later.Add(time.Hour))
	if _, held := r.Floor("history", later.Add(time.Hour)); held {
		t.Fatal("no live subscribers should mean no retention hold")
	}
}
