package collections

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// listMovedCells counts the list cells c's columnarized segments hold only in their columns.
func listMovedCells(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cn := seg.colNative.Load(); cn != nil {
				for _, b := range cn.seg.blocks {
					n += b.listMoved
				}
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// payloadVersions reads every columnarized segment's payload version from its stored header, and checks
// that version 3 is written exactly for the segments holding a moved list.
func payloadVersions(t *testing.T, c *Collection) map[uint32]int {
	t.Helper()
	out := map[uint32]int{}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			_, v, err := splitColPayload(recAd(seg.data, cn.rec))
			if err != nil {
				t.Errorf("%s: %v", seg.path, err)
				continue
			}
			out[v]++
			if moved := cn.seg.hasMovedLists(); moved != (v == colPayloadVersionLists) {
				t.Errorf("%s: payload version %d for a segment holding moved lists=%v", seg.path, v, moved)
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// nonCanonicalList rewrites list node in a form a reader accepts and the encoder never writes: its
// element count spelled with a redundant continuation byte, and, when the first element is an integer, that
// integer's varint likewise. The values are unchanged.
func nonCanonicalList(t *testing.T, node []byte) []byte {
	t.Helper()
	var elems [][]byte
	n, ok := wire.ScalarListElems(node, func(e []byte) bool { elems = append(elems, e); return true })
	if !ok || n >= 0x80 {
		t.Fatalf("not a short scalar list: %x", node)
	}
	out := []byte{node[0], 0x80 | byte(n), 0x00}
	for i, e := range elems {
		if i == 0 && wire.ElemKind(e) == wire.LitInt && len(e) == 2 && e[1] < 0x80 {
			out = append(out, e[0], 0x80|e[1], 0x00)
			continue
		}
		out = append(out, e...)
	}
	if _, ok := wire.ScalarListElems(out, nil); !ok {
		t.Fatalf("the reader rejects the non-canonical list %x", out)
	}
	return out
}

// putNonCanonical writes ad under key with each named list attribute's node re-spelled by
// nonCanonicalList, as Put would otherwise write it.
func putNonCanonical(t *testing.T, c *Collection, key []byte, src string, lists map[string]bool) {
	t.Helper()
	raw, err := c.encodeAd(mustAd(t, src).AST())
	if err != nil {
		t.Fatal(err)
	}
	hdr, ents, n, inline, ok := wire.Ad(raw).SplitBody()
	if !ok {
		t.Fatal("encoded ad does not split")
	}
	var out []byte
	changed := 0
	if !remnantEntries(ents, n, inline, func(k, node []byte) bool {
		out = append(out, k...)
		if _, name, _ := remnantKey(k, inline); lists[strings.ToLower(name)] {
			if _, isList := wire.ScalarListElems(node, nil); isList {
				out = append(out, nonCanonicalList(t, node)...)
				changed++
				return true
			}
		}
		out = append(out, node...)
		return true
	}) {
		t.Fatal("encoded ad does not walk")
	}
	if changed == 0 {
		t.Fatal("no list was re-spelled")
	}
	w := wire.BuildAd(nil, hdr, n, out)
	codec := c.currentCodec()
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.commitOne(pendingPut{hash: h, key: key, ad: codec.Compress(nil, w), codec: codec, ct: c.namesCurrentTime(w)})
	c.indexSealedSegments()
	c.collapseSealedChains()
}

// collectRawReads gathers every raw read of c, sorted per path: full and projected (with and without a list
// attribute), privileged and redacted, wire and text.
func collectRawReads(t *testing.T, c *Collection, mutable bool) map[string][]string {
	t.Helper()
	all, _ := vm.Parse("true")
	sel, _ := vm.Parse(`member("busy", ChildState) || ProcId >= 0`)
	projs := [][]string{nil, {"ChildCpus"}, {"ChildState", "Owner"}, {"ChildExpr", "AllUndef"}, {"MemUse"}, {"Owner", "ProcId"}}
	out := map[string][]string{}
	add := func(key string, s string) { out[key] = append(out[key], s) }
	for _, proj := range projs {
		for _, redact := range []bool{false, true} {
			key := fmt.Sprintf("%v redact=%v", proj, redact)
			for w := range c.QueryRawWire(sel, proj, redact) {
				add("QueryRawWire(sel) "+key, string(w))
			}
			for w := range c.QueryRawWire(all, proj, redact) {
				add("QueryRawWire(all) "+key, string(w))
			}
			for w := range c.ScanRawWire(proj, redact) {
				add("ScanRawWire "+key, string(w))
			}
			if proj != nil {
				for w := range c.QueryRawWireRefs(sel, proj, redact) {
					add("QueryRawWireRefs "+key, string(w))
				}
				for r := range c.QueryRawProjected(sel, proj, false, redact) {
					add("QueryRawProjected "+key, string(bytes.Join(r.Exprs, []byte("; "))))
				}
			}
		}
	}
	for r := range c.ScanRaw() {
		add("ScanRaw", string(bytes.Join(r.Exprs, []byte("; "))))
	}
	if mutable {
		for ad := range c.Scan() {
			cid, _ := ad.EvaluateAttrInt("ClusterId")
			g, ok := c.Get([]byte(fmt.Sprintf("%d.0", cid)))
			if !ok {
				t.Fatalf("Get(%d.0) found nothing", cid)
			}
			add("Get", g.String())
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

// sameReads fails unless a and b hold the same bytes on every path, and every path read something.
func sameReads(t *testing.T, label string, a, b map[string][]string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d read paths vs %d", label, len(a), len(b))
	}
	for key, wa := range a {
		wb := b[key]
		if len(wa) == 0 || len(wa) != len(wb) {
			t.Fatalf("%s %s: %d vs %d results", label, key, len(wa), len(wb))
		}
		for i := range wa {
			if wa[i] != wb[i] {
				t.Fatalf("%s %s: result %d differs:\n%q\n%q", label, key, i, wa[i], wb[i])
			}
		}
	}
}

// setListMode sets whether columnarizing writes list columns and whether it moves them, and rewrites every
// columnarized segment under it.
func setListMode(t *testing.T, c *Collection, columns bool, moves int) {
	t.Helper()
	colListColumns = columns
	c.listMoves = moves
	if recolumnarizeAll(t, c) == 0 {
		t.Fatal("nothing was re-columnarized")
	}
}

// TestListMovesByteIdentical is the form-reader differential at the byte level. One store is read with no
// list columns at all (what main writes), with list columns as copies (moving off), and with lists moved
// out of their records; every raw read -- full and projected, with and without a list attribute,
// privileged and redacted, wire and text, and Get -- must return identical bytes in all three. The mutable
// store also holds records whose lists are spelled non-canonically: those cells must stay in their
// records, and read back exactly as written. Then: after a reopen, after compaction (mutable) or
// interning (archive), moving on and moving off must still agree, and a final rewrite without list
// columns must give back the first bytes.
func TestListMovesByteIdentical(t *testing.T) {
	prev := colListColumns
	t.Cleanup(func() { colListColumns = prev })
	for _, tc := range []struct {
		name              string
		archive, interned bool
	}{
		{"mutable", false, false},
		{"archive", true, false},
		{"archive-interned", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			colListColumns = false
			var f *formFixture
			nonCanon := 0
			adir := t.TempDir()
			if tc.archive {
				f = buildFormFixtureOpts(t, adir, 1100, 50, true, tc.interned, -1, listAd)
			} else {
				dir := t.TempDir()
				c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, MoveListColumns: -1})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				lists := map[string]bool{"childcpus": true, "childstate": true, "childgroup": true}
				for i := range 1100 {
					key := []byte(fmt.Sprintf("%d.0", i))
					if i%3 == 0 && i%9 != 0 && i%37 == 3 {
						putNonCanonical(t, c, key, listAd(i, 50), lists)
						nonCanon++
						continue
					}
					if err := c.Put(key, mustAd(t, listAd(i, 50))); err != nil {
						t.Fatal(err)
					}
				}
				if !c.BuildAndEnableSchemaScan(4096, 8) {
					t.Fatal("no schema derived")
				}
				f = &formFixture{c: c}
				if nonCanon == 0 {
					t.Fatal("fixture: no non-canonical record")
				}
			}
			c := f.c
			if listColumnsHeld(c) != 0 {
				t.Fatal("list columns were written with the gate off")
			}
			base := collectRawReads(t, c, !tc.archive)

			setListMode(t, c, true, -1)
			if listColumnsHeld(c) == 0 || listMovedCells(c) != 0 || payloadVersions(t, c)[colPayloadVersionLists] != 0 {
				t.Fatal("copy arm: want list columns and no moved cell")
			}
			sameReads(t, "list columns as copies", base, collectRawReads(t, c, !tc.archive))

			_, kept0 := ListCellsMoved()
			setListMode(t, c, true, 1)
			_, kept1 := ListCellsMoved()
			if listMovedCells(c) == 0 || payloadVersions(t, c)[colPayloadVersionLists] == 0 {
				t.Fatal("move arm: no list cell moved, or no version 3 payload")
			}
			if !tc.archive && kept1-kept0 < int64(nonCanon) {
				t.Fatalf("move arm: %d cells kept for non-canonical bytes, want at least %d", kept1-kept0, nonCanon)
			}
			moved := collectRawReads(t, c, !tc.archive)
			sameReads(t, "lists moved", base, moved)

			if tc.archive {
				if err := f.close(); err != nil {
					t.Fatal(err)
				}
				f = reopenFormFixture(t, adir, f, true, 1)
				c = f.c
			} else {
				dir := c.dir
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, MoveListColumns: 1})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c2.Close() })
				c2.BuildAndEnableSchemaScan(4096, 8)
				c = c2
			}
			if listMovedCells(c) == 0 {
				t.Fatal("reopening lost the moved lists")
			}
			sameReads(t, "lists moved, reopened", base, collectRawReads(t, c, !tc.archive))

			// A rewrite of the records: compaction de-columnarizes what it rewrites, interning reseals in row
			// form; columnarizing again moves the lists again. Moving on and off must agree after it.
			if tc.archive {
				c.InternSealed()
			} else {
				for i := 0; i < 1100; i += 13 {
					c.Delete([]byte(fmt.Sprintf("%d.0", i)))
				}
				c.Compact()
			}
			c.listMoves = 1
			c.ColumnarizeSealed()
			setListMode(t, c, true, 1)
			if listMovedCells(c) == 0 {
				t.Fatal("after the rewrite no list cell moved")
			}
			on := collectRawReads(t, c, !tc.archive)
			setListMode(t, c, true, -1)
			if listMovedCells(c) != 0 {
				t.Fatal("moving off kept moved cells")
			}
			sameReads(t, "after the rewrite, moving on vs off", on, collectRawReads(t, c, !tc.archive))
			if tc.archive {
				setListMode(t, c, false, -1)
				sameReads(t, "control", base, collectRawReads(t, c, false))
			}
		})
	}
}

