package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestAppendOnlyMode covers the append-log Collection: every Put appends a record (no per-key
// supersession), the collection is single-shard, and Compact/Delete/Rewrite/RetrainDict are
// inert so the log is never rewritten.
func TestAppendOnlyMode(t *testing.T) {
	c := New(Options{AppendOnly: true, Shards: 8}) // Shards forced to 1
	defer c.Close()
	if got := len(c.shards); got != 1 {
		t.Fatalf("append-only shards = %d, want 1 (forced)", got)
	}

	// Two Puts to the SAME key both persist (no supersession) -- append semantics.
	for i, text := range []string{
		`[ ClusterId = 1; JobStatus = 4; Owner = "alice" ]`,
		`[ ClusterId = 1; JobStatus = 3; Owner = "alice" ]`, // same key, different content
		`[ ClusterId = 2; JobStatus = 4; Owner = "bob" ]`,
	} {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		key := []byte("1.0")
		if i == 2 {
			key = []byte("2.0")
		}
		if err := c.Put(key, ad); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	count := func() int {
		n := 0
		for range c.Scan() {
			n++
		}
		return n
	}
	if got := count(); got != 3 {
		t.Fatalf("scan returned %d records, want 3 (all appends, no dedup)", got)
	}

	// Maintenance that would rewrite/renumber the log is inert.
	if n := c.Compact(); n != 0 {
		t.Errorf("Compact on append-only did work (%d); want no-op", n)
	}
	if n := c.Rewrite(); n != 0 {
		t.Errorf("Rewrite on append-only did work (%d); want no-op", n)
	}
	if _, err := c.RetrainDict(1000); err == nil {
		t.Error("RetrainDict on append-only should error (recompression path is not append-safe)")
	}
	if c.Delete([]byte("1.0")) {
		t.Error("Delete on append-only should be a no-op")
	}
	if got := count(); got != 3 {
		t.Errorf("after inert maintenance scan = %d, want 3 (log unchanged)", got)
	}
}
