package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// columnarizeShard rewrites every eligible sealed segment of sh in place and returns how many were
// replaced, so a test can exercise the read paths against a live shard holding the columnar shape.
func columnarizeShard(t *testing.T, c *Collection, sh *shard, s *adSchema, hot []int) int {
	t.Helper()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	n := 0
	for i, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.used == 0 || seg.columnarized() {
			continue
		}
		dst := c.columnarizeSegment(sh, seg, s, hot)
		if dst == nil {
			continue
		}
		sh.segs[i] = dst
		seg.retire()
		n++
	}
	return n
}

// readAll returns every ad the collection yields for expr, summarized order-insensitively.
func readAll(t *testing.T, c *Collection, expr string) []string {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for w := range c.QueryRawWire(q, nil, false) {
		out = append(out, adSummary(t, c, w))
	}
	return out
}

// TestColumnarizedShardServesEveryReadPath is the test that makes the format usable rather than
// merely buildable: it swaps columnarized segments into a LIVE shard and requires the ordinary read
// paths to return exactly what they returned before.
//
// This is the assertion the reader migration exists to satisfy. A columnarized record physically
// holds only the attributes the schema does not cover, so before that migration a plain scan
// returned a fraction of the rows (measured: 90 of 1500) and every ad it did return was missing
// half its attributes -- with nothing in the result to indicate it.
func TestColumnarizedShardServesEveryReadPath(t *testing.T) {
	c, s, hot := columnarFixture(t, 3000)
	defer c.Close()
	sh := c.shards[0]

	exprs := []string{
		"true",            // full scan
		"ProcId >= 5",     // one indexed-shape conjunct, columnar-servable
		"Owner == \"u3\"", // string equality
		"ProcId >= 5 && ProcId <= 8",
		"ProcId < 2 || ProcId > 7", // disjunction: never columnar-served
	}
	before := make([][]string, len(exprs))
	for i, e := range exprs {
		before[i] = readAll(t, c, e)
	}
	counts := make([]int, len(exprs))
	for i, e := range exprs {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := c.CountQuery(q)
		counts[i] = n
	}

	n := columnarizeShard(t, c, sh, s, hot)
	if n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	t.Logf("columnarized %d segments in a live shard", n)

	for i, e := range exprs {
		after := readAll(t, c, e)
		if len(after) != len(before[i]) {
			t.Errorf("%s: %d rows after columnarizing, want %d", e, len(after), len(before[i]))
			continue
		}
		want := map[string]int{}
		for _, k := range before[i] {
			want[k]++
		}
		bad := 0
		for _, k := range after {
			want[k]--
			if want[k] < 0 {
				bad++
			}
		}
		if bad != 0 {
			t.Errorf("%s: %d ads differ after columnarizing", e, bad)
		}
	}
	// The columnar count path reads the same columns the records no longer carry, so it must agree
	// with itself across the transform too.
	for i, e := range exprs {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := c.CountQuery(q); got != counts[i] {
			t.Errorf("%s: count %d after columnarizing, want %d", e, got, counts[i])
		}
	}
}

// TestColumnarizedShardServesReverseScan covers the newest-first walk separately: it collects each
// window's offsets in a forward pass and replays them backward, so it reaches records through a
// different path than the forward scan and could regress independently.
func TestColumnarizedShardServesReverseScan(t *testing.T) {
	c, s, hot := columnarFixture(t, 3000)
	defer c.Close()
	c.reverseScan = true
	before := readAll(t, c, "true")
	if n := columnarizeShard(t, c, c.shards[0], s, hot); n == 0 {
		t.Skip("no sealed segment was columnarized")
	}
	after := readAll(t, c, "true")
	if len(after) != len(before) {
		t.Fatalf("reverse scan: %d rows, want %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Fatalf("reverse scan diverges at %d: order or content changed", i)
		}
	}
}