// TestListMovesDowngrade: a store whose lists were moved, reopened with MoveListColumns negative, is
// brought back by the routine schema review to payloads an older build reads -- no segment holds a moved
// list, fsck finds no version 3 payload -- and reads every record as written, before and after.
func TestListMovesDowngrade(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive=%v", archive), func(t *testing.T) {
			dir := t.TempDir()
			f := buildFormFixtureOpts(t, dir, 1200, 50, archive, false, 1, listAd)
			f.extraProj = [][]string{{"ChildCpus"}, {"ChildState", "ChildGroup"}}
			if listMovedCells(f.c) == 0 || payloadVersions(t, f.c)[colPayloadVersionLists] == 0 {
				t.Fatal("fixture: no moved list")
			}
			checkFormReaders(t, f, "moved")
			before := collectRawReads(t, f.c, !archive)
			if err := f.close(); err != nil {
				t.Fatal(err)
			}
			rep, err := Fsck(dir)
			if err != nil {
				t.Fatal(err)
			}
			v3 := 0
			for _, s := range rep.Segments {
				if s.ColumnarVersion == colPayloadVersionLists {
					v3++
				}
			}
			if v3 == 0 {
				t.Fatal("fsck finds no version 3 payload in a store with moved lists")
			}
			// Reopened with moving off: the schema review is what brings the segments back.
			g := reopenFormFixture(t, dir, f, archive, -1)
			if listMovedCells(g.c) != 0 {
				t.Fatalf("after the schema review %d list cells are still moved", listMovedCells(g.c))
			}
			if vs := payloadVersions(t, g.c); vs[colPayloadVersionLists] != 0 {
				t.Fatalf("payload versions after the downgrade: %v", vs)
			}
			if listColumnsHeld(g.c) == 0 {
				t.Fatal("the downgrade dropped the list columns rather than keeping them as copies")
			}
			checkFormReaders(t, g, "moved back")
			sameReads(t, "moved back", before, collectRawReads(t, g.c, !archive))
			if err := g.close(); err != nil {
				t.Fatal(err)
			}
			if rep, err = Fsck(dir); err != nil {
				t.Fatal(err)
			}
			for _, s := range rep.Segments {
				if s.ColumnarVersion >= colPayloadVersionLists || s.ListSectionDamage != "" {
					t.Errorf("%s: payload version %d after the downgrade", filepath.Base(s.Path), s.ColumnarVersion)
				}
			}
		})
	}
}

