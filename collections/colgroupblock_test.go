package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// groupBlockFixture builds interned-wire records with a planted 3-attribute group, where:
//   - i%4 == 0        -> holds ALL three (a member)
//   - i%4 == 1        -> holds SOME (an exception: the partial state)
//   - otherwise       -> holds NONE
//
// Returns the wire records, the group's attribute ids, and the expected state per record.
func groupBlockFixture(t *testing.T, c *Collection, n int) ([][]byte, []uint32, []string) {
	t.Helper()
	var iws [][]byte
	var state []string
	for i := range n {
		text := fmt.Sprintf(`[ ClusterId=%d; Owner="u%d" ]`, i, i%5)
		st := "none"
		switch i % 4 {
		case 0:
			text = fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; GA=%d; GB=%d; GC=%d ]`, i, i%5, i, i*2, i*3)
			st = "in"
		case 1:
			text = fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; GA=%d ]`, i, i%5, i)
			st = "partial"
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		iws = append(iws, wire.Encode(nil, ad.AST(), c.intern))
		state = append(state, st)
	}
	ids := []uint32{c.intern.Intern("GA"), c.intern.Intern("GB"), c.intern.Intern("GC")}
	return iws, ids, state
}

func mkGroup(t *testing.T, c *Collection, iws [][]byte, ids []uint32) *colGroup {
	t.Helper()
	g := &colGroup{ids: ids, schema: buildAdSchemaFor(iws, ids)}
	if g.schema == nil || len(g.schema.fields) != len(ids) {
		t.Fatalf("group schema has %d fields, want %d", len(g.schema.fields), len(ids))
	}
	return g
}

// TestGroupBlockMembershipAndExceptions is the storage contract: a member's bit is set and its
// values are in the group block; a record holding NONE has a clear bit and is not an exception, so
// a clear-and-not-excepted bit proves every member attribute undefined; a record holding SOME is
// listed as an exception, because reading its clear bit as a proof of absence would be wrong.
func TestGroupBlockMembershipAndExceptions(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 200
	iws, ids, state := groupBlockFixture(t, c, n)
	g := mkGroup(t, c, iws, ids)

	gbs := buildGroupBlocks([]*colGroup{g}, iws, c.regionCodec())
	if len(gbs) != 1 {
		t.Fatalf("built %d group blocks, want 1", len(gbs))
	}
	gb := gbs[0]

	wantIn, wantPartial := 0, 0
	for k, st := range state {
		idx, ok := gb.index(k)
		switch st {
		case "in":
			wantIn++
			if !ok {
				t.Fatalf("record %d: not a member, want member", k)
			}
			if idx < 0 || idx >= gb.memberCount() {
				t.Fatalf("record %d: group index %d out of range (%d members)", k, idx, gb.memberCount())
			}
			if gb.isException(k) {
				t.Errorf("record %d: a member must not be an exception", k)
			}
		case "partial":
			wantPartial++
			if ok {
				t.Errorf("record %d: holds only part of the group but reads as a member", k)
			}
			if !gb.isException(k) {
				t.Errorf("record %d: holds part of the group but is not listed as an exception; "+
					"its clear bit would be read as proof the attributes are undefined", k)
			}
		default:
			if ok {
				t.Errorf("record %d: holds none of the group but reads as a member", k)
			}
			if gb.isException(k) {
				t.Errorf("record %d: holds none of the group but is listed as an exception", k)
			}
		}
	}
	if gb.memberCount() != wantIn {
		t.Errorf("member count = %d, want %d", gb.memberCount(), wantIn)
	}
	if len(gb.exceptions) != wantPartial {
		t.Errorf("exceptions = %d, want %d", len(gb.exceptions), wantPartial)
	}
}

// TestGroupBlockIndexIsRank: the base-index -> group-index mapping must be the rank of the
// membership bitmap, and it must be exact past the 64-record boundaries the prefix table covers --
// an off-by-one there would read a NEIGHBOUR's values, which is silent corruption rather than an
// error.
func TestGroupBlockIndexIsRank(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 500 // several 64-record ranks
	iws, ids, state := groupBlockFixture(t, c, n)
	g := mkGroup(t, c, iws, ids)
	gb := buildGroupBlocks([]*colGroup{g}, iws, c.regionCodec())[0]

	seen := 0
	for k := range n {
		idx, ok := gb.index(k)
		if state[k] != "in" {
			continue
		}
		if !ok {
			t.Fatalf("record %d: member reads as non-member", k)
		}
		if idx != seen {
			t.Fatalf("record %d: group index %d, want %d (members seen so far)", k, idx, seen)
		}
		seen++
	}
	if seen != gb.memberCount() {
		t.Errorf("walked %d members, block holds %d", seen, gb.memberCount())
	}
}

