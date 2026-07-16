package collections

import (
	"fmt"
	"os"
	"testing"
)

// TestRefreshHotSetInline: on a persistent (inline) collection, RefreshHotSet and
// AddHotAttrs actually install a hot set. Regression for the id-based ForEach counting
// nothing on inline ads (no intern ids), which made .refreshhot a silent no-op.
func TestRefreshHotSetInline(t *testing.T) {
	dir, err := os.MkdirTemp("", "hotinline")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c, err := Open(Options{Dir: dir, Shards: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.inline {
		t.Fatal("expected an inline (persistent) collection")
	}
	for i := 0; i < 800; i++ {
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, fmt.Sprintf(
			`[ Id=%d; Owner="u%d"; Arch="X86_64"; OpSys="LINUX"; Memory=%d ]`, i, i%20, i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.HotAttrNames(); !contains(got, "Requirements") || !contains(got, "Rank") {
		t.Fatalf("expected Requirements/Rank hot initially, got %v", got)
	}
	if n := c.RefreshHotSet(800, 3); n == 0 {
		t.Fatalf("RefreshHotSet was a no-op on inline (the bug); chose 0")
	}
	hot := c.HotAttrNames()
	if !contains(hot, "Requirements") || !contains(hot, "Rank") {
		t.Fatalf("hot attrs must include the defaults, got %v", hot)
	}
	// AddHotAttrs also works in inline mode (merges).
	c.AddHotAttrs("customattr")
	if got := c.HotAttrNames(); !contains(got, "customattr") {
		t.Errorf("AddHotAttrs did not pin customattr in inline mode: %v", got)
	}
}

// TestRefreshHotSetRanksByAccess: the hot set is the attributes the workload actually
// reads (Requirements/Rank references), not every attribute present in the ads. Regression
// for presence-ranking, which tied every attribute and truncated alphabetically.
func TestRefreshHotSetRanksByAccess(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, ValueAttrs: []string{"Memory"}, CategoricalAttrs: []string{"Arch", "State"}})
	for i := 0; i < 1500; i++ {
		c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, fmt.Sprintf(
			`[ Id=%d; Aardvark="x"; Beetle="y"; Cat="z"; Memory=%d; Arch="X86_64"; State="Unclaimed"; Requirements=true ]`,
			i, (i%8+1)*1024)))
	}
	c.Reindex()
	job := mustAd(t, `[ RequestMemory=2048;
		Requirements = (TARGET.Memory >= RequestMemory) && (TARGET.Arch == "X86_64") && (TARGET.State == "Unclaimed");
		Rank = TARGET.Memory ]`)
	for i := 0; i < 30; i++ {
		_, _ = c.MatchSortedRankedFiltered(job, "", 0)
	}
	c.RefreshHotSet(1500, 32) // topN 32 but only Memory/Arch/State (+ the defaults) are read
	hot := c.HotAttrNames()
	// The read attributes and the always-hot Requirements/Rank defaults are present...
	for _, want := range []string{"Arch", "Memory", "State", "Requirements", "Rank"} {
		if !contains(hot, want) {
			t.Errorf("hot set %v missing expected attribute %s", hot, want)
		}
	}
	// ...but never-read, never-referenced attributes are not (no presence padding).
	for _, notWant := range []string{"Aardvark", "Beetle", "Cat"} {
		if contains(hot, notWant) {
			t.Errorf("hot set %v includes never-read attribute %s (presence padding)", hot, notWant)
		}
	}
}