// reopenFormFixture opens f's store again with Options.MoveListColumns set to moves, and runs the schema
// review as maintenance would.
func reopenFormFixture(t *testing.T, dir string, f *formFixture, archive bool, moves int) *formFixture {
	t.Helper()
	g := &formFixture{archive: archive, src: f.src, ever: f.ever, extraProj: f.extraProj}
	if archive {
		a, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, GroupSchemaCount: -1, MoveListColumns: moves})
		if err != nil {
			t.Fatal(err)
		}
		g.c, g.close = a.c, a.Close
	} else {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, WatchHistory: 64, MoveListColumns: moves,
			TimeTravel: &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond}})
		if err != nil {
			t.Fatal(err)
		}
		g.c, g.close = c, c.Close
	}
	t.Cleanup(func() { g.close() })
	g.c.BuildAndEnableSchemaScan(4096, 8)
	return g
}

// TestTagListRefRejectedByWire: a remnant that reaches one of wire's readers with a placeholder still in it
// fails to read; it never reads as a record without the attribute.
func TestTagListRefRejectedByWire(t *testing.T) {
	ref := []byte{wire.TagListRef, 0x00}
	if got := wire.ClassifyNode(ref); got != wire.ClassInvalid {
		t.Errorf("ClassifyNode(TagListRef) = %v, want invalid", got)
	}
	if _, ok := wire.NodeLen(ref); ok {
		t.Error("NodeLen accepted a TagListRef node")
	}
	if _, err := wire.DecodeNodeInline(ref); err == nil {
		t.Error("DecodeNodeInline accepted a TagListRef node")
	}
	hdr, _, _, _, _ := wire.Ad(wire.EncodeInline(nil, mustAd(t, "[X = 1]").AST())).SplitBody()
	ad := wire.BuildAd(nil, hdr, 1, append(binary.AppendUvarint(nil, 1), append([]byte("L"), ref...)...))
	if _, err := wire.DecodeInline(ad); err == nil {
		t.Error("DecodeInline accepted an ad holding a TagListRef node")
	}
	if _, ok := wire.Ad(ad).LookupByName("L"); ok {
		t.Error("LookupByName returned a TagListRef node as a value")
	}
}

