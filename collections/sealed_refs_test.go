package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// An encrypted attribute's value refers to no other attribute: in every real use it is a token or
// a secret string, and the read paths are built on that. The narrowed match and the columnar
// projections treat a sealed value as reference-free when deciding whether a narrowed record can
// stand in for the whole one, and every consumer -- the text projection, the wire-row assembler,
// the partial decode a query is evaluated on -- stops at a sealed value instead of following the
// references inside it. These tests pin that from both sides: an out-of-contract sealed expression
// is read the same documented way on every path, and in-contract sealed values keep the fast paths.

func sealedRefKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	return key
}

// sealedRefAd is ad i of the fixture. Secret is OUT of contract: a sealed expression over Threshold,
// which no projection or query below names. Token is in contract: a sealed string.
func sealedRefAd(i int) string {
	return fmt.Sprintf(`[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; Threshold=%d;`+
		` Secret = (Threshold > 1); Token = "tok%d"; Filler0 = "f%d"; Filler1 = "g%d" ]`, i, i%10, i%3, i, i, i)
}

const sealedRefN = 1200

// sealedRefFixture is a persistent encrypted store holding every stored form a sealed value reaches
// a reader in: columnarized sealed segments (interned first, when interned is set) and an active
// tail of inline records.
func sealedRefFixture(t *testing.T, interned bool) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 15,
		DataKey: sealedRefKey(), EncryptedAttrs: []string{"Secret", "Token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, sealedRefAd(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(0, sealedRefN*3/4)
	if interned {
		if _, err := c.RetrainDict(2000); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	put(sealedRefN*3/4, sealedRefN)

	dicts, active := 0, 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			switch {
			case s == nil || s.used == 0:
			case s == sh.act:
				active++
			case s.dict.Load() != nil:
				dicts++
			}
		}
		sh.mu.RUnlock()
	}
	if columnarSegmentsForTest(c) == 0 || active == 0 || (dicts > 0) != interned {
		t.Fatalf("fixture: %d columnarized, %d active, %d interned segments (interned=%v)",
			columnarSegmentsForTest(c), active, dicts, interned)
	}
	return c
}

// TestSealedValueReferencesAreNotFollowed pins the documented behavior for an OUT-OF-CONTRACT sealed
// value, one that refers to another attribute: no read path follows the reference. A projection of
// Secret ships Secret but not Threshold -- from the text projection, the serial and the parallel
// wire rows, with and without a query -- and a query reading Secret evaluates it without Threshold,
// so it is UNDEFINED, under every combination of the narrowed match and the conjunct rejection.
//
// The point is that the paths AGREE. Each used to make its own choice (the text projection stopped
// at a sealed value, the wire rows and the matcher opened it and followed its references), so the
// rows a caller got depended on which read it made and which segment a record was in.
func TestSealedValueReferencesAreNotFollowed(t *testing.T) {
	for _, interned := range []bool{false, true} {
		t.Run(fmt.Sprintf("interned=%v", interned), func(t *testing.T) {
			c := sealedRefFixture(t, interned)
			proj := []string{"ClusterId", "Secret"}
			all := mustParseQ(t, "ProcId >= 0")
			check := func(label string, rows []map[string]string) {
				t.Helper()
				if len(rows) != sealedRefN {
					t.Fatalf("%s: %d rows, want %d", label, len(rows), sealedRefN)
				}
				for _, m := range rows {
					if _, ok := m["Secret"]; !ok {
						t.Fatalf("%s: the sealed attribute itself is missing: %v", label, sortedKeys(m))
					}
					if _, ok := m["Threshold"]; ok {
						t.Fatalf("%s: the reference inside a sealed value was followed: %v", label, sortedKeys(m))
					}
				}
			}
			wireRows := func(seq func(func([]byte) bool)) []map[string]string {
				var out []map[string]string
				for row := range seq {
					out = append(out, wireRowMap(t, row))
				}
				return out
			}
			for _, chase := range []bool{false, true} {
				var text []map[string]string
				for ra := range c.QueryRawProjected(all, proj, chase, false) {
					text = append(text, rawAdMap(t, ra))
				}
				check(fmt.Sprintf("QueryRawProjected chase=%v", chase), text)
				text = text[:0]
				for ra := range c.ScanRawProjected(proj, chase, false) {
					text = append(text, rawAdMap(t, ra))
				}
				check(fmt.Sprintf("ScanRawProjected chase=%v", chase), text)
			}
			for _, parallel := range []bool{false, true} {
				if parallel {
					c.parallelMinBytes = 0
				} else {
					c.parallelMinBytes = 1 << 62
				}
				check(fmt.Sprintf("QueryRawWireRefs parallel=%v", parallel), wireRows(c.QueryRawWireRefs(all, proj, false)))
				check(fmt.Sprintf("ScanRawWireRefs parallel=%v", parallel), wireRows(c.ScanRawWireRefs(proj, false)))
			}
			c.parallelMinBytes = 1 << 62

			// The whole ad does evaluate Secret through Threshold -- which is exactly why the value is
			// out of contract: a read that loads only what it names cannot see that dependency.
			defined := 0
			for ad := range c.Scan() {
				if v := ad.EvaluateAttr("Secret"); v.IsBool() {
					defined++
				}
			}
			if defined != sealedRefN {
				t.Fatalf("the whole ad evaluates Secret in %d of %d records; the fixture is not what this test is about",
					defined, sealedRefN)
			}
			undef := mustParseQ(t, "Secret =?= undefined")
			for _, arm := range narrowBenchArms {
				restore := arm.set()
				n := 0
				for range c.Query(undef) {
					n++
				}
				var st ScanStats
				rows := wireRows(c.QueryRawWireRefsStats(undef, proj, false, &st))
				restore()
				if n != sealedRefN {
					t.Errorf("[%s] Query: %d of %d records read Secret as undefined", arm.name, n, sealedRefN)
				}
				check(fmt.Sprintf("[%s] QueryRawWireRefs Secret =?= undefined", arm.name), rows)
			}
		})
	}
}

