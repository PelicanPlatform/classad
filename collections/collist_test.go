package collections

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// listAd is formAd plus list-valued attributes shaped like a partitionable slot's Child* lists, with
// every cell shape the list columns must get right mixed into the same blocks:
//
//	ChildCpus   ints, empty lists, absent; a scalar in one record in 97, a list mixing int and real in one
//	            in 89, a list of reals in one in 83 (all OTHER: the record holds the attribute, the column
//	            cannot carry it)
//	ChildState  strings in mixed case; a nested list in one record in 101 (OTHER)
//	ChildGroup  strings with UNDEFINED elements
//	ChildRank   reals; a list reading ProcId in a few records (OTHER, and not literal: those blocks go
//	            back to the per-record path)
//	ChildFlags  booleans
//	ChildExpr   an expression element: always OTHER
//	AllUndef    only UNDEFINED elements
func listAd(i, excEvery int) string {
	s := formAd(i, excEvery)
	var parts []string
	if i%3 == 0 {
		var cpus, state, group, rank, flags []string
		for j := 0; j < i%9; j++ {
			cpus = append(cpus, fmt.Sprint(1+(i+j)%3))
			state = append(state, []string{`"Busy"`, `"Idle"`, `"Claimed"`, `"busy"`}[(i+j)%4])
			if (i+j)%5 == 0 {
				group = append(group, "undefined")
			} else {
				group = append(group, fmt.Sprintf(`"g%d"`, (i+j)%4))
			}
			rank = append(rank, []string{"0.0", "1.5", "0.5", "2.25"}[(i+j)%4])
			flags = append(flags, []string{"true", "false"}[(i+j)%2])
		}
		lit := func(xs []string) string { return "{" + strings.Join(xs, ", ") + "}" }
		parts = append(parts, "ChildCpus = "+lit(cpus), "ChildState = "+lit(state), "ChildGroup = "+lit(group),
			"ChildRank = "+lit(rank), "ChildFlags = "+lit(flags))
	}
	switch {
	case i%97 == 5:
		parts = append(parts, "ChildCpus = 4")
	case i%89 == 7:
		parts = append(parts, "ChildCpus = {1, 2.5}")
	case i%83 == 9:
		parts = append(parts, "ChildCpus = {2.5, 3.5}") // one kind, but not the column's
	}
	if i%79 == 11 && i%3 != 0 {
		parts = append(parts, "ChildRank = {ProcId + 0.5}") // an element that reads the record
	}
	if i%101 == 4 {
		parts = append(parts, `ChildState = {"Busy", {1}}`)
	}
	if i%7 == 0 {
		parts = append(parts, "ChildExpr = {ProcId + 1, 2}")
	}
	if i%11 == 0 {
		parts = append(parts, "AllUndef = {undefined, undefined}")
	}
	if len(parts) == 0 {
		return s
	}
	return strings.TrimSuffix(s, " ]") + "; " + strings.Join(parts, "; ") + " ]"
}

// listQueries are the constraints the list fixture is checked under.
var listQueries = []string{
	"size(ChildCpus) > 2",
	"size(ChildCpus) == 0",
	`member("busy", ChildState)`,
	`identicalMember("busy", ChildState)`,
	`identicalMember("Busy", ChildState)`,
	"member(1, ChildCpus)",
	"member(2.5, ChildCpus)",
	"member(1.0, ChildCpus)",
	"identicalMember(1.0, ChildCpus)",
	"isList(ChildCpus)",
	"ChildCpus is undefined",
	"ChildCpus isnt undefined",
	"isUndefined(ChildState)",
	"sum(ChildCpus) > 4",
	"avg(ChildRank) > 0.5",
	"min(ChildCpus) == 1",
	"max(ChildRank) >= 1.5",
	"member(0.5, ChildRank)",
	`member("g1", ChildGroup)`,
	`member(undefined, ChildGroup)`,
	`identicalMember(undefined, ChildGroup) is undefined`,
	"member(true, ChildFlags)",
	"size(ChildCpus) > 2 && ProcId > 3",
	`member("busy", ChildState) || MemUse > 2047`,
	"size(AllUndef) == 2",
	"sum(AllUndef) == 0",
	`member(Owner, ChildGroup)`,
	"size(ChildExpr) == 2",
	"member(1, ChildCpus ?: ChildRank)",
	"size(ChildCpus) + size(ChildState) > 6",
	"max(ChildRank) > 5",
	"member(2.5, ChildCpus) || member(3.5, ChildCpus)",
}

// listColumnsHeld reports how many list fields the columnarized segments of c keep, over all blocks.
func listColumnsHeld(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cn := seg.colNative.Load(); cn != nil {
				for _, b := range cn.seg.blocks {
					n += len(b.listIDs)
				}
			}
		}
	}
	return n
}

