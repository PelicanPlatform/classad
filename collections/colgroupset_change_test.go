package collections

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Segments columnarized under different committed group sets, read by one scan.
//
// A table's committed group set changes while it runs (promotion on a maintenance pass, a re-schema),
// and the segments built before the change keep the old set until convergence rewrites them, which is
// bounded per pass. A single COUNT therefore walks segments whose groups differ in number, order and
// width. Any reader state that caches a position resolved against one segment's groups and applies it
// to another's reads past the end of a group block's schema -- the shape that took a production daemon
// down on `EnteredHistoryTime >= t && MemoryUsage > 4095` over a history archive, with MemoryUsage an
// expression in most records and a literal in the rest.

const (
	gscNPhase   = 1500 // records per group-set phase
	gscWideBool = 12   // bool members of the first group, ahead of MemoryUsage in its layout
	gscNarrow   = 10   // int members of the second group
)

// gscAd is record i of the fixture. Every record carries the narrow group's attributes (ten ints)
// and MemoryUsage; phase 1 records also carry the wide group's twelve bools. MemoryUsage is the condor
// expression in nine records of ten and a literal in the tenth, so the literal ones are members of
// the wide group and the expression ones are not.
func gscAd(i int) string {
	rss := int64(1+i%9) * 1_000_000 // KiB: MemoryUsage straddles the 4095 MiB threshold
	mem := "MemoryUsage = ((ResidentSetSize + 1023) / 1024)"
	if i%10 == 0 {
		mem = fmt.Sprintf("MemoryUsage = %d", (rss+1023)/1024+int64(i%3)*100)
	}
	s := fmt.Sprintf(`[ ClusterId = %d; ProcId = 0; EnteredHistoryTime = %d; Owner = "u%d"; ResidentSetSize = %d; %s`,
		i, 1_700_000_000+i, i%5, rss, mem)
	if i < gscNPhase {
		for b := 0; b < gscWideBool; b++ {
			s += fmt.Sprintf("; WideFlag%02d = %v", b, (i+b)%2 == 0)
		}
	}
	for n := 0; n < gscNarrow; n++ {
		s += fmt.Sprintf("; Narrow%02d = %d", n, i*10+n)
	}
	return s + " ]"
}

// gscWant is the plain-evaluation answer for constraint over the first n fixture records.
func gscWant(t *testing.T, n int, constraint string) int {
	t.Helper()
	q, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(gscAd(i))
		if err != nil {
			t.Fatal(err)
		}
		if v := ad.EvaluateExprWithTarget(q, nil); v.IsBool() {
			if b, _ := v.BoolValue(); b {
				want++
			}
		}
	}
	return want
}

