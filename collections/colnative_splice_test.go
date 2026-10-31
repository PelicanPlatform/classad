package collections

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// spliceFixtureAd is ad i of a store whose columnar rows carry cold tails: Rare* attributes sit below
// the schema's presence floor, and every seventh ClusterId is a string where the schema holds an int,
// so it escapes its slot. ProcExpr is an expression and stays in the record.
func spliceFixtureAd(i int) string {
	cid := fmt.Sprint(i)
	if i%7 == 3 {
		cid = fmt.Sprintf(`"c%d"`, i)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `[ ClusterId = %s; ProcId = %d; Owner = "user%d"; JobStatus = %d; RequestMemory = %d; ProcExpr = ProcId * 2`,
		cid, i%10, i%7, 1+i%5, (1+i%8)*512)
	if i%5 == 0 {
		fmt.Fprintf(&b, `; Rare%d = %d; RareStr = "r%d"`, i%3, i, i)
	}
	b.WriteString(" ]")
	return b.String()
}

// spliceFixture builds a persistent store whose sealed segments are interned and then columnarized,
// and returns it with its columnarized, dictionary-keyed segments.
func spliceFixture(t *testing.T) (*Collection, []*segment) {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 15, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := 0; i < 2000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("%05d.0", i)), mustAd(t, spliceFixtureAd(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(4000); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	var segs []*segment
	sh := c.shards[0]
	sh.mu.RLock()
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.columnarized() && s.dict.Load() != nil {
			segs = append(segs, s)
		}
	}
	sh.mu.RUnlock()
	if len(segs) == 0 {
		t.Fatal("no interned segment was columnarized; the fixture covers nothing")
	}
	return c, segs
}