// listFuzzElem appends a random scalar element node, canonical or (with spell) not.
func listFuzzElem(r *rand.Rand, kind int, spell bool) []byte {
	switch {
	case r.IntN(7) == 0:
		return wire.AppendUndefinedNode(nil)
	case kind == 0:
		v := []int64{0, 1, -1, 63, -64, 64, math.MaxInt64, math.MinInt64, r.Int64()}[r.IntN(9)]
		n := wire.AppendIntNode(nil, v)
		if spell && len(n) < 10 && r.IntN(4) == 0 {
			n[len(n)-1] |= 0x80
			n = append(n, 0x00)
		}
		return n
	case kind == 1:
		f := []float64{0, math.Copysign(0, -1), math.NaN(), math.Inf(1), 1.5, r.Float64()}[r.IntN(6)]
		n := wire.AppendRealNode(nil, f)
		if r.IntN(9) == 0 { // a NaN with a payload of its own
			binary.LittleEndian.PutUint64(n[1:], 0x7ff8000000000000|uint64(r.IntN(1<<20))+1)
		}
		return n
	case kind == 2:
		return wire.AppendBoolNode(nil, r.IntN(2) == 0)
	}
	return wire.AppendStringNode(nil, []string{"", "busy", "Busy", "g1", strings.Repeat("x", 200)}[r.IntN(5)])
}

// listFuzzNode builds a random list node: usually a qualifying list of one kind, sometimes a near miss
// (mixed kinds, a nested list, an ERROR element), sometimes spelled non-canonically.
func listFuzzNode(r *rand.Rand, kind int) []byte {
	spell := r.IntN(5) == 0
	n := r.IntN(6)
	var elems [][]byte
	for range n {
		k := kind
		if r.IntN(15) == 0 {
			k = r.IntN(4)
		}
		elems = append(elems, listFuzzElem(r, k, spell))
	}
	switch r.IntN(25) {
	case 0:
		elems = append(elems, wire.AppendListHeader(nil, 0)) // a nested list
	case 1:
		elems = append(elems, []byte{0x01}) // ERROR
	}
	out := wire.AppendListHeader(nil, len(elems))
	if spell && len(elems) < 0x80 {
		out = []byte{out[0], 0x80 | byte(len(elems)), 0x00}
	}
	for _, e := range elems {
		out = append(out, e...)
	}
	return out
}

