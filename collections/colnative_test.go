package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// adSummary renders an ad as a sorted name=value list, so two encodings of the same ad compare
// equal regardless of attribute order -- which the splice deliberately does not preserve.
func adSummary(t *testing.T, c *Collection, w []byte) string {
	t.Helper()
	var parts []string
	wire.Ad(w).ForEachNamed(c.intern, func(name string, node []byte) bool {
		lit, ok := wire.LiteralValue(node)
		if !ok {
			parts = append(parts, strings.ToLower(name)+"=<expr>")
			return true
		}
		parts = append(parts, fmt.Sprintf("%s=%v", strings.ToLower(name), lit))
		return true
	})
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// columnarFixture fills a persistent collection, seals its segments, and returns it with the
// derived schema.
func columnarFixture(t *testing.T, n int) (*Collection, *adSchema, []int) {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		// A mix of shapes: schema'd numerics and strings, an attribute only some ads carry (so
		// the schema escapes it), and one that no schema will cover.
		text := fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep" ]`,
			i, i%10, i%7, i%6, (i%16)*1024)
		switch i % 5 {
		case 0:
			text = fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep"; Rare%d=%d ]`,
				i, i%10, i%7, i%6, (i%16)*1024, i%3, i)
		case 1:
			text = fmt.Sprintf(
				`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; Cmd="/bin/sleep"; Wide=%d ]`,
				i, i%10, i%7, i%6, (i%16)*1024, i*1000000)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema")
	}
	return c, st.schema, st.hot
}

// TestColumnarizeRoundTrip is the contract this whole format rests on: after a segment's schema'd
// attributes are moved into its columnar record and removed from its rows, every ad must read back
// exactly as before. The values are no longer anywhere else, so a bug here is data loss rather than
// a slow path.
func TestColumnarizeRoundTrip(t *testing.T) {
	c, s, hot := columnarFixture(t, 2000)
	defer c.Close()

	sh := c.shards[0]
	sh.mu.Lock()
	act := sh.act
	var src *segment
	for _, seg := range sh.segs {
		if seg != nil && seg != act && seg.used > 0 {
			src = seg
			break
		}
	}
	sh.mu.Unlock()
	if src == nil {
		t.Skip("no sealed segment")
	}

	// What the segment says before the rewrite.
	before := map[string]string{}
	var buf []byte
	for off := 0; off < src.used; {
		o := uint32(off)
		rl := recTotalLen(src.data, o)
		if rl == 0 {
			break
		}
		off += int(rl)
		if recIsMarker(src.data, o) {
			continue
		}
		raw, err := src.codec.Decompress(buf[:0], recAd(src.data, o))
		if err != nil {
			t.Fatal(err)
		}
		buf = raw
		before[string(recKey(src.data, o))] = adSummary(t, c, raw)
	}
	if len(before) == 0 {
		t.Skip("no records in the sealed segment")
	}

	sh.mu.Lock()
	dst := c.columnarizeSegment(sh, src, s, hot)
	sh.mu.Unlock()
	if dst == nil {
		t.Fatal("columnarizeSegment returned nil")
	}
	defer func() {
		dst.retire()
		dst.reapAndHook()
	}()
	if !dst.columnarized() {
		t.Fatal("the rewritten segment is not columnarized")
	}

	after := map[string]string{}
	for off := 0; off < dst.used; {
		o := uint32(off)
		rl := recTotalLen(dst.data, o)
		if rl == 0 {
			break
		}
		off += int(rl)
		if recIsMarker(dst.data, o) {
			continue
		}
		full, err := c.recordWire(dst, o, nil)
		if err != nil {
			t.Fatalf("record %d: %v", o, err)
		}
		after[string(recKey(dst.data, o))] = adSummary(t, c, full)
	}
	if len(after) != len(before) {
		t.Fatalf("records after %d, before %d", len(after), len(before))
	}
	for k, want := range before {
		got, ok := after[k]
		if !ok {
			t.Fatalf("key %q lost", k)
		}
		if got != want {
			t.Fatalf("key %q\n  before %s\n   after %s", k, want, got)
		}
	}
}