func buildListFixture(t *testing.T, n int, archive, interned bool) *formFixture {
	t.Helper()
	f := buildFormFixtureGen(t, t.TempDir(), n, 50, archive, interned, listAd)
	f.extraProj = [][]string{{"ChildCpus"}, {"ChildState", "ChildGroup", "ChildRank"}, {"ChildFlags", "AllUndef", "ChildExpr"}}
	if listColumnsHeld(f.c) == 0 {
		t.Fatal("no columnarized block keeps a list column")
	}
	odd := false
	for _, sh := range f.c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cn := seg.colNative.Load(); cn != nil {
				for _, b := range cn.seg.blocks {
					odd = odd || (len(b.listIDs) > 0 && b.n%64 != 0)
				}
			}
		}
	}
	if !odd {
		t.Fatal("every block with list columns is a multiple of 64 records; the fixture should mix sizes")
	}
	return f
}

// checkListValues compares every list query's count, by each count tier and by the filtered read,
// against evaluating each record; it fails if no query was served from a list column.
func checkListValues(t *testing.T, f *formFixture, label string) {
	t.Helper()
	c := f.c
	ads := make([]*classad.ClassAd, 0, len(f.src))
	for _, ad := range f.src {
		ads = append(ads, ad)
	}
	before := vm.ListFusedCalls.Load()
	for _, e := range listQueries {
		want := computedRefCount(t, ads, e)
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := c.CountQuery(q); ok && got != want {
			t.Errorf("%s: CountQuery(%s) = %d, want %d", label, e, got, want)
		}
		if got, ok := c.CountQueryRedacted(q); ok && got != want {
			t.Errorf("%s: CountQueryRedacted(%s) = %d, want %d", label, e, got, want)
		}
		if got, ok := c.VectorEvalCount(q); !ok || got != want {
			t.Errorf("%s: VectorEvalCount(%s) = %d (ok=%v), want %d", label, e, got, ok, want)
		}
		if got, ok := c.ColumnarEvalCount(q); ok && got != want {
			t.Errorf("%s: ColumnarEvalCount(%s) = %d, want %d", label, e, got, want)
		}
		if got := len(readAll(t, c, e)); got != want {
			t.Errorf("%s: query %s returned %d records, want %d", label, e, got, want)
		}
		if gs, ok := c.GroupStatsQuery(q, "JobStatus", nil); ok {
			sum := 0
			for _, g := range gs {
				sum += g.Count
			}
			if sum != want {
				t.Errorf("%s: GroupStatsQuery(%s) BY JobStatus sums to %d, want %d", label, e, sum, want)
			}
		}
	}
	if vm.ListFusedCalls.Load() == before {
		t.Errorf("%s: no list query was served from a list column", label)
	}
	// The blocks holding a literal exception (ChildState's nested list, ChildCpus's scalar and mixed
	// lists) are served with that record read alone, not handed back: every block of a list-only
	// constraint on them vectorizes. ChildRank's exceptions include a list reading ProcId, which only the
	// per-record path evaluates, so blocks holding one are handed back.
	exc := listExceptionEvals.Load()
	for _, e := range []string{`member("busy", ChildState)`, `size(ChildState) > 1`, "size(ChildCpus) > 2", "member(2.5, ChildCpus)"} {
		q, _ := vm.Parse(e)
		if _, ok := c.VectorEvalCount(q); !ok {
			t.Fatalf("%s: VectorEvalCount(%s) declined", label, e)
		}
		sp := lastVecSplit.Load()
		if sp.vecBlocks == 0 || sp.scopeBlocks != 0 {
			t.Errorf("%s: VectorEvalCount(%s) served %d blocks as vectors and gave %d back", label, e, sp.vecBlocks, sp.scopeBlocks)
		}
	}
	if listExceptionEvals.Load() == exc {
		t.Errorf("%s: no list-column exception was read from its record", label)
	}
}

// TestListColumnsMatchPlainEvaluation is the value differential: every count tier and the filtered
// read over list built-ins agree with evaluating each record, on a mutable table, an archive and an
// interned archive, after reopening, re-columnarizing and compacting or interning.
func TestListColumnsMatchPlainEvaluation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		archive, interned bool
	}{
		{"mutable", false, false},
		{"archive", true, false},
		{"archive-interned", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := buildListFixture(t, 1500, tc.archive, tc.interned)
			checkListValues(t, f, "reopened")
			if recolumnarizeAll(t, f.c) == 0 {
				t.Fatal("nothing was re-columnarized")
			}
			if listColumnsHeld(f.c) == 0 {
				t.Fatal("re-columnarizing dropped the list columns")
			}
			checkListValues(t, f, "re-columnarized")
			if !tc.archive {
				for i := 0; i < len(f.src); i += 97 {
					f.c.Delete([]byte(fmt.Sprintf("%d.0", i)))
					delete(f.src, int64(i))
				}
				f.c.Compact()
				checkListValues(t, f, "compacted")
			} else {
				// Interning reseals each segment in row form; its columns (lists included) come back when
				// maintenance columnarizes it again.
				f.c.InternSealed()
				f.c.ColumnarizeSealed()
				if listColumnsHeld(f.c) == 0 {
					t.Fatal("columnarizing the interned segments wrote no list column")
				}
				checkListValues(t, f, "interned")
			}
			f = f.reopen()
			checkListValues(t, f, "reopened again")
		})
	}
}