// checkListMoveRoundTrip builds one block's list columns from records holding the given list nodes under
// names L0..L2, moves what qualifies out of the records, and requires each rewritten record to expand,
// through the reader's expansion over the compressed column, to exactly the record it was. It returns how
// many cells moved and how many records were rewritten.
func checkListMoveRoundTrip(t *testing.T, c *Collection, recs [][][]byte) (moved int) {
	t.Helper()
	names := []string{"L0", "L1", "L2"}
	inlineHdr, _, _, _, ok := wire.Ad(wire.EncodeInline(nil, mustAd(t, "[X = 1]").AST())).SplitBody()
	if !ok {
		t.Fatal("an inline ad does not split")
	}
	ids := make([]uint32, len(names))
	for i, nm := range names {
		ids[i] = c.intern.Intern(nm)
	}
	var rows []listRow
	var bodies, orig [][]byte
	for _, rec := range recs {
		var ents []byte
		var row listRow
		ents = wire.AppendKey(ents, true, 0, "X")
		ents = wire.AppendIntNode(ents, 1)
		cnt := 1
		for j, node := range rec {
			if node == nil {
				continue
			}
			ents = wire.AppendKey(ents, true, 0, names[j])
			ents = append(ents, node...)
			cnt++
			row.ids = append(row.ids, ids[j])
			if kind, ok := listKind(node); ok {
				row.lists = append(row.lists, listCell{id: ids[j], kind: kind, node: node})
			}
		}
		body := wire.BuildAd(nil, inlineHdr, cnt, ents)
		if _, _, _, inline, ok := wire.Ad(body).SplitBody(); !ok || !inline {
			t.Fatalf("test body does not split as inline: %x", body)
		}
		rows = append(rows, row)
		bodies = append(bodies, body)
		orig = append(orig, bytes.Clone(body))
	}
	codec := c.regionCodec()
	b := &columnarBlock{n: len(recs), codec: codec, id: colBlockSeq.Add(1)}
	attachBlockLists(b, rows, codec, func(id uint32) (string, bool) { return c.intern.Name(id) }, nil)
	c.moveBlockLists([]*columnarBlock{b}, bodies, nil, codec)
	for k := range bodies {
		got, _, err := b.expandRemnant(bodies[k], k, nil, nil)
		if err != nil {
			t.Fatalf("record %d: %v (rewritten %x)", k, err, bodies[k])
		}
		if !bytes.Equal(got, orig[k]) {
			t.Fatalf("record %d expands to\n%x\nwant\n%x", k, got, orig[k])
		}
	}
	return b.listMoved
}

// TestListMoveRoundTripRandom drives checkListMoveRoundTrip over random blocks.
func TestListMoveRoundTripRandom(t *testing.T) {
	c, err := Open(Options{Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := rand.New(rand.NewPCG(1, 2))
	moved, kept0 := 0, listCellsKept.Load()
	for range 400 {
		n := 1 + r.IntN(90)
		kinds := []int{r.IntN(4), r.IntN(4), r.IntN(4)}
		recs := make([][][]byte, n)
		for k := range recs {
			recs[k] = make([][]byte, 3)
			for j := range 3 {
				if r.IntN(4) != 0 {
					recs[k][j] = listFuzzNode(r, kinds[j])
				}
			}
		}
		moved += checkListMoveRoundTrip(t, c, recs)
	}
	if moved == 0 || listCellsKept.Load() == kept0 {
		t.Fatalf("moved %d, kept %d: the corpus should exercise both", moved, listCellsKept.Load()-kept0)
	}
}

// FuzzListMoveRoundTrip: build, move and reassemble any block of lists; the records always come back
// byte for byte.
func FuzzListMoveRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint8(10))
	f.Add(uint64(7), uint8(64))
	c, err := Open(Options{Shards: 1})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { c.Close() })
	f.Fuzz(func(t *testing.T, seed uint64, n uint8) {
		r := rand.New(rand.NewPCG(seed, uint64(n)))
		kinds := []int{r.IntN(4), r.IntN(4), r.IntN(4)}
		recs := make([][][]byte, 1+int(n)%120)
		for k := range recs {
			recs[k] = make([][]byte, 3)
			for j := range 3 {
				if r.IntN(4) != 0 {
					recs[k][j] = listFuzzNode(r, kinds[j])
				}
			}
		}
		checkListMoveRoundTrip(t, c, recs)
	})
}

