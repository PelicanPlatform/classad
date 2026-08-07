package collections

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// fillDemandCollection returns a persistent collection with ads and recorded demand on
// Owner (equality) and ClusterId (range).
func fillDemandCollection(t *testing.T, dir string, queries int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Owner="user%d" ]`, i, i%7))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for range queries {
		drainQuery(t, c, `Owner == "user3"`)
		drainQuery(t, c, `ClusterId > 100`)
	}
	return c
}

func demandOf(t *testing.T, c *Collection, attr string) (eq, rng int64) {
	t.Helper()
	v, ok := c.demand.m.Load(attr)
	if !ok {
		return 0, 0
	}
	d := v.(*demandCounts)
	return d.eq.Load(), d.rng.Load()
}

// TestDemandSurvivesReopen is the point of the checkpoint: a restart must not erase the
// evidence that justified the current index set. Without it every attribute reads zero on
// reopen, which is indistinguishable from "no query has ever wanted this".
func TestDemandSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	c := fillDemandCollection(t, dir, 20)
	eq, rng := demandOf(t, c, "owner")
	if eq != 20 {
		t.Fatalf("pre-save owner eq = %d, want 20", eq)
	}
	c.SaveDemand()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	gotEq, _ := demandOf(t, c2, "owner")
	if gotEq != eq {
		t.Errorf("owner eq after reopen = %d, want %d", gotEq, eq)
	}
	_, cid := demandOf(t, c2, "clusterid")
	if cid == 0 {
		t.Errorf("clusterid range demand lost across reopen (had %d)", rng)
	}
	// The reopened collection must be able to act on it: suggestions come back too.
	var got []string
	for _, s := range c2.SuggestIndexes(1000) {
		got = append(got, s.Attr+":"+s.Kind)
	}
	if len(got) != 2 {
		t.Errorf("suggestions after reopen = %v, want ClusterId and Owner", got)
	}
}

// TestDemandDecaysAcrossDowntime checks the other half: a checkpoint is not a permanent
// record. Demand written long ago comes back scaled down by the elapsed time, so an index
// justified by a burst last quarter does not stay justified forever.
func TestDemandDecaysAcrossDowntime(t *testing.T) {
	dir := t.TempDir()
	c := fillDemandCollection(t, dir, 64)
	c.SaveDemand()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Backdate the checkpoint by three half-lives: 64 -> 8.
	path := filepath.Join(dir, demandFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p persistedDemand
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	p.SavedUnix = time.Now().Add(-3 * defaultDemandHalfLife).UnixNano()
	if data, err = json.Marshal(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	eq, _ := demandOf(t, c2, "owner")
	if eq < 6 || eq > 10 {
		t.Errorf("owner eq after 3 half-lives = %d, want ~8 (64/2^3)", eq)
	}
}

// TestDemandForgottenWhenFullyDecayed: an attribute nothing has queried for long enough
// drops out entirely rather than lingering in the checkpoint forever.
func TestDemandForgottenWhenFullyDecayed(t *testing.T) {
	dir := t.TempDir()
	c := fillDemandCollection(t, dir, 4)
	c.SaveDemand()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, demandFile)
	data, _ := os.ReadFile(path)
	var p persistedDemand
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	p.SavedUnix = time.Now().Add(-30 * defaultDemandHalfLife).UnixNano()
	data, _ = json.Marshal(p)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if eq, rng := demandOf(t, c2, "owner"); eq != 0 || rng != 0 {
		t.Errorf("owner demand after 30 half-lives = (%d,%d), want dropped", eq, rng)
	}
}

// TestDemandDowntimeIsNotObservation: restoring a week-old checkpoint must not let a
// daemon that has been up for a moment claim it has been watching for a week. Drop
// decisions are gated on observation time precisely because a zero from a short window
// means nothing, and downtime is not a window.
func TestDemandDowntimeIsNotObservation(t *testing.T) {
	dir := t.TempDir()
	c := fillDemandCollection(t, dir, 200)
	c.SaveDemand()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, demandFile)
	data, _ := os.ReadFile(path)
	var p persistedDemand
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	p.SavedUnix = time.Now().Add(-24 * time.Hour).UnixNano()
	data, _ = json.Marshal(p)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(Options{Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	// The queries were observed in well under a second of real running time, so a
	// one-hour window must not be satisfied by the day of downtime.
	if c2.demand.hasDropEvidence(1, time.Hour) {
		t.Error("a day of downtime counted as observation time")
	}
	// ...but a window shorter than the real observation is satisfied, so the gate is not
	// simply always-false.
	if !c2.demand.hasDropEvidence(1, time.Nanosecond) {
		t.Error("observation time not accumulated at all")
	}
}

// TestDemandDecayDisabled: a negative half-life keeps the old accumulate-forever behaviour
// for a caller that wants it.
func TestDemandDecayDisabled(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, SegmentSize: 1 << 16, DemandHalfLife: -1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		ad, _ := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; Owner="user%d" ]`, i, i%7))
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for range 16 {
		drainQuery(t, c, `Owner == "user3"`)
	}
	c.SaveDemand()
	c.Close()

	path := filepath.Join(dir, demandFile)
	data, _ := os.ReadFile(path)
	var p persistedDemand
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	p.SavedUnix = time.Now().Add(-10 * defaultDemandHalfLife).UnixNano()
	data, _ = json.Marshal(p)
	os.WriteFile(path, data, 0o644)

	c2, err := Open(Options{Dir: dir, SegmentSize: 1 << 16, DemandHalfLife: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if eq, _ := demandOf(t, c2, "owner"); eq != 16 {
		t.Errorf("owner eq with decay disabled = %d, want 16 (undecayed)", eq)
	}
}

func TestDecayFactor(t *testing.T) {
	hl := time.Hour
	for _, tc := range []struct {
		age  time.Duration
		want float64
	}{
		{0, 1}, {hl, 0.5}, {2 * hl, 0.25}, {-time.Hour, 1},
	} {
		if got := decayFactor(tc.age, hl); got < tc.want-1e-9 || got > tc.want+1e-9 {
			t.Errorf("decayFactor(%v) = %v, want %v", tc.age, got, tc.want)
		}
	}
	if got := decayFactor(time.Hour, 0); got != 1 {
		t.Errorf("decayFactor with no half-life = %v, want 1 (no decay)", got)
	}
}

// TestDemandNotErodedByFrequentSaves guards the trap that decaying an INTEGER counter is
// not free even when the factor is ~1: rounding down a fraction of a count on every
// checkpoint erodes demand at a rate set by how often maintenance runs rather than by the
// configured half-life. A day's worth of maintenance passes must leave a week-half-life
// counter essentially untouched.
func TestDemandNotErodedByFrequentSaves(t *testing.T) {
	dir := t.TempDir()
	c := fillDemandCollection(t, dir, 20)
	defer c.Close()
	before, _ := demandOf(t, c, "owner")
	for range 50 {
		c.SaveDemand()
	}
	after, _ := demandOf(t, c, "owner")
	if after != before {
		t.Errorf("owner eq eroded by 50 checkpoints: %d -> %d (half-life is %v)",
			before, after, defaultDemandHalfLife)
	}
}