// TestListColumnFormReaders: no form reader is affected by a block keeping list columns. Every path
// that rebuilds an ad or ships attribute text, wire or nodes returns each record exactly as written,
// list attributes included, projected alone or with others.
func TestListColumnFormReaders(t *testing.T) {
	for _, tc := range []struct {
		name              string
		archive, interned bool
	}{
		{"mutable", false, false},
		{"archive", true, false},
		{"archive-interned", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := buildListFixture(t, 1200, tc.archive, tc.interned)
			checkFormReaders(t, f, "reopened")
			if recolumnarizeAll(t, f.c) == 0 {
				t.Fatal("nothing was re-columnarized")
			}
			checkFormReaders(t, f, "re-columnarized")
			f = f.reopen()
			checkFormReaders(t, f, "reopened again")
		})
	}
}

// TestListColumnsByteIdentical requires every raw wire read -- full and projected, privileged and
// redacted -- to return identical bytes whether or not the segments keep list columns. One store is
// columnarized without them, re-columnarized under the same schema with them, reopened, and
// re-columnarized without them again (the control: a rewrite that changes nothing changes no byte).
func TestListColumnsByteIdentical(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive=%v", archive), func(t *testing.T) {
			prev := colListColumns
			t.Cleanup(func() { colListColumns = prev })
			colListColumns = false
			f := buildFormFixtureGen(t, t.TempDir(), 900, 50, archive, false, listAd)
			if listColumnsHeld(f.c) != 0 {
				t.Fatal("list columns were written with the gate off")
			}
			all, _ := vm.Parse("true")
			sel, _ := vm.Parse(`member("busy", ChildState) || ProcId >= 0`)
			projs := [][]string{nil, {"ChildCpus"}, {"ChildState", "Owner"}, {"ChildExpr", "AllUndef"}, {"MemUse"}}
			collect := func(f *formFixture) map[string][]string {
				out := map[string][]string{}
				for _, proj := range projs {
					for _, redact := range []bool{false, true} {
						key := fmt.Sprintf("%v redact=%v", proj, redact)
						var ws []string
						for w := range f.c.QueryRawWire(sel, proj, redact) {
							ws = append(ws, string(w))
						}
						for w := range f.c.ScanRawWire(proj, redact) {
							ws = append(ws, string(w))
						}
						for w := range f.c.QueryRawWire(all, proj, redact) {
							ws = append(ws, string(w))
						}
						slices.Sort(ws)
						out[key] = ws
					}
				}
				return out
			}
			same := func(label string, a, b map[string][]string) {
				t.Helper()
				for key, wa := range a {
					wb := b[key]
					if len(wa) == 0 || len(wa) != len(wb) {
						t.Fatalf("%s %s: %d vs %d ads", label, key, len(wa), len(wb))
					}
					for i := range wa {
						if !bytes.Equal([]byte(wa[i]), []byte(wb[i])) {
							t.Fatalf("%s %s: the wire bytes differ:\n%v\n%v", label, key,
								wireToAd(t, f.c, []byte(wa[i])), wireToAd(t, f.c, []byte(wb[i])))
						}
					}
				}
			}
			base := collect(f)
			colListColumns = true
			if recolumnarizeAll(t, f.c) == 0 || listColumnsHeld(f.c) == 0 {
				t.Fatal("re-columnarizing with the gate on wrote no list column")
			}
			same("with list columns", base, collect(f))
			f = f.reopen()
			if listColumnsHeld(f.c) == 0 {
				t.Fatal("reopening dropped the list columns")
			}
			same("reopened with list columns", base, collect(f))
			colListColumns = false
			if recolumnarizeAll(t, f.c) == 0 || listColumnsHeld(f.c) != 0 {
				t.Fatal("re-columnarizing with the gate off kept list columns")
			}
			same("control", base, collect(f))
		})
	}
}

