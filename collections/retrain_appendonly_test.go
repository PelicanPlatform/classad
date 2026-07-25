package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// scanAllN returns every record's N value from a full scan, in scan order.
func scanAllN(t *testing.T, c *Collection) []int64 {
	t.Helper()
	var out []int64
	for ad := range c.Scan() {
		v, _ := ad.EvaluateAttrInt("N")
		out = append(out, v)
	}
	return out
}

// TestAppendOnlyRetrain verifies RetrainDict works on an append-only collection: it trains
// a dictionary, recompresses every segment in place preserving all records and order, keeps
// queries correct, and survives a reopen. The retrained data should be no larger than before.
func TestAppendOnlyRetrain(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 13, ValueAttrs: []string{"N"}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	const n = 500
	// Repetitive, dictionary-friendly ads.
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(
			`[ N=%d; Owner="user_%d"; JobStatus="Completed"; Cmd="/usr/bin/some_long_repeated_command_path"; Args="--flag=value --other=thing" ]`,
			i, i%20))
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
	}
	before := scanAllN(t, c)
	if len(before) != n {
		t.Fatalf("before retrain: %d records, want %d", len(before), n)
	}

	sz, err := c.RetrainDict(1000)
	if err != nil {
		t.Fatalf("RetrainDict on append-only collection: %v", err)
	}
	if sz == 0 {
		t.Fatal("RetrainDict returned zero dictionary size")
	}

	// Every record survives, in the same scan order, with intact attributes.
	after := scanAllN(t, c)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("retrain changed the record set/order:\nbefore=%v\nafter=%v", before, after)
	}
	if got := c.Len(); got != n {
		t.Errorf("Len after retrain = %d, want %d", got, n)
	}
	// Attributes are intact (spot-check an indexed query).
	cnt := 0
	for ad := range c.Query(mustQ(t, `N >= 250`)) {
		if v, _ := ad.EvaluateAttrInt("N"); v < 250 {
			t.Fatalf("query returned N=%d < 250", v)
		}
		own, _ := ad.EvaluateAttrString("Owner")
		if !strings.HasPrefix(own, "user_") {
			t.Fatalf("Owner attribute corrupted after retrain: %q", own)
		}
		cnt++
	}
	if cnt != 250 {
		t.Errorf("N>=250 after retrain returned %d, want 250", cnt)
	}
	c.Close()

	// Reopen: the recompressed segments recover with the new dictionary, data intact.
	c2 := open()
	defer c2.Close()
	reopened := scanAllN(t, c2)
	if fmt.Sprint(reopened) != fmt.Sprint(before) {
		t.Fatalf("after reopen record set/order differs from pre-retrain")
	}
}

// TestAppendOnlyRewrite verifies Rewrite recompresses/re-encodes an append-only collection
// in place, preserving every record and keeping queries correct.
func TestAppendOnlyRewrite(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 300
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; Owner="u%d" ]`, i, i%5))
		c.Put([]byte("k"), ad)
	}
	before := scanAllN(t, c)
	got := c.Rewrite()
	if got != n {
		t.Errorf("Rewrite reported %d, want %d", got, n)
	}
	after := scanAllN(t, c)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("rewrite changed the record set/order")
	}
	if c.Len() != n {
		t.Errorf("Len after rewrite = %d, want %d", c.Len(), n)
	}
	// A query still returns correct results after rewrite.
	cnt := 0
	for range c.Query(mustQ(t, `Owner == "u2"`)) {
		cnt++
	}
	if cnt != n/5 {
		t.Errorf(`Owner=="u2" after rewrite returned %d, want %d`, cnt, n/5)
	}
}

// TestAppendOnlyAddIndex verifies AddIndex/DropIndex/Reindex work on an append-only
// collection: adding an index makes a query on that attribute return correct results.
func TestAppendOnlyAddIndex(t *testing.T) {
	c := New(Options{AppendOnly: true, SegmentSize: 1 << 12})
	const n = 400
	for i := 0; i < n; i++ {
		ad, _ := classad.Parse(fmt.Sprintf(`[ N=%d; G=%d ]`, i, i%4))
		c.Put([]byte("k"), ad)
	}
	if !c.AddIndex(nil, []string{"G"}) {
		t.Fatal("AddIndex returned false on append-only collection")
	}
	c.Reindex()
	// Query the newly-indexed attribute: correct results.
	var got []int64
	for ad := range c.Query(mustQ(t, `G == 3`)) {
		v, _ := ad.EvaluateAttrInt("N")
		got = append(got, v)
	}
	if len(got) != n/4 {
		t.Fatalf("G==3 returned %d, want %d", len(got), n/4)
	}
	if !c.DropIndex("G") {
		t.Error("DropIndex returned false")
	}
	// Still correct after dropping the index (full scan).
	cnt := 0
	for range c.Query(mustQ(t, `G == 3`)) {
		cnt++
	}
	if cnt != n/4 {
		t.Errorf("after DropIndex G==3 returned %d, want %d", cnt, n/4)
	}
}