// TestSealedValuesKeepTheFastPaths: a sealed value is reference-free, so a projection or a query
// naming one is served from the columns and the narrowed record like any other. Before the rule
// held everywhere, a sealed attribute made every record "not closed": a wire-row projection naming
// Owner or ClaimId reassembled each record, and a query reading a sealed attribute was never matched
// on the narrowed record.
func TestSealedValuesKeepTheFastPaths(t *testing.T) {
	t.Run("wire rows", func(t *testing.T) {
		c := colPlanMutable(t, true) // Owner sealed by EncryptedAttrs, ClaimId sealed as private
		q := mustParseQ(t, "ProcId < 4")
		for _, proj := range [][]string{{"ClusterId", "Owner"}, {"ClusterId", "Owner", "ClaimId"}} {
			var st ScanStats
			got := map[string]map[string]string{}
			for row := range c.QueryRawWireRefsStats(q, proj, false, &st) {
				m := wireRowMap(t, row)
				got[m["ClusterId"]] = m
			}
			if st.RecordsColumnDecided == 0 {
				t.Fatalf("%v: nothing decided from columns: the fast path is not exercised", proj)
			}
			if st.RecordsClosureReassembled != 0 {
				t.Errorf("%v: %d of %d records fell back to reassembly; a sealed value refers to nothing, "+
					"so every one should have been projected from its columns",
					proj, st.RecordsClosureReassembled, st.RecordsVisited)
			}
			// The rows are the reassembling text projection's, attribute for attribute.
			n := 0
			for ra := range refRawProjected(c, q, proj, true, false, nil) {
				want := rawAdMap(t, ra)
				g := got[want["ClusterId"]]
				if strings.Join(sortedKeys(g), ",") != strings.Join(sortedKeys(want), ",") {
					t.Fatalf("%v: ClusterId %s: wire %v, text %v", proj, want["ClusterId"], sortedKeys(g), sortedKeys(want))
				}
				for k, v := range want {
					if g[k] != v {
						t.Fatalf("%v: ClusterId %s %s: wire %q, text %q", proj, want["ClusterId"], k, g[k], v)
					}
				}
				n++
			}
			if n == 0 || n != len(got) {
				t.Fatalf("%v: %d wire rows, %d text rows", proj, len(got), n)
			}
		}
	})
	t.Run("narrowed match", func(t *testing.T) {
		// Not interned: there the matcher reads a sealed value, so the answer below is a real one.
		c := sealedRefFixture(t, false)
		expr := `Token != "tok7" && ProcId < 4`
		q := mustParseQ(t, expr)
		want := 0
		for ad := range c.Scan() {
			tok, _ := ad.EvaluateAttrString("Token")
			proc, _ := ad.EvaluateAttrInt("ProcId")
			if tok != "tok7" && proc < 4 {
				want++
			}
		}
		restore := narrowBenchArm{"narrow", true, false}.set()
		defer restore()
		var st ScanStats
		got := 0
		for range c.QueryRawWireRefsStats(q, []string{"ClusterId", "Token"}, false, &st) {
			got++
		}
		if got != want || want == 0 {
			t.Fatalf("%s: %d rows, whole-ad evaluation %d", expr, got, want)
		}
		if st.RecordsNarrowMatched == 0 {
			t.Fatalf("%s: no record was matched on its narrowed form (%+v); a sealed string refers to nothing, "+
				"so the narrowed record is a stand-in for the whole one", expr, st)
		}
	})
}

