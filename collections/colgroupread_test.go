package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// groupReadFixture attaches a group to one block of records and returns everything a read test
// needs: the scope with the group bound, the base block, and the expected per-record state.
func groupReadFixture(t *testing.T, c *Collection, n int) (*colScope, *colSegment, []string) {
	t.Helper()
	iws, ids, state := groupBlockFixture(t, c, n)
	base := buildAdSchema(iws, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	var rows [][]byte
	for _, iw := range iws {
		rows = append(rows, base.encode(wire.Ad(iw)))
	}
	blk := encodeColumnarBlock(base, rows, nil, c.regionCodec())
	g := &colGroup{ids: nil, schema: buildAdSchemaFor(iws, ids)}
	for _, f := range g.schema.fields {
		g.ids = append(g.ids, f.id)
	}
	g.blocks = buildGroupBlocks([]*colGroup{g}, iws, c.regionCodec())
	seg := &colSegment{blocks: []*columnarBlock{blk}, offs: make([]uint32, n), groups: []*colGroup{g}}

	bc, err := newBlockCache(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	cs := &colScope{bc: bc, c: c}
	cs.setBlock(blk)
	cs.setGroups(seg, 0)
	return cs, seg, state
}

// TestGroupResolveThreeStates is the read contract. A member's value comes from the group block; a
// record holding none of the group reads UNDEFINED with no fallback (the case the whole design
// exists for); an exception reads its value from the BASE block's cold tail, since a group
// attribute is not a base field and the base encoder left it there.
func TestGroupResolveThreeStates(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 240
	cs, _, state := groupReadFixture(t, c, n)

	for k := range n {
		cs.k = k
		cs.fellBack = false
		v := cs.resolve("GA", ast.NoScope)
		switch state[k] {
		case "in":
			iv, _ := v.IntValue()
			if !v.IsInteger() || iv != int64(k) {
				t.Fatalf("record %d (member): GA = %v, want %d", k, v, k)
			}
			if cs.fellBack {
				t.Errorf("record %d: a member must not fall back", k)
			}
		case "partial":
			// GA is the attribute the partial records DO carry.
			iv, _ := v.IntValue()
			if !v.IsInteger() || iv != int64(k) {
				t.Fatalf("record %d (exception): GA = %v, want %d from the cold tail", k, v, k)
			}
		default:
			if !v.IsUndefined() {
				t.Fatalf("record %d (holds none of the group): GA = %v, want undefined", k, v)
			}
			if cs.fellBack {
				t.Errorf("record %d: proving the group absent must not require a fallback", k)
			}
		}
	}
	// GB is NOT carried by the partial records, so it must read undefined for them too.
	for k := range n {
		if state[k] != "partial" {
			continue
		}
		cs.k = k
		cs.fellBack = false
		if v := cs.resolve("GB", ast.NoScope); !v.IsUndefined() {
			t.Fatalf("record %d: GB = %v, want undefined (the exception does not carry it)", k, v)
		}
	}
}

// TestGroupVecColumnMatchesScalar: the vectorized load must agree with the per-record resolver
// element for element. A disagreement here is a wrong query answer that only appears on the
// vectorized path, which is the hardest kind to notice.
func TestGroupVecColumnMatchesScalar(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 300
	cs, seg, state := groupReadFixture(t, c, n)
	blk := seg.blocks[0]

	src := &blockVecSource{c: c, bc: cs.bc, dicts: map[int]*blockDict{}}
	src.blk = blk
	src.setGroups(seg, 0)
	dst := &vm.Vec{I: make([]int64, n), St: make([]uint8, n), S: make([]string, n)}
	loaded, handled := src.loadGroupColumn("GA", dst)
	if !handled {
		t.Fatal("loadGroupColumn did not handle a group attribute")
	}
	if !loaded {
		t.Fatal("loadGroupColumn declined a column it should serve")
	}
	for k := range n {
		cs.k = k
		cs.fellBack = false
		want := cs.resolve("GA", ast.NoScope)
		switch {
		case want.IsUndefined():
			if dst.St[k] != vm.VsUndef {
				t.Fatalf("record %d (%s): vector state %d, scalar says undefined", k, state[k], dst.St[k])
			}
		case want.IsInteger():
			iv, _ := want.IntValue()
			if dst.St[k] != vm.VsInt || dst.I[k] != iv {
				t.Fatalf("record %d (%s): vector (%d,%d), scalar says int %d", k, state[k], dst.St[k], dst.I[k], iv)
			}
		default:
			t.Fatalf("record %d: unexpected scalar kind %v", k, want)
		}
	}
}

// TestGroupQueryMatchesRowPath: a query over a grouped attribute must return exactly what the row
// scan returns, from the columnar path that actually reads the group column.
//
// CountQuery reaches it by its LAST tier: the attribute is not in the base schema, so the
// hand-written numeric and presence scans decline, and VectorEvalCount evaluates the query against
// the columns -- where LoadColumn finds the attribute in a group schema and presents it at full
// block length. Verified by instrumenting loadGroupColumn: 15 calls per query here, one per sealed
// block, and 30 for a two-column expression.
func TestGroupQueryMatchesRowPath(t *testing.T) {
	dir := t.TempDir()
	// Small segments so several SEAL: an unsealed segment carries no columnar block at all, and the
	// comparison below would then be the row path against itself.
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 3000
	want := map[string]int{}
	for i := range n {
		text := fmt.Sprintf(`[ ClusterId=%d; Owner="u%d" ]`, i, i%5)
		switch i % 4 {
		case 0:
			text = fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; GA=%d; GB=%d; GC=%d ]`, i, i%5, i, i*2, i*3)
		case 1:
			text = fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; GA=%d ]`, i, i%5, i)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{
		`GA > 1000`, `GA is undefined`, `GA isnt undefined`,
		`GB > 100 && ClusterId < 2000`, // a group column and a base column together
		`GA + GB > 10`,                 // two group columns in one expression
		`GA > GC`,                      // group column against group column
	}
	for _, q := range queries {
		want[q] = 0
		qq, err := vm.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		for range c.Query(qq) {
			want[q]++
		}
	}
	// Establish that the groups keep co-occurring: blocks are not built for a group seen once
	// (see stableGroupKeys). Three derivations is the default gate.
	for range 3 {
		c.GroupSchemas(4096, 4)
	}
	// Enable the accelerator and re-ask. The answers must not move.
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	// The comparison is only worth anything if groups were actually built: without them both
	// sides take the same path and the test proves nothing.
	st := c.schemaScan.Load()
	if st == nil || len(st.groups) == 0 {
		t.Fatal("no group schemas derived; this test would compare a path against itself")
	}
	var carriesGA bool
	gaID := c.intern.Intern("GA")
	for _, g := range st.groups {
		if _, ok := g.schema.byID[gaID]; ok {
			carriesGA = true
		}
	}
	if !carriesGA {
		t.Fatalf("no group carries GA; the queries below would not exercise a group column")
	}
	for _, q := range queries {
		exp := want[q]
		qq, err := vm.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		// The row scan must still agree...
		got := 0
		for range c.Query(qq) {
			got++
		}
		if got != exp {
			t.Errorf("%q: %d rows with the accelerator, %d without", q, got, exp)
		}
		// ...and so must the COLUMNAR count, which is the path that reads group columns. A
		// query answered by the row scan on both sides would compare a path against itself.
		cnt, ok := c.CountQuery(qq)
		if !ok {
			t.Errorf("%q: columnar count declined; the group column is not being exercised", q)
			continue
		}
		if cnt != exp {
			t.Errorf("%q: columnar count %d, row scan %d", q, cnt, exp)
		}
	}
}