// TestListMoveProofRefusesBadRewrite: a rewritten record whose expansion is not its original bytes is left
// whole. The test hook damages the rewrite the proof sees.
func TestListMoveProofRefusesBadRewrite(t *testing.T) {
	c, err := Open(Options{Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	node := func(s string) []byte { return mustListNode(t, s) }
	recs := [][][]byte{{node("{1, 2}"), node(`{"a"}`), nil}, {node("{3}"), node(`{"b", "c"}`), nil}}
	if checkListMoveRoundTrip(t, c, recs) == 0 {
		t.Fatal("the intact rewrite moved nothing")
	}
	listMoveTestHook = func(b []byte) []byte {
		// Point every placeholder at field 0: the rewrite still parses and resolves, but to the wrong list.
		for i := 0; i+1 < len(b); i++ {
			if b[i] == wire.TagListRef {
				b[i+1] = 0
			}
		}
		return b
	}
	defer func() { listMoveTestHook = nil }()
	fails := listMoveProofFailures.Load()
	if moved := checkListMoveRoundTrip(t, c, recs); moved != 0 {
		t.Fatalf("%d cells moved under a rewrite that does not expand to the record", moved)
	}
	if listMoveProofFailures.Load() == fails {
		t.Fatal("the refused rewrite was not counted")
	}
}

// TestListMovesNonCanonicalStay: a list whose stored bytes are not what the column re-encodes stays in its
// record, and reads back as stored; its canonical neighbours move.
func TestListMovesNonCanonicalStay(t *testing.T) {
	c, err := Open(Options{Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	canon := mustListNode(t, "{1, 2, 3}")
	odd := nonCanonicalList(t, canon)
	recs := [][][]byte{{canon, nil, nil}, {odd, nil, nil}, {canon, nil, nil}}
	kept := listCellsKept.Load()
	if moved := checkListMoveRoundTrip(t, c, recs); moved != 2 {
		t.Fatalf("%d cells moved, want the 2 canonical ones", moved)
	}
	if listCellsKept.Load()-kept != 1 {
		t.Fatalf("%d cells kept, want 1", listCellsKept.Load()-kept)
	}
}

// TestListSectionDamageFailsReads: a version 3 payload whose list section does not parse makes the payload
// malformed; a list body that does not decode fails the reads of records holding a placeholder into it,
// and fsck names it. Nothing returns a record short of its list.
func TestListSectionDamageFailsReads(t *testing.T) {
	f := buildListFixtureMoves(t, 900, true, false, 1)
	var seg *segment
	for _, sh := range f.c.shards {
		for _, s := range sh.segs {
			if s != nil && s.colNative.Load() != nil && s.colNative.Load().seg.hasMovedLists() {
				seg = s
			}
		}
	}
	if seg == nil {
		t.Fatal("fixture: no segment with moved lists")
	}
	cn := seg.colNative.Load()
	blob := bytes.Clone(recAd(seg.data, cn.rec))
	intern := func(name string) uint32 { return f.c.intern.Intern(name) }
	if _, err := unmarshalColPayload(blob, f.c.regionCodec(), intern); err != nil {
		t.Fatalf("the intact payload does not read: %v", err)
	}
	at := bytes.Index(blob, binary.LittleEndian.AppendUint32(nil, colListBodySection))
	if at < 0 {
		t.Fatal("no list section in a version 3 payload")
	}
	for name, bad := range map[string][]byte{
		"truncated in the section": blob[:at+12],
		"the section's magic":      append(append(bytes.Clone(blob[:at]), 0, 0, 0, 0), blob[at+4:]...),
	} {
		if _, err := unmarshalColPayload(bad, f.c.regionCodec(), intern); err == nil {
			t.Errorf("%s: a damaged list section was accepted", name)
		}
	}
	// Bytes after the trailers that are not an extension trailer drop the trailers -- but not a version 3
	// body's lists, which are the only copy of what the records' placeholders stand for.
	if cs, err := unmarshalColPayload(append(bytes.Clone(blob), 0x01), f.c.regionCodec(), intern); err != nil {
		t.Fatalf("trailing damage failed the payload: %v", err)
	} else if !cs.hasMovedLists() || !cs.hasLists() {
		t.Fatal("trailing damage dropped a version 3 payload's list columns")
	}
	// Moved counts the cells cannot hold.
	cs, _ := unmarshalColPayload(blob, f.c.regionCodec(), intern)
	cs.blocks[0].listMoved = cs.blocks[0].n*len(cs.blocks[0].listIDs) + 1
	if cs.blocks[0].listMoved > 0 && len(cs.blocks[0].listIDs) > 0 {
		re := marshalColPayload(cs, func(id uint32) (string, bool) { return f.c.intern.Name(id) })
		if _, err := unmarshalColPayload(re, f.c.regionCodec(), intern); err == nil {
			t.Error("a moved count past the block's cells was accepted")
		}
	}

	// A list body that does not decode, in a payload that parses: every record whose placeholder points
	// into it fails, and none comes back without its list.
	var blk *columnarBlock
	for _, b := range cn.seg.blocks {
		if b.listMoved > 0 {
			blk = b
			break
		}
	}
	saved := blk.listComp
	blk.listComp = []byte("not a zstd frame")
	cn.cache.c.Clear()
	defer func() { blk.listComp = saved; cn.cache.c.Clear() }()
	failed := 0
	for k := 0; k < cn.seg.offsLen(); k++ {
		b, _ := cn.blockFor(k)
		if b != blk {
			continue
		}
		w, err := f.c.recordWire(seg, cn.seg.offAt(k))
		if err != nil {
			failed++
			continue
		}
		if !bytes.Contains(w, []byte("ChildCpus")) {
			continue
		}
		ad := wireToAd(t, f.c, w)
		if _, ok := ad.Lookup("ChildCpus"); !ok {
			t.Fatalf("record %d read without its list", k)
		}
	}
	if failed == 0 {
		t.Fatal("no read failed over a list body that does not decode")
	}
	// A rewrite reading the segment -- re-columnarizing reconstructs every record -- refuses rather than
	// carry forward fewer records than it read.
	if offs, _, ok := f.c.reconstructFullRecords(seg, nil, nil); ok {
		t.Fatalf("reconstructing the segment over an unreadable list body returned %d records and no failure", len(offs))
	}
	s := &FsckSegment{}
	fsckListSection(s, cn.seg)
	if s.ListSectionDamage == "" || !segmentNeedsRepair(s) {
		t.Fatalf("fsck did not report the damaged list body: %+v", s)
	}
	blk.listComp = saved
	s = &FsckSegment{}
	fsckListSection(s, cn.seg)
	if s.ListSectionDamage != "" {
		t.Fatalf("fsck reports an intact list body: %s", s.ListSectionDamage)
	}
}

// TestListMovesOnDiskDamage: a byte flipped inside a version 3 payload's list section on disk fails the
// payload's checksum at reopen; the segment's records then fail to read rather than read short, and Fsck
// reports the damage.
func TestListMovesOnDiskDamage(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	f := buildFormFixtureOpts(t, dir, 900, 50, true, false, 1, listAd)
	var path string
	var at int
	for _, sh := range f.c.shards {
		for _, s := range sh.segs {
			if s == nil {
				continue
			}
			if cn := s.colNative.Load(); cn != nil && cn.seg.hasMovedLists() {
				blob := recAd(s.data, cn.rec)
				i := bytes.Index(blob, binary.LittleEndian.AppendUint32(nil, colListBodySection))
				if i < 0 {
					t.Fatal("no list section")
				}
				path = s.path
				start := int(cn.rec) + recKeyOff + int(recKeyLen(s.data, cn.rec)) + 4 // where recAd begins
				if !bytes.Equal(s.data[start:start+len(blob)], blob) {
					t.Fatal("the payload is not where the record says")
				}
				at = start + i + 40
			}
		}
	}
	if path == "" {
		t.Fatal("fixture: no moved list")
	}
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[at] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	for _, s := range rep.Segments {
		if filepath.Base(s.Path) == filepath.Base(path) && (len(s.Damage) > 0 || s.ColumnarMismatch != "" || s.ListSectionDamage != "") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("fsck did not report the flipped byte:\n%s", rep)
	}
	a, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	total := func() int64 {
		n := int64(0)
		for _, v := range RecordReadSkips() {
			n += v
		}
		return n
	}
	skips := total()
	n := 0
	all, _ := vm.Parse("true")
	for ad := range a.Query(all) {
		n++
		if cid, _ := ad.EvaluateAttrInt("ClusterId"); cid%3 == 0 && cid%9 != 0 {
			if _, ok := ad.Lookup("ChildCpus"); !ok {
				t.Fatalf("record %d read without its list", cid)
			}
		}
	}
	if n == len(f.src) || total() == skips {
		t.Fatalf("read %d of %d records with no read failure counted; the damaged segment's records must fail", n, len(f.src))
	}
}

// placeholdersOf counts the records of c's columnarized segments holding a moved-list placeholder under
// attribute name, and the records holding name's value sealed.
func placeholdersOf(t *testing.T, c *Collection, name string) (moved, sealed int) {
	t.Helper()
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := slices.Clone(sh.segs)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg.colNative.Load() == nil {
				continue
			}
			cn := seg.colNative.Load()
			d := seg.dict.Load()
			for k := 0; k < cn.seg.offsLen(); k++ {
				raw, err := seg.codec.Decompress(nil, recAd(seg.data, cn.seg.offAt(k)))
				if err != nil {
					t.Fatal(err)
				}
				_, ents, n, inline, ok := wire.Ad(raw).SplitBody()
				if !ok {
					t.Fatal("a remnant does not split")
				}
				remnantEntries(ents, n, inline, func(key, node []byte) bool {
					id, nm, _ := remnantKey(key, inline)
					if nm == "" && d != nil {
						nm = string(d.name(id))
					}
					if strings.EqualFold(nm, name) {
						switch {
						case node[0] == wire.TagListRef:
							moved++
						case wire.IsEncryptedNode(node):
							sealed++
						}
					}
					return true
				})
			}
		}
	}
	return moved, sealed
}

// TestListMovesSealedPolicy: a list the policy names is never moved, a list moved while its attribute was
// in the clear is withheld from a reader not entitled to sealed values once the policy is widened to it,
// and the migration puts it back into its records sealed -- it never stays in a column in the clear.
func TestListMovesSealedPolicy(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = []string{"Secret"}
	opts.MoveListColumns = 1
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 400
	tx := c.Begin()
	for i := range n {
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, listSealAd(i)))
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("put conflicted")
	}
	internAll(c)
	if err := c.SetEncryptedAttrs([]string{"Secret", "Early"}); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	if m, _ := placeholdersOf(t, c, "ChildRemoteUser"); m == 0 {
		t.Fatal("fixture: no ChildRemoteUser list moved")
	}
	for _, nm := range []string{"Secret", "Early"} {
		if m, _ := placeholdersOf(t, c, nm); m != 0 {
			t.Fatalf("%d %s lists moved; the policy names it", m, nm)
		}
	}
	if err := c.SetEncryptedAttrs([]string{"Secret", "Early", "ChildRemoteUser"}); err != nil {
		t.Fatal(err)
	}
	for _, proj := range [][]string{nil, {"ChildRemoteUser"}, {"ChildRemoteUser", "ChildCpus"}} {
		for w := range c.ScanRawWire(proj, true) {
			if bytes.Contains(w, []byte("@ap")) {
				t.Fatalf("a redacted read of %v after the widen returned the moved list in the clear", proj)
			}
		}
	}
	if c.MigrateSealedAttrs(0) == 0 {
		t.Fatal("the migration rewrote nothing")
	}
	c.ColumnarizeSealed()
	m, sealed := placeholdersOf(t, c, "ChildRemoteUser")
	if m != 0 {
		t.Fatalf("after the migration %d ChildRemoteUser lists are still only in a column", m)
	}
	if sealed == 0 {
		t.Fatal("after the migration no columnarized record holds ChildRemoteUser sealed")
	}
	if m, _ := placeholdersOf(t, c, "ChildCpus"); m == 0 {
		t.Error("after the migration the clear ChildCpus lists are no longer moved")
	}
	for i := 0; i < n; i += 37 {
		ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i)))
		if !ok {
			t.Fatalf("Get(%d.0) found nothing", i)
		}
		want := fmt.Sprintf(`{ "user%d@ap","user%d@ap" }`, i%7, (i+1)%7)
		if e, _ := ad.Lookup("ChildRemoteUser"); e == nil || strings.ReplaceAll(e.String(), " ", "") != strings.ReplaceAll(want, " ", "") {
			t.Fatalf("record %d: ChildRemoteUser = %v, want %s", i, e, want)
		}
	}
}

