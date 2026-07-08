package collections

import (
	"context"
	"testing"
	"time"
)

// TestChainedWatchFlattened verifies watch events for children carry inherited
// (flattened) attributes and that cluster ads are hidden from the stream.
func TestChainedWatchFlattened(t *testing.T) {
	c := New(Options{
		Shards: 4, WatchHistory: 1024,
		ParentKeyFor: jobParentKey, IsStructural: jobStructural,
	})
	c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=42; Owner="alice"]`))
	c.Put([]byte("1.0"), mustAd(t, `[ProcId=0]`))
	c.Put([]byte("1.1"), mustAd(t, `[ProcId=1]`))

	events, _ := collectCatchUp(t, c, nil)
	nUpsert := 0
	for _, ev := range events {
		if ev.Kind != WatchUpsert {
			continue
		}
		nUpsert++
		if d, _ := ev.Ad.EvaluateAttrInt("DAGManJobId"); d != 42 {
			t.Errorf("catch-up upsert %s: DAGManJobId=%d, want 42 (not flattened)", ev.Key, d)
		}
	}
	if nUpsert != 2 {
		t.Errorf("catch-up upserts=%d, want 2 (two jobs; cluster ad hidden)", nUpsert)
	}
}

// TestChainedWatchFanout verifies a change to an inherited parent attribute
// fans out to the children (with the new value), while a change to only a
// parent-private attribute does not.
func TestChainedWatchFanout(t *testing.T) {
	c := New(Options{
		Shards: 4, WatchHistory: 1024,
		ParentKeyFor: jobParentKey, IsStructural: jobStructural,
		ParentPrivateAttrs: []string{"JobMaterializeNextProcId"},
	})
	c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=42; JobMaterializeNextProcId=0]`))
	c.Put([]byte("1.0"), mustAd(t, `[ProcId=0]`))
	c.Put([]byte("1.1"), mustAd(t, `[ProcId=1]`))
	_, cursor := collectCatchUp(t, c, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := c.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		kind WatchKind
		key  string
		dag  int64
	}
	evs := make(chan rec, 64)
	synced := make(chan struct{})
	go func() {
		for ev := range seq {
			if ev.Kind == WatchSynced {
				close(synced)
				continue
			}
			d := int64(-1)
			if ev.Ad != nil {
				d, _ = ev.Ad.EvaluateAttrInt("DAGManJobId")
			}
			evs <- rec{ev.Kind, string(ev.Key), d}
		}
	}()
	<-synced

	// A change to only a parent-private attribute must NOT fan out.
	c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=42; JobMaterializeNextProcId=1]`))
	select {
	case e := <-evs:
		t.Fatalf("private-only parent change fanned out: %v %s", e.kind, e.key)
	case <-time.After(250 * time.Millisecond):
	}

	// A change to an inherited attribute must fan out to both children.
	c.Put([]byte("1.-1"), mustAd(t, `[DAGManJobId=99; JobMaterializeNextProcId=2]`))
	got := map[string]int64{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case e := <-evs:
			if e.kind == WatchUpsert {
				got[e.key] = e.dag
			}
		case <-deadline:
			t.Fatalf("fan-out incomplete: got %v, want 1.0 and 1.1 at 99", got)
		}
	}
	if got["1.0"] != 99 || got["1.1"] != 99 {
		t.Errorf("fan-out values = %v, want both procs at DAGManJobId 99", got)
	}
}