// TestWireRowAssemblerSkipsOpenedSealedValues pins the assembler's half of the rule where the
// stored form no longer shows it: a record converted to inline names before assembly -- an interned
// segment's, or an in-memory collection's -- has its sealed values OPENED by the conversion, so the
// assembler has to remember which entries were sealed or it would follow their references. Driven
// directly, without the id-space narrowing a scan applies first, which would otherwise drop the
// referenced attribute before the assembler could reach it.
func TestWireRowAssemblerSkipsOpenedSealedValues(t *testing.T) {
	ad := mustAd(t, `[ MyType="Job"; ClusterId=7; Threshold=2; Secret = (Threshold > 1); Note="n" ]`)
	proj := []string{"ClusterId", "Secret"}
	check := func(label string, row []byte, ok bool) {
		t.Helper()
		if !ok {
			t.Fatalf("%s: the assembler refused the record", label)
		}
		m := wireRowMap(t, row)
		if _, has := m["Secret"]; !has {
			t.Fatalf("%s: the sealed attribute is missing: %v", label, sortedKeys(m))
		}
		if _, has := m["Threshold"]; has {
			t.Fatalf("%s: the reference inside the opened sealed value was followed: %v", label, sortedKeys(m))
		}
	}

	t.Run("interned segment", func(t *testing.T) {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 1, DataKey: sealedRefKey(), EncryptedAttrs: []string{"Secret"}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		table := wire.NewInternTable()
		w := c.encodeInterned(nil, ad.AST(), table, nil)
		if !recordHasSealedEntry(w) {
			t.Fatal("the encoded record holds no sealed value")
		}
		dict := &segDictHandle{data: appendSegDict(nil, table.Names())}
		sel := c.newWireSubsetSelectorRefs(proj, false, true)
		asm := c.newWireRowAssembler(sel)
		row, ok := asm.appendRowConverted(nil, dict, w, sel.neededCount(), c.renderKey(false))
		check("interned", row, ok)
	})
	t.Run("in-memory", func(t *testing.T) {
		c := New(Options{Shards: 1, DataKey: sealedRefKey(), EncryptedAttrs: []string{"Secret"}})
		defer c.Close()
		w := wire.EncodeWithHotEnc(nil, ad.AST(), c.intern, nil, c.shouldEncrypt, c.sealer)
		if !recordHasSealedEntry(w) {
			t.Fatal("the encoded record holds no sealed value")
		}
		sel := c.newWireSubsetSelectorRefs(proj, false, true)
		asm := c.newWireRowAssembler(sel)
		row, ok := asm.appendRow(nil, wire.Ad(w), sel.neededCount(), c.renderKey(false))
		check("in-memory", row, ok)
	})
}

func recordHasSealedEntry(w []byte) bool {
	found := false
	wire.Ad(w).ForEachRaw(func(_ uint32, _ string, node []byte) bool {
		found = found || wire.IsEncryptedNode(node)
		return true
	})
	return found
}
