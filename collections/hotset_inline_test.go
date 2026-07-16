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
	if got := c.HotAttrNames(); len(got) != 0 {
		t.Fatalf("expected no hot attrs initially, got %v", got)
	}
	if n := c.RefreshHotSet(800, 3); n != 3 {
		t.Fatalf("RefreshHotSet chose %d attrs, want 3 (it was a no-op on inline before the fix)", n)
	}
	hot := c.HotAttrNames()
	if len(hot) != 3 {
		t.Fatalf("hot attrs = %v, want 3", hot)
	}
	// AddHotAttrs also works in inline mode (merges).
	c.AddHotAttrs("customattr")
	if got := c.HotAttrNames(); !contains(got, "customattr") {
		t.Errorf("AddHotAttrs did not pin customattr in inline mode: %v", got)
	}
}