// gscSchema builds a schema over the named attributes from c's current sample. The sample is taken
// first because normalizing it is what interns a persistent store's attribute names.
func gscSchema(t *testing.T, c *Collection, names []string) ([]uint32, *adSchema) {
	t.Helper()
	samples := c.normalizeSamples(c.CollectSamplesRecentN(4096))
	ids := make([]uint32, 0, len(names))
	for _, n := range names {
		id, ok := c.intern.LookupID(n)
		if !ok {
			t.Fatalf("%s was never interned", n)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	s := buildAdSchemaFor(samples, ids)
	if len(s.fields) != len(names) {
		t.Fatalf("schema over %v built %d fields, want %d", names, len(s.fields), len(names))
	}
	return ids, s
}

// gscGroup builds a group schema over the named attributes.
func gscGroup(t *testing.T, c *Collection, names []string) *colGroup {
	t.Helper()
	ids, s := gscSchema(t, c, names)
	return &colGroup{schema: s, ids: ids}
}

// gscBuild writes the fixture through put, columnarizing the first phase's sealed segments under the
// wide group and the second phase's under the narrow one. It returns the two groups' MemoryUsage
// layout, checked here, is what the tests rely on.
func gscBuild(t *testing.T, c *Collection, put func(i int)) {
	t.Helper()
	c.colBudget = 1 << 20
	for i := 0; i < gscNPhase; i++ {
		put(i)
	}
	_, sch := gscSchema(t, c, []string{"ClusterId", "ProcId", "EnteredHistoryTime", "ResidentSetSize", "Owner"})
	if !c.installSchemaScan(sch, nil) {
		t.Fatal("schema scan did not install")
	}
	st := c.schemaScan.Load()

	wideNames := []string{"MemoryUsage"}
	for b := 0; b < gscWideBool; b++ {
		wideNames = append(wideNames, fmt.Sprintf("WideFlag%02d", b))
	}
	wide := gscGroup(t, c, wideNames)
	memID, _ := c.intern.LookupID("MemoryUsage")
	if fi := wide.schema.byID[memID]; fi < gscNarrow {
		t.Fatalf("MemoryUsage is field %d of the wide group; the fixture needs it at or past %d", fi, gscNarrow)
	}
	c.schemaScan.Store(&schemaScanState{schema: st.schema, hot: st.hot, cache: st.cache, groups: []*colGroup{wide}})
	if n := c.ColumnarizeSealed(); n == 0 {
		t.Fatal("no phase-1 segment was columnarized")
	}

	for i := gscNPhase; i < 2*gscNPhase; i++ {
		put(i)
	}
	narrowNames := make([]string, 0, gscNarrow)
	for n := 0; n < gscNarrow; n++ {
		narrowNames = append(narrowNames, fmt.Sprintf("Narrow%02d", n))
	}
	narrow := gscGroup(t, c, narrowNames)
	c.schemaScan.Store(&schemaScanState{schema: st.schema, hot: st.hot, cache: st.cache, groups: []*colGroup{narrow}})
	if n := c.ColumnarizeSealed(); n == 0 {
		t.Fatal("no phase-2 segment was columnarized")
	}

	// The scan must meet both group sets, wide first, or it proves nothing.
	var sets []int
	sh := c.shards[0]
	sh.mu.RLock()
	for _, seg := range sh.segs {
		if seg == nil {
			continue
		}
		if cs := seg.colblk.Load(); cs != nil && len(cs.groups) == 1 {
			sets = append(sets, len(cs.groups[0].schema.fields))
		}
	}
	sh.mu.RUnlock()
	sawWide, sawNarrowAfter := false, false
	for _, n := range sets {
		if n == len(wideNames) {
			sawWide = true
		} else if n == gscNarrow && sawWide {
			sawNarrowAfter = true
		}
	}
	if !sawWide || !sawNarrowAfter {
		t.Fatalf("segments' group widths in scan order %v: need a wide-group segment before a narrow-group one", sets)
	}
}

var gscConstraints = []string{
	"EnteredHistoryTime >= 1700000000 && MemoryUsage > 4095",
	"MemoryUsage > 4095",
	"MemoryUsage is undefined",
	"WideFlag03 == true && MemoryUsage > 2000",
	"Narrow04 > 0 || MemoryUsage > 8000",
}

// TestColumnarCountAcrossGroupSetChangeCollection is the mutable-table form: a COUNT over segments
// columnarized under two group sets must answer as plain evaluation does, and must not panic.
func TestColumnarCountAcrossGroupSetChangeCollection(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	gscBuild(t, c, func(i int) {
		ad, err := classad.Parse(gscAd(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	})
	for _, q := range gscConstraints {
		got, ok := c.CountConstraint(q)
		if !ok {
			t.Fatalf("%s: columnar count declined", q)
		}
		if want := gscWant(t, 2*gscNPhase, q); got != want {
			t.Errorf("%s: columnar count %d, plain evaluation %d", q, got, want)
		}
	}
}

// TestColumnarCountAcrossGroupSetChangeArchive is the archive form, which is how the query reached
// it in production (an aggregate COUNT over a history table).
func TestColumnarCountAcrossGroupSetChangeArchive(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	gscBuild(t, a.c, func(i int) {
		ad, err := classad.Parse(gscAd(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	})
	for _, q := range gscConstraints {
		got, ok := a.CountConstraint(q)
		if !ok {
			t.Fatalf("%s: columnar count declined", q)
		}
		if want := gscWant(t, 2*gscNPhase, q); got != want {
			t.Errorf("%s: columnar count %d, plain evaluation %d", q, got, want)
		}
	}
}

// TestGroupBindingFollowsTheList: the cached (group, field) bindings are checked against the group
// list on use, so a binding cannot be applied to a list it was not resolved in even by a caller that
// changes the list without going through setGroups.
func TestGroupBindingFollowsTheList(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	x := c.intern.Intern("X")
	h := c.intern.Intern("H")
	g := func(ids ...uint32) *colGroup {
		var fs []adField
		for _, id := range ids {
			fs = append(fs, adField{id: id, kind: akInt, width: 8})
		}
		return &colGroup{schema: layoutSchema(fs), ids: ids}
	}
	segA := &colSegment{groups: []*colGroup{g(h)}}
	segB := &colSegment{groups: []*colGroup{g(h), g(x)}}
	cs := &colScope{c: c}
	cs.setGroups(segA, 0)
	if _, ok := cs.bindGroupField("X"); ok {
		t.Fatal("X bound to a group in a list where no group carries it")
	}
	cs.groups = segB.groups // no setGroups: nothing resets the cache
	if b, ok := cs.bindGroupField("X"); !ok || b.gi != 1 {
		t.Fatalf("X after the list changed bound to (%v, %+v), want group 1", ok, b)
	}
	cs.groups = segA.groups
	if _, ok := cs.bindGroupField("X"); ok {
		t.Fatal("X kept the other list's binding")
	}
}

// TestColScopeFieldPastSchemaFallsBack: a field index the current block's schema does not have is a
// fallback (the record answers), not an out-of-range read.
func TestColScopeFieldPastSchemaFallsBack(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	cs, _, _ := groupSchemaReadFixture(t, c, 64)
	n := len(cs.blk.schema.fields)
	for _, idx := range []int{n, n + 7, -1} {
		cs.k, cs.fellBack = 0, false
		if v := cs.slotValue(idx); !v.IsUndefined() || !cs.fellBack {
			t.Errorf("slotValue(%d) of a %d-field schema = %v (fellBack %v), want a fallback", idx, n, v, cs.fellBack)
		}
		cs.k, cs.fellBack = 0, false
		if v := cs.resolveField(idx); !v.IsUndefined() || !cs.fellBack {
			t.Errorf("resolveField(%d) of a %d-field schema = %v (fellBack %v), want a fallback", idx, n, v, cs.fellBack)
		}
	}
}

// TestResolveGroupUnreadableBindingFallsBack: when a group binding cannot be read against the
// current block -- a position past the group's schema, a group the list does not have, no selection
// for the block, or a selection built under another schema -- a member record falls back to the
// record. None of them may read out of range or return another field's value.
func TestResolveGroupUnreadableBindingFallsBack(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 240
	member := -1
	{
		_, _, state := groupSchemaReadFixture(t, c, n)
		for k, s := range state {
			if s == "in" {
				member = k
				break
			}
		}
	}
	if member < 0 {
		t.Fatal("fixture has no member record")
	}
	cases := map[string]func(cs *colScope){
		"field past the group schema": func(cs *colScope) {
			cs.bindG, cs.bindGFor = map[string]groupBind{"GA": {gi: 0, fi: 99}}, cs.groups
		},
		"group past the list": func(cs *colScope) {
			cs.bindG, cs.bindGFor = map[string]groupBind{"GA": {gi: 3, fi: 0}}, cs.groups
		},
		"no selection for the block": func(cs *colScope) {
			cs.gblocks[0] = nil
		},
		"selection under another schema": func(cs *colScope) {
			g := *cs.groups[0]
			sch := *g.schema
			g.schema = &sch // equal content, different schema: positions are not provably the same
			cs.groups = []*colGroup{&g}
		},
	}
	for name, poison := range cases {
		t.Run(name, func(t *testing.T) {
			cs, _, _ := groupSchemaReadFixture(t, c, n)
			cs.k, cs.fellBack = member, false
			if v := cs.resolve("GA", ast.NoScope); !v.IsInteger() || cs.fellBack {
				t.Fatalf("unpoisoned member read GA = %v (fellBack %v), want its value", v, cs.fellBack)
			}
			poison(cs)
			cs.k, cs.fellBack = member, false
			if v := cs.resolve("GA", ast.NoScope); !v.IsUndefined() || !cs.fellBack {
				t.Fatalf("GA = %v (fellBack %v), want a fallback", v, cs.fellBack)
			}
			if _, handled := cs.groupAbsentAt("GA", member); handled {
				t.Fatal("groupAbsentAt answered from a selection it cannot read")
			}
		})
	}
}

// TestLoadGroupColumnDeclinesWithoutSelection: a block with no selection for a group cannot present
// the group's column -- every element would be a guess -- so the block is declined, and binding a
// block index outside the segment does not index out of range.
func TestLoadGroupColumnDeclinesWithoutSelection(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 128
	cs, seg, _ := groupSchemaReadFixture(t, c, n)
	src := &blockVecSource{c: c, bc: cs.bc, dicts: map[int]*blockDict{}}
	src.blk = seg.blocks[0]
	for _, bi := range []int{1, -1} {
		src.setGroups(seg, bi)
		cs.setGroups(seg, bi)
		dst := &vm.Vec{I: make([]int64, n), St: make([]uint8, n), S: make([]string, n)}
		loaded, handled := src.loadGroupColumn("GA", dst)
		if !handled || loaded {
			t.Errorf("block index %d: loadGroupColumn = (loaded %v, handled %v), want declined", bi, loaded, handled)
		}
		cs.k, cs.fellBack = 0, false
		if v := cs.resolve("GA", ast.NoScope); !v.IsUndefined() || !cs.fellBack {
			t.Errorf("block index %d: GA = %v (fellBack %v), want a fallback", bi, v, cs.fellBack)
		}
	}
}

// TestResolveGroupRankPastBlockFallsBack: a membership bitmap claiming more members than its group
// block holds maps a record to a rank past the block's end. That record falls back rather than read
// past the block.
func TestResolveGroupRankPastBlockFallsBack(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 240
	cs, _, _ := groupSchemaReadFixture(t, c, n)
	gb := *cs.gblocks[0]
	gb.members = make([]byte, len(gb.members))
	for i := range gb.members {
		gb.members[i] = 0xff
	}
	gb.exceptions = nil
	gb.buildRank(n)
	cs.gblocks[0] = &gb
	k := n - 1
	if idx, ok := gb.index(k); !ok || idx < gb.blk.n {
		t.Fatalf("fixture: record %d maps to (%d, %v), want a rank past the %d-record block", k, idx, ok, gb.blk.n)
	}
	cs.k, cs.fellBack = k, false
	if v := cs.resolve("GA", ast.NoScope); !v.IsUndefined() || !cs.fellBack {
		t.Fatalf("GA = %v (fellBack %v), want a fallback", v, cs.fellBack)
	}
}

// TestColumnReadersDeclineForeignField: the per-block column readers take a field position resolved
// against the segment's schema. One that is not a field of the block's own schema declines the block
// -- the caller reads its records -- rather than index past the end of the schema.
func TestColumnReadersDeclineForeignField(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	cs, _, _ := groupSchemaReadFixture(t, c, 64)
	blk := cs.blk
	for _, idx := range []int{len(blk.schema.fields), len(blk.schema.fields) + 5, -1} {
		if blk.loadIntBatch(idx, cs.bc, make([]int64, blk.n)) {
			t.Errorf("loadIntBatch(%d) served a field the block does not have", idx)
		}
		if _, ok := blk.rawColumn(idx, cs.bc); ok {
			t.Errorf("rawColumn(%d) served a field the block does not have", idx)
		}
		if err := blk.scanInt(idx, cs.bc, func(int, bool, int64) {}); err == nil {
			t.Errorf("scanInt(%d) served a field the block does not have", idx)
		}
		if _, ok := newNumCol(blk, idx, 0, cs.bc); ok {
			t.Errorf("newNumCol(%d) served a field the block does not have", idx)
		}
		if _, ok := newGroupCol(blk, idx, 0, cs.bc, &groupColScratch{}, false); ok {
			t.Errorf("newGroupCol(%d) served a field the block does not have", idx)
		}
	}
}