// attrSet renders an ad as its sorted "name = value" lines, names folded, so two ads holding the same
// attributes in different orders compare equal.
func attrSet(ad *ast.ClassAd) string {
	lines := make([]string, 0, len(ad.Attributes))
	for _, a := range ad.Attributes {
		lines = append(lines, strings.ToLower(a.Name)+" = "+a.Value.String())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestSpliceRefusesMalformedColdTail: a columnar row whose cold tail does not parse to its end must
// fail the read, counted, rather than return the attributes that preceded the damage as the whole ad.
// The splice used to ignore the walk's failure and build the ad from whatever it had collected.
func TestSpliceRefusesMalformedColdTail(t *testing.T) {
	c, segs := spliceFixture(t)
	want := readAll(t, c, "true")

	// The last row of a block whose cold tail is non-empty: ending its tail one byte early cuts its last
	// node short, and no row follows it in the block to be shifted by the same edit.
	var seg *segment
	var key string
	var off uint32
	for _, s := range segs {
		cn := s.colNative.Load()
		base := 0
		for bi, blk := range cn.seg.blocks {
			last := blk.n - 1
			if blk.n > 0 && blk.coldOffAt(last+1) > blk.coldOffAt(last) {
				nb := *blk
				nb.coldOffB = slices.Clone(blk.coldOffB)
				binary.LittleEndian.PutUint32(nb.coldOffB[(last+1)*4:], uint32(blk.coldOffAt(last+1)-1))
				cs := *cn.seg
				cs.blocks = slices.Clone(cn.seg.blocks)
				cs.blocks[bi] = &nb
				moved := *cn
				moved.seg = &cs
				s.colNative.Store(&moved)
				seg, off = s, cn.seg.offAt(base+last)
				key = string(recKey(s.data, off))
				break
			}
			base += blk.n
		}
		if seg != nil {
			break
		}
	}
	if seg == nil {
		t.Fatal("no block ends in a row with a cold tail; the test proves nothing")
	}

	rows, skips := ColNativeBadRows(), readSkipsOf("reassemble")
	if _, err := c.recordWire(seg, off, nil); !errors.Is(err, errBadRemnant) {
		t.Errorf("recordWire = %v, want errBadRemnant", err)
	}
	if ad, ok := c.Get([]byte(key)); ok {
		t.Errorf("Get(%s) returned an ad from a row that does not parse: %s", key, ad.String())
	}
	got := readAll(t, c, "true")
	if len(got) != len(want)-1 {
		t.Errorf("a scan returned %d ads, want %d (every ad but the damaged one)", len(got), len(want)-1)
	}
	left := map[string]int{}
	for _, w := range want {
		left[w]++
	}
	for _, a := range got {
		if left[a] == 0 {
			t.Errorf("a scan returned an ad that was not written: %q", a)
			continue
		}
		left[a]--
	}
	if ColNativeBadRows()-rows < 3 {
		t.Errorf("ColNativeBadRows rose by %d over three refused reads", ColNativeBadRows()-rows)
	}
	if readSkipsOf("reassemble")-skips < 2 {
		t.Errorf("RecordReadSkips[reassemble] rose by %d over a refused Get and scan", readSkipsOf("reassemble")-skips)
	}
}

// TestSpliceNamesInlineRecordInInternedSegment: an inline-keyed remnant in an interned segment is
// reassembled with every attribute under its own name. The splice used to write the schema fields of
// such a record with an empty name, and to name its cold-tail entries through this process's intern
// table although the payload keys them by the segment's dictionary.
//
// The remnant is the interned record re-encoded inline, which is the shape a materialized delta has:
// the merge of a chain is written inline whatever segment it lands in.
func TestSpliceNamesInlineRecordInInternedSegment(t *testing.T) {
	t.Parallel()
	c, segs := spliceFixture(t)
	checked, withCold := 0, 0
	for _, s := range segs {
		cn := s.colNative.Load()
		d := s.dict.Load()
		for k := 0; k < cn.seg.offsLen(); k++ {
			off := cn.seg.offAt(k)
			full, err := c.recordWire(s, off, nil)
			if err != nil {
				t.Fatalf("record %d: %v", k, err)
			}
			orig, err := wire.DecodeResolve(full, d.resolve)
			if err != nil {
				t.Fatalf("record %d: %v", k, err)
			}
			raw, err := s.codec.Decompress(nil, recAd(s.data, off))
			if err != nil {
				t.Fatal(err)
			}
			// The remnant as the readers see it: with any remnant dictionary reference expanded.
			blk, _ := cn.blockFor(k)
			if raw, _, err = blk.expandRemnant(raw, cn.cache, nil); err != nil {
				t.Fatal(err)
			}
			rem, err := wire.DecodeResolve(raw, d.resolve)
			if err != nil {
				t.Fatal(err)
			}
			spliced, err := cn.spliceInto(c, wire.EncodeInline(nil, rem), k, nil)
			if err != nil {
				t.Fatalf("record %d: splicing the inline remnant: %v", k, err)
			}
			got, err := wire.DecodeInline(spliced)
			if err != nil {
				t.Fatalf("record %d: the spliced ad does not decode: %v", k, err)
			}
			if g, w := attrSet(got), attrSet(orig); g != w {
				t.Fatalf("record %d reassembled from an inline remnant as\n%s\nwant\n%s", k, g, w)
			}
			checked++
			if blk, local := cn.blockFor(k); blk.coldOffAt(local+1) > blk.coldOffAt(local) {
				withCold++
			}
		}
	}
	if checked == 0 || withCold == 0 {
		t.Fatalf("checked %d records, %d with a cold tail: the cold-tail naming is not covered", checked, withCold)
	}
	t.Logf("%d records reassembled from inline remnants, %d with a cold tail", checked, withCold)
}

// TestColumnarizeRefusesRecordItCannotCarry: the rewrite carries forward only the records its build
// kept, so a record the build passes over must refuse the rewrite rather than vanish from the table.
// An inline-keyed record in an interned segment is one: the build canonicalizes records through the
// segment's dictionary and cannot read an inline one that way. It reaches the build here as a delta
// plan's materialized ad, which is how such a record would.
func TestColumnarizeRefusesRecordItCannotCarry(t *testing.T) {
	t.Parallel()
	c, segs := spliceFixture(t)
	src := segs[0]
	cn := src.colNative.Load()
	off := cn.seg.offAt(cn.seg.offsLen() / 2)
	full, err := c.recordWire(src, off, nil)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := wire.DecodeResolve(full, src.dict.Load().resolve)
	if err != nil {
		t.Fatal(err)
	}
	plan := &deltaPlan{drop: map[uint32]struct{}{}, full: map[uint32][]byte{off: wire.EncodeInline(nil, ad)}}

	st := c.schemaScan.Load()
	c.maintMu.Lock()
	dst, srcOffs, _ := c.columnarizeSegment(c.shards[0], src, st.schema, st.hot, plan)
	c.maintMu.Unlock()
	if dst != nil {
		dst.retire()
		dst.reapAndHook()
		t.Fatalf("the rewrite went ahead carrying %d of %d records, without the one at offset %d",
			len(srcOffs), cn.seg.offsLen(), off)
	}
}

// TestRecordRewritersRefuseTruncatedRecords: the helpers that rebuild a record from a subset of its
// entries -- the narrowed read's narrowAd and the columnar rewrite's stripSchemaAttrs and keepAttrs --
// must refuse a record whose entries stop short rather than build an ad from the ones before the
// damage. A full read of the same bytes fails on decode; a narrowed one that succeeded would answer
// from a different ad.
func TestRecordRewritersRefuseTruncatedRecords(t *testing.T) {
	ad := mustAd(t, `[ A = 1; B = "two"; C = 3.5; D = A + 1 ]`)
	whole := wire.EncodeInline(nil, ad.AST())
	short := whole[:len(whole)-1] // D's node loses its last byte
	all := func(uint32, string, []byte) bool { return true }
	none := func(uint32, string, []byte) bool { return false }

	var sc narrowScratch
	if _, ok := narrowAd(&sc, whole, func(uint32, string) bool { return true }); !ok {
		t.Fatal("narrowAd refused a well-formed record")
	}
	if out, ok := narrowAd(&sc, short, func(uint32, string) bool { return true }); ok {
		t.Errorf("narrowAd accepted a truncated record and returned %d entries", wire.Ad(out).AttrCount())
	}
	if _, ok := stripSchemaAttrs(whole, none); !ok {
		t.Fatal("stripSchemaAttrs refused a well-formed record")
	}
	if out, ok := stripSchemaAttrs(short, none); ok {
		t.Errorf("stripSchemaAttrs accepted a truncated record and returned %d entries", wire.Ad(out).AttrCount())
	}
	if _, ok := keepAttrs(whole, all); !ok {
		t.Fatal("keepAttrs refused a well-formed record")
	}
	if out, ok := keepAttrs(short, all); ok {
		t.Errorf("keepAttrs accepted a truncated record and returned %d entries", wire.Ad(out).AttrCount())
	}
}