// TestColumnarizeRemovesTheRowCopy: the point of the exercise. A schema'd attribute must be gone
// from the record's own bytes -- if it is still there, the segment is carrying two copies and the
// format has bought nothing.
func TestColumnarizeRemovesTheRowCopy(t *testing.T) {
	c, s, hot := columnarFixture(t, 2000)
	defer c.Close()
	sh := c.shards[0]
	sh.mu.Lock()
	act := sh.act
	var src *segment
	for _, seg := range sh.segs {
		if seg != nil && seg != act && seg.used > 0 {
			src = seg
			break
		}
	}
	dst := c.columnarizeSegment(sh, src, s, hot)
	sh.mu.Unlock()
	if src == nil || dst == nil {
		t.Skip("nothing to columnarize")
	}
	defer func() {
		dst.retire()
		dst.reapAndHook()
	}()

	// ClusterId is in the schema, so no remnant may still carry it.
	id, ok := c.intern.LookupID("clusterid")
	if !ok {
		t.Skip("ClusterId not interned")
	}
	if _, covered := s.byID[id]; !covered {
		t.Skip("ClusterId is not a schema field in this fixture")
	}
	var buf []byte
	remnantsWith, records := 0, 0
	for off := 0; off < dst.used; {
		o := uint32(off)
		rl := recTotalLen(dst.data, o)
		if rl == 0 {
			break
		}
		off += int(rl)
		if recIsMarker(dst.data, o) {
			continue
		}
		records++
		raw, err := dst.codec.Decompress(buf[:0], recAd(dst.data, o))
		if err != nil {
			t.Fatal(err)
		}
		buf = raw
		wire.Ad(raw).ForEachNamed(c.intern, func(name string, _ []byte) bool {
			if strings.EqualFold(name, "ClusterId") {
				remnantsWith++
				return false
			}
			return true
		})
	}
	if records == 0 {
		t.Skip("no records")
	}
	if remnantsWith != 0 {
		t.Errorf("%d of %d remnants still carry ClusterId; the row copy was not removed",
			remnantsWith, records)
	}
	// And it must still be readable, from the column.
	full, err := c.recordWire(dst, firstDataRecord(dst), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	wire.Ad(full).ForEachNamed(c.intern, func(name string, _ []byte) bool {
		if strings.EqualFold(name, "ClusterId") {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Error("ClusterId is neither in the remnant nor readable from the column")
	}
}

func firstDataRecord(s *segment) uint32 {
	for off := 0; off < s.used; {
		o := uint32(off)
		rl := recTotalLen(s.data, o)
		if rl == 0 {
			break
		}
		if !recIsMarker(s.data, o) {
			return o
		}
		off += int(rl)
	}
	return 0
}

// TestColumnarizeShrinksTheSegment measures the point of the format.
func TestColumnarizeShrinksTheSegment(t *testing.T) {
	c, s, hot := columnarFixture(t, 4000)
	defer c.Close()
	sh := c.shards[0]
	sh.mu.Lock()
	act := sh.act
	var src *segment
	for _, seg := range sh.segs {
		if seg != nil && seg != act && seg.used > 0 {
			src = seg
			break
		}
	}
	var beforeUsed int
	if src != nil {
		beforeUsed = src.used
	}
	dst := c.columnarizeSegment(sh, src, s, hot)
	sh.mu.Unlock()
	if src == nil || dst == nil {
		t.Skip("nothing to columnarize")
	}
	defer func() {
		dst.retire()
		dst.reapAndHook()
	}()
	t.Logf("segment bytes: %d whole-record -> %d columnarized (%.2fx)",
		beforeUsed, dst.used, float64(dst.used)/float64(beforeUsed))
	if dst.used >= beforeUsed {
		t.Errorf("columnarized segment is not smaller: %d -> %d", beforeUsed, dst.used)
	}
}
