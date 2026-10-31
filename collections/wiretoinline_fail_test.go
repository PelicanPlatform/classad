package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestWireToInlineUndecodableRecordIsSkipped: an interned record that its segment dictionary
// cannot decode -- here an attribute id the dictionary does not hold -- must be skipped by every
// reader, never handed on in its segment-id-keyed form. Those bytes still parse as an ad, but a
// reader expecting inline names or global ids names each id through the wrong table: a raw scan
// emitted the record's values under the global table's names (Requirements = 7, Rank = "alice")
// and silently dropped Val, whose id named nothing there.
func TestWireToInlineUndecodableRecordIsSkipped(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Give the global table names of its own, so a segment id misread through it lands on one.
	if err := c.Put([]byte("other"), mustAd(t, `[Requirements = true; Rank = 1; Memory = 2]`)); err != nil {
		t.Fatal(err)
	}

	table := wire.NewInternTable()
	rec := wire.Encode(nil, mustAd(t, `[Id = 7; Owner = "alice"; Val = 42]`).AST(), table)
	// The dictionary lacks the record's last id, so the record does not decode against it.
	dict := &segDictHandle{data: appendSegDict(nil, table.Names()[:2])}
	if _, err := c.decodeWireDict(dict, rec); err == nil {
		t.Fatal("setup: the record decodes against the short dictionary")
	}
	proj := []string{"Requirements", "Rank", "Memory", "Id", "Owner", "Val"}

	collect := func(rows *[]string) func(RawAd) bool {
		return func(ra RawAd) bool {
			for _, e := range ra.Exprs {
				*rows = append(*rows, string(e))
			}
			*rows = append(*rows, "|")
			return true
		}
	}
	skipped := func(t *testing.T, run func()) {
		t.Helper()
		before := readSkips[skipDecode].Load()
		run()
		// At least: the counter is process-wide, and another test may skip a record meanwhile.
		if got := readSkips[skipDecode].Load() - before; got < 1 {
			t.Errorf("decode read skips = %d, want the skipped record counted", got)
		}
	}

	t.Run("raw scan", func(t *testing.T) {
		var rows []string
		skipped(t, func() { c.yieldRaw(collect(&rows), false)(append([]byte(nil), rec...), dict) })
		if len(rows) != 0 {
			t.Errorf("emitted the undecodable record: %q", rows)
		}
	})
	t.Run("redacted projection", func(t *testing.T) {
		var rows []string
		p := c.newRawProjector(proj, false, true)
		skipped(t, func() { c.yieldRawProjected(collect(&rows), p)(append([]byte(nil), rec...), dict) })
		if len(rows) != 0 {
			t.Errorf("emitted the undecodable record: %q", rows)
		}
	})
	t.Run("wire row", func(t *testing.T) {
		sel := c.newWireSubsetSelector(proj, false)
		asm := c.newWireRowAssembler(sel)
		var row []byte
		var ok bool
		skipped(t, func() {
			row, ok = asm.appendRowConverted(nil, dict, append([]byte(nil), rec...), sel.neededCount(), c.renderKey(false))
		})
		if ok {
			t.Errorf("assembled a row from the undecodable record: %v", sortedKeys(wireRowMap(t, row)))
		}
	})
}