// TestGroupBlockValuesMatch reads the group block back through the ordinary columnar resolver and
// checks each member's values are its OWN. A group block is just a columnarBlock under the group's
// schema, which is what lets the existing readers work on it -- so this also pins that property.
func TestGroupBlockValuesMatch(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 300
	iws, ids, state := groupBlockFixture(t, c, n)
	g := mkGroup(t, c, iws, ids)
	gb := buildGroupBlocks([]*colGroup{g}, iws, c.regionCodec())[0]
	if gb.blk == nil {
		t.Fatal("no group block built")
	}

	bc, err := newBlockCache(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	cs := &colScope{bc: bc, c: c}
	cs.setBlock(gb.blk)
	for k := range n {
		if state[k] != "in" {
			continue
		}
		idx, ok := gb.index(k)
		if !ok {
			t.Fatalf("record %d not a member", k)
		}
		cs.k = idx
		cs.fellBack = false
		// GA was planted as the record's own base index.
		for _, tc := range []struct {
			attr string
			want int64
		}{{"GA", int64(k)}, {"GB", int64(k) * 2}, {"GC", int64(k) * 3}} {
			got := cs.resolve(tc.attr, ast.NoScope)
			iv, _ := got.IntValue()
			if !got.IsInteger() || iv != tc.want {
				t.Fatalf("record %d (group index %d): %s = %v, want %d", k, idx, tc.attr, got, tc.want)
			}
		}
	}
}

// TestGroupBlocksFollowBaseBlockBoundaries: when a segment seals several base blocks, each gets its
// own group block, and membership bitmaps are indexed by the BASE BLOCK's record numbering rather
// than the segment's -- otherwise every block after the first would read the wrong records.
func TestGroupBlocksFollowBaseBlockBoundaries(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 4000
	for i := range n {
		text := fmt.Sprintf(`[ ClusterId=%d; Owner="u%d" ]`, i, i%5)
		if i%4 == 0 {
			text = fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; GA=%d; GB=%d; GC=%d ]`, i, i%5, i, i*2, i*3)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Build directly over one sealed segment, with a small grouping so several blocks seal.
	sh := c.shards[0]
	_, wins := sh.snapshot()
	defer releaseWindows(wins)
	if len(wins) == 0 {
		t.Skip("no segments")
	}
	w := wins[0]
	samples := c.normalizeSamples(c.windowSamples(w, 8192))
	if len(samples) == 0 {
		t.Skip("no samples")
	}
	bs := buildAdSchema(samples, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	ids := []uint32{c.intern.Intern("GA"), c.intern.Intern("GB"), c.intern.Intern("GC")}
	g := &colGroup{ids: ids, schema: buildAdSchemaFor(samples, ids)}

	blocks, gblocks, _ := buildColumnarFromSegmentGrouped(w.data, w.used, w.codec, c.regionCodec(),
		bs, nil, []*colGroup{g}, byRows(256), func(dst, x []byte) ([]byte, bool) {
			return c.recordToInterned(dst, x)
		})
	if len(blocks) < 2 {
		t.Skipf("only %d base block(s); need several to test boundaries", len(blocks))
	}
	if len(gblocks) != len(blocks) {
		t.Fatalf("%d group-block sets for %d base blocks", len(gblocks), len(blocks))
	}
	for bi, blk := range blocks {
		gb := gblocks[bi][0]
		// The bitmap must be sized to THIS block, and every member index inside its own block.
		if got, want := len(gb.members), (blk.n+7)/8; got != want {
			t.Errorf("block %d: membership bitmap %d bytes for %d records, want %d", bi, got, blk.n, want)
		}
		members := 0
		for k := range blk.n {
			if idx, ok := gb.index(k); ok {
				if idx != members {
					t.Fatalf("block %d record %d: group index %d, want %d", bi, k, idx, members)
				}
				members++
			}
		}
		if members != gb.memberCount() {
			t.Errorf("block %d: walked %d members, block holds %d", bi, members, gb.memberCount())
		}
	}
}

// TestGroupBlocksRoundTripThroughPersistence: the group selections must survive marshal/unmarshal
// byte-exactly, including the membership bitmap and the exception list. A group column that reloads
// short or misaligned reads another record's values, which is a wrong answer rather than an error.
func TestGroupBlocksRoundTripThroughPersistence(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 400
	iws, ids, state := groupBlockFixture(t, c, n)
	g := mkGroup(t, c, iws, ids)
	base := buildAdSchema(iws, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})

	var rows [][]byte
	for _, iw := range iws {
		rows = append(rows, base.encode(wire.Ad(iw)))
	}
	blk := encodeColumnarBlock(base, rows, nil, identityCodec{})
	g.blocks = buildGroupBlocks([]*colGroup{g}, iws, identityCodec{})
	cs := &colSegment{blocks: []*columnarBlock{blk}, offs: make([]uint32, n), groups: []*colGroup{g}}

	blob := marshalColSegment(cs, func(id uint32) (string, bool) { return c.intern.Name(id) })
	if blob == nil {
		t.Fatal("marshal returned nil")
	}
	got := unmarshalColSegment(blob, identityCodec{}, func(name string) uint32 { return c.intern.Intern(name) })
	if got == nil {
		t.Fatal("unmarshal returned nil for a well-formed grouped segment")
	}
	if len(got.groups) != 1 {
		t.Fatalf("reloaded %d groups, want 1", len(got.groups))
	}
	rg := got.groups[0]
	if len(rg.ids) != len(g.ids) {
		t.Fatalf("reloaded %d member ids, want %d", len(rg.ids), len(g.ids))
	}
	if len(rg.blocks) != 1 {
		t.Fatalf("reloaded %d selections, want 1", len(rg.blocks))
	}
	rgb := rg.blocks[0]
	if rgb.memberCount() != g.blocks[0].memberCount() {
		t.Errorf("reloaded member count %d, want %d", rgb.memberCount(), g.blocks[0].memberCount())
	}
	if len(rgb.exceptions) != len(g.blocks[0].exceptions) {
		t.Errorf("reloaded %d exceptions, want %d", len(rgb.exceptions), len(g.blocks[0].exceptions))
	}
	// Values, through the ordinary resolver, at the reloaded rank.
	bc, err := newBlockCache(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	sc := &colScope{bc: bc, c: c}
	sc.setBlock(rgb.blk)
	for k := range n {
		idx, ok := rgb.index(k)
		if state[k] == "in" != ok {
			t.Fatalf("record %d: reloaded membership = %v, want %v", k, ok, state[k] == "in")
		}
		if state[k] == "partial" && !rgb.isException(k) {
			t.Fatalf("record %d: exception lost across reload", k)
		}
		if !ok {
			continue
		}
		sc.k = idx
		sc.fellBack = false
		v := sc.resolve("GB", ast.NoScope)
		iv, _ := v.IntValue()
		if !v.IsInteger() || iv != int64(k)*2 {
			t.Fatalf("record %d (group index %d): GB = %v, want %d", k, idx, v, k*2)
		}
	}
}

// TestGroupSectionRejectsInconsistentSelection: a membership bitmap whose population disagrees with
// its group block's record count would make rank address the wrong row. It must be refused, not
// read -- the columnar section is derived state, so rejecting means rebuilding.
func TestGroupSectionRejectsInconsistentSelection(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 128
	iws, ids, _ := groupBlockFixture(t, c, n)
	g := mkGroup(t, c, iws, ids)
	base := buildAdSchema(iws, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	var rows [][]byte
	for _, iw := range iws {
		rows = append(rows, base.encode(wire.Ad(iw)))
	}
	blk := encodeColumnarBlock(base, rows, nil, identityCodec{})
	g.blocks = buildGroupBlocks([]*colGroup{g}, iws, identityCodec{})
	cs := &colSegment{blocks: []*columnarBlock{blk}, offs: make([]uint32, n), groups: []*colGroup{g}}
	nameOf := func(id uint32) (string, bool) { return c.intern.Name(id) }
	internName := func(s string) uint32 { return c.intern.Intern(s) }

	if unmarshalColSegment(marshalColSegment(cs, nameOf), identityCodec{}, internName) == nil {
		t.Fatal("the well-formed blob must reload; the corruption test below would prove nothing")
	}
	// Set one more membership bit than the block has records.
	for i := range g.blocks[0].members {
		if g.blocks[0].members[i] != 0xFF {
			g.blocks[0].members[i] = 0xFF
			break
		}
	}
	if got := unmarshalColSegment(marshalColSegment(cs, nameOf), identityCodec{}, internName); got != nil {
		t.Error("a selection whose bitmap population exceeds its block's record count was accepted")
	}
}

// TestGroupBlockPopulationPastRankBoundary guards the trap that found this: rank's last entry
// counts members before the last 64-record boundary, which equals the total population only when
// the record count is a multiple of 64. A fixture sized to a multiple of 64 hides the difference,
// so this checks both sides of a boundary.
func TestGroupBlockPopulationPastRankBoundary(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	for _, n := range []int{64, 65, 128, 129, 191, 192, 400} {
		iws, ids, state := groupBlockFixture(t, c, n)
		g := mkGroup(t, c, iws, ids)
		gb := buildGroupBlocks([]*colGroup{g}, iws, c.regionCodec())[0]
		want := 0
		for _, st := range state {
			if st == "in" {
				want++
			}
		}
		if got := gb.population(); got != want {
			t.Errorf("n=%d: population = %d, want %d", n, got, want)
		}
		if got := gb.memberCount(); got != want {
			t.Errorf("n=%d: member count = %d, want %d", n, got, want)
		}
		// And the last member's rank must be the last index in the block.
		last := -1
		for k := range n {
			if _, ok := gb.index(k); ok {
				last = k
			}
		}
		if last >= 0 {
			if idx, _ := gb.index(last); idx != want-1 {
				t.Errorf("n=%d: last member at record %d has group index %d, want %d", n, last, idx, want-1)
			}
		}
	}
}