// TestListColumnRejectsCorruptBody: a list body whose offsets or codes do not describe a column of the
// block is refused rather than read, so a damaged section costs speed and never a wrong answer.
func TestListColumnRejectsCorruptBody(t *testing.T) {
	rows := []listRow{
		{lists: []listCell{{id: 7, kind: akString, node: mustListNode(t, `{"a", "b"}`)}}, ids: []uint32{7}},
		{ids: []uint32{3}},
		{lists: []listCell{{id: 7, kind: akString, node: mustListNode(t, `{"b"}`)}}, ids: []uint32{7}},
		{lists: []listCell{{id: 7, kind: akNone, node: mustListNode(t, `{}`)}}, ids: []uint32{7}},
	}
	name := func(uint32) (string, bool) { return "L", true }
	ids, _, body := buildBlockLists(rows, name, nil)
	if len(ids) != 1 {
		t.Fatalf("%d fields, want 1", len(ids))
	}
	b := &columnarBlock{n: len(rows)}
	var buf []string
	if lc, _, ok := b.listColumn(body, 0, &buf); !ok || !lc.Has(0) || lc.Has(1) || !lc.Has(3) {
		t.Fatalf("the intact body does not read back: %v", ok)
	}
	const offs = 8 + 1 // kind, flags, presence bitmap of 4 records
	put := func(src []byte, at int, v uint32) []byte {
		out := bytes.Clone(src)
		binary.LittleEndian.PutUint32(out[at:], v)
		return out
	}
	for name, bad := range map[string][]byte{
		"decreasing offsets":          put(body, offs+4*3, 1),
		"offsets past the child":      put(put(body, offs+4*3, 9), offs+4*4, 9),
		"elements for an absent cell": put(put(body, offs+4*2, 3), offs+4*3, 3),
		"a nonzero first offset":      put(body, offs, 1),
		"a code past the dictionary":  append(bytes.Clone(body[:len(body)-1]), 9),
		"a truncated body":            body[:len(body)-2],
		"an unknown flag":             put(body, 4, 1<<9),
	} {
		if _, _, ok := b.listColumn(bad, 0, &buf); ok {
			t.Errorf("%s: the corrupt body was read", name)
		}
	}
}

func mustListNode(t *testing.T, src string) []byte {
	t.Helper()
	e, err := classad.ParseExpr(src)
	if err != nil {
		t.Fatal(err)
	}
	ad := wire.EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "L", Value: e.AST()}}})
	node, ok := wire.Ad(ad).LookupByName("L")
	if !ok {
		t.Fatal("not found")
	}
	return node
}

// TestListTrailerSkipsUnknownSections: the extension trailer is read section by section, so a section a
// later build adds is skipped while the list section beside it is still read -- and bytes after the
// trailers that are not an extension trailer drop every trailer, computed values and lists included.
func TestListTrailerSkipsUnknownSections(t *testing.T) {
	f := buildListFixture(t, 900, true, false)
	var cs *colSegment
	for _, sh := range f.c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cn := seg.colNative.Load(); cn != nil && cn.seg.hasLists() && cn.seg.hasComputed() && cn.seg.remnants != nil {
				cs = cn.seg
			}
		}
	}
	if cs == nil {
		t.Fatal("no segment carries lists, computed values and a remnant list")
	}
	nameOf := func(id uint32) (string, bool) { return f.c.intern.Name(id) }
	// The payload without its extension trailer, then one written by hand: an unknown section first.
	saved := make([][]uint32, len(cs.blocks))
	for i, b := range cs.blocks {
		saved[i] = b.listIDs
		b.listIDs = nil
	}
	bare := marshalColPayload(cs, nameOf)
	for i, b := range cs.blocks {
		b.listIDs = saved[i]
	}
	ext := appendU32(nil, colExtTrailer)
	ext = appendU32(ext, 2)
	ext = appendU32(ext, 0x4b4e4b55) // a tag no build knows
	ext = appendBytes(ext, []byte("from a later build"))
	ext = appendU32(ext, colExtLists)
	ext = appendBytes(ext, appendListSection(nil, cs.blocks))
	intern := func(name string) uint32 { return f.c.intern.Intern(name) }
	read := func(blob []byte) *colSegment {
		got, err := unmarshalColPayload(blob, f.c.regionCodec(), intern)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := read(append(bytes.Clone(bare), ext...))
	if !got.hasLists() || !got.hasComputed() || got.remnants == nil {
		t.Fatalf("with an unknown section: lists %v, computed %v, remnant list %v; want all three",
			got.hasLists(), got.hasComputed(), got.remnants != nil)
	}
	got = read(append(append(bytes.Clone(bare), ext...), 0x01))
	if got.hasLists() || got.hasComputed() || got.remnants != nil {
		t.Fatalf("with trailing damage: lists %v, computed %v, remnant list %v; want none",
			got.hasLists(), got.hasComputed(), got.remnants != nil)
	}
}