// TestListNodeAtBounds: a placeholder resolves only to a present cell of a field and row the region holds;
// an absent or OTHER cell, a field or row past the end, or a truncated region resolves to nothing.
func TestListNodeAtBounds(t *testing.T) {
	rows := []listRow{
		{lists: []listCell{{id: 7, kind: akInt, node: mustListNode(t, "{1, 2}")}}, ids: []uint32{7}},
		{ids: []uint32{3}},
		{ids: []uint32{7}}, // OTHER: holds the attribute, not as a list the column carries
		{lists: []listCell{{id: 7, kind: akNone, node: mustListNode(t, "{}")}}, ids: []uint32{7}},
	}
	ids, _, body := buildBlockLists(rows, func(uint32) (string, bool) { return "L", true }, nil)
	b := &columnarBlock{n: len(rows), listIDs: ids}
	nodes, ok := renderListNodes(b, body)
	if !ok {
		t.Fatal("the body does not render")
	}
	for k, want := range [][]byte{mustListNode(t, "{1, 2}"), nil, nil, mustListNode(t, "{}")} {
		got, ok := listNodeAt(nodes, 0, k)
		if ok != (want != nil) || !bytes.Equal(got, want) {
			t.Errorf("row %d: %x (ok=%v), want %x", k, got, ok, want)
		}
	}
	for _, bad := range []struct {
		j uint64
		k int
	}{{1, 0}, {0, 4}, {0, -1}} {
		if _, ok := listNodeAt(nodes, bad.j, bad.k); ok {
			t.Errorf("field %d row %d resolved", bad.j, bad.k)
		}
	}
	if _, ok := listNodeAt(nodes[:len(nodes)-1], 0, 3); ok {
		t.Error("a cell cut short by a truncated region resolved")
	}
}
