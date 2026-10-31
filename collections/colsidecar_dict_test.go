package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestSidecarBlockOnInternedSegmentReadsItsColdTail: a sidecar columnar block over a segment with a
// dictionary keys its cold tails by that dictionary's ids, and the block read back from the sidecar
// carried no translation into them. Every cold-tail lookup then went by this process's intern id,
// which names another attribute of the dictionary or none: a type exception on a schema field read as
// a missing value, and an attribute the schema does not carry read as absent from every record. The
// counts came from the columns with no fallback, so nothing but the answer showed it.
//
// Columnarizing is off, so the sealed segments are read through sidecar blocks only -- the state of
// every segment that maintenance has not yet columnarized. The intern ids and the dictionary's differ
// from the start (the dictionary numbers names as the segment met them), and a reopen moves the intern
// ids again, so the check runs on both sides of one.
func TestSidecarBlockOnInternedSegmentReadsItsColdTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: -1, ColumnarSegmentBudget: -1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	const rare = 6
	for i := range 2000 {
		cid := fmt.Sprintf("%d", i)
		if i%50 == 1 {
			cid = `"text"` // a type exception: escapes ClusterId's int slot into the cold tail
		}
		extra := ""
		if i%7 == 0 {
			extra = fmt.Sprintf("; X%d=%d", (i/100)%rare, i) // below the presence floor: never a field
		}
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d%s ]`, cid, i%10, i%7, i%6, extra))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(4096); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	exprs := []string{`ClusterId == "text"`, `ClusterId is undefined`, `ClusterId > 100`}
	for x := range rare {
		exprs = append(exprs, fmt.Sprintf("X%d > 0", x), fmt.Sprintf("X%d is undefined", x))
	}
	check := func(stage string) {
		t.Helper()
		blocks := 0
		for _, seg := range c.shards[0].segs {
			if seg == nil {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil && seg.dict.Load() != nil && !seg.columnarized() {
				blocks++
			}
		}
		if blocks == 0 {
			t.Fatalf("%s: no sidecar block over an interned segment; the case goes untested", stage)
		}
		served := 0
		for _, e := range exprs {
			q, err := vm.Parse(e)
			if err != nil {
				t.Fatal(err)
			}
			n, ok := c.CountQuery(q)
			if !ok {
				continue
			}
			served++
			if want := len(readAll(t, c, e)); n != want {
				t.Errorf("%s, counting %s: %d from the columns, %d from the records", stage, e, n, want)
			}
		}
		if served < len(exprs)/2 {
			t.Fatalf("%s: the columns served %d of %d counts", stage, served, len(exprs))
		}
	}
	check("before the reopen")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()
	check("after the reopen")
}

// TestBuiltSidecarBlockTranslatesNonSchemaAttributes: a block built over an interned segment, before it
// is written to the sidecar and read back, must translate every attribute its cold tails hold, not only
// the schema's fields -- a reader looks up the others there too, and one the translation does not know
// reads as absent from the payload.
func TestBuiltSidecarBlockTranslatesNonSchemaAttributes(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: -1, ColumnarSegmentBudget: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 600 {
		extra := ""
		if i%7 == 0 {
			extra = fmt.Sprintf("; Rare=%d", i)
		}
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"%s ]`, i, i%10, i%7, extra))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(4096); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	st := c.schemaScan.Load()
	rare, ok := c.intern.LookupID("Rare")
	if !ok {
		t.Fatal("Rare not interned")
	}
	if _, isField := st.schema.byID[rare]; isField {
		t.Fatal("Rare became a schema field; the case goes untested")
	}
	built := 0
	for _, seg := range c.shards[0].segs {
		if seg == nil || seg.dict.Load() == nil {
			continue
		}
		cs := c.buildColSegment(seg, st.schema, st.hot)
		if cs == nil {
			continue
		}
		built++
		lid, inDict := seg.dict.Load().lookup("Rare")
		for _, b := range cs.blocks {
			got, known := b.remap.storedID(rare)
			if known != inDict || (inDict && got != lid) {
				t.Fatalf("Rare (intern id %d, dictionary id %d/%v) translates to %d/%v", rare, lid, inDict, got, known)
			}
		}
	}
	if built == 0 {
		t.Fatal("no block built over an interned segment")
	}
}

// TestSidecarGroupBlockOnInternedSegmentReadsItsColdTail is the group-block half: a group column's own
// cold tail is keyed by the segment dictionary too, and a value that escapes it (a string where the
// group's slot holds an integer) read as absent through a sidecar block, before a reopen and after one.
func TestSidecarGroupBlockOnInternedSegmentReadsItsColdTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: 4, GroupStabilityRuns: 1, ColumnarSegmentBudget: -1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	adText := func(i int) string {
		if i%3 != 0 {
			return fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d ]`, i, i%10, i%7, i%6)
		}
		exit := "1"
		if i%150 == 0 {
			exit = `"aborted"` // escapes the group's int slot into the group block's cold tail
		}
		return fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; AaStart=%d; AaEnd=%d; AaHost="h%d"; AaExit=%s ]`,
			i, i%10, i%7, i%6, i, i+10, i%50, exit)
	}
	const n = 3000
	c := open()
	for i := range n {
		ad, err := classad.Parse(adText(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(0); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	if st := c.schemaScan.Load(); st == nil || len(st.groups) == 0 {
		t.Fatal("no group schema was derived; the case goes untested")
	}
	check := func(stage string) {
		t.Helper()
		grouped := 0
		for _, seg := range c.shards[0].segs {
			if seg == nil || seg.dict.Load() == nil || seg.columnarized() {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil && len(cs.groups) > 0 {
				grouped++
			}
		}
		if grouped == 0 {
			t.Fatalf("%s: no sidecar block with groups over an interned segment", stage)
		}
		for _, e := range []string{`AaExit == "aborted"`, `AaExit == 1`, `AaExit is undefined`} {
			q, err := vm.Parse(e)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			for i := range n {
				ad, _ := classad.Parse(adText(i))
				if q.Matches(ad) {
					want++
				}
			}
			if got, ok := c.CountQuery(q); ok && got != want {
				t.Errorf("%s: CountQuery(%s) = %d, want %d", stage, e, got, want)
			}
			if got, ok := c.ColumnarEvalCount(q); ok && got != want {
				t.Errorf("%s: ColumnarEvalCount(%s) = %d, want %d", stage, e, got, want)
			}
		}
	}
	check("before the reopen")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	defer c.Close()
	check("after the reopen")
}
