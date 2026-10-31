package collections

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// preFixTranscode is wire.AppendInternedFromInline as an earlier build ran it: a sealed value is
// copied into the interned record unopened, so its plaintext stays in the inline-names form.
//
// It is built from today's transcode, which refuses sealed values: each sealed node is swapped for a
// string literal whose node encoding has exactly its length, the record is transcoded, and the sealed
// bytes are put back where the placeholder landed. Equal lengths keep every hot-header offset right. As
// the old transcode did, it keeps a sealed attribute out of the hot header.
func preFixTranscode(dst []byte, src wire.Ad, t *wire.InternTable, hot map[uint32]struct{}) ([]byte, bool) {
	type swap struct{ placeholder, sealed []byte }
	var swaps []swap
	sealedNames := map[string]bool{}
	src.ForEachNamed(nil, func(name string, node []byte) bool {
		if wire.IsEncryptedNode(node) {
			sealedNames[name] = true
			swaps = append(swaps, swap{sealed: append([]byte(nil), node...)})
		}
		return true
	})
	if len(swaps) == 0 {
		return wire.AppendInternedFromInline(dst, src, t, hot)
	}
	ad, err := wire.DecodeInlineRedact(src) // sealed values come back undefined; replaced below
	if err != nil {
		return dst, false
	}
	i := 0
	for _, a := range ad.Attributes {
		if !sealedNames[a.Name] {
			continue
		}
		lit, node, ok := placeholderOfLen(i, len(swaps[i].sealed))
		if !ok {
			return dst, false
		}
		a.Value = lit
		swaps[i].placeholder = node
		i++
	}
	noSealed := make(map[uint32]struct{}, len(hot))
	for id := range hot {
		if name, ok := t.Name(id); !ok || !sealedNames[name] {
			noSealed[id] = struct{}{}
		}
	}
	out, ok := wire.AppendInternedFromInline(dst, wire.Ad(wire.EncodeInline(nil, ad)), t, noSealed)
	if !ok {
		return dst, false
	}
	for _, s := range swaps {
		at := bytes.Index(out, s.placeholder)
		if at < 0 {
			return dst, false
		}
		copy(out[at:], s.sealed)
	}
	return out, true
}

// placeholderOfLen returns a string literal, distinct per i, whose node encoding is n bytes long.
func placeholderOfLen(i, n int) (ast.Expr, []byte, bool) {
	for body := n - 1; body > 0; body-- {
		s := fmt.Sprintf("\x01placeholder-%d-", i)
		if len(s) > body {
			return nil, nil, false
		}
		s += strings.Repeat("#", body-len(s))
		lit := &ast.StringLiteral{Value: s}
		enc := wire.EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "P", Value: lit}}})
		node, _ := wire.Ad(enc).LookupByName("P")
		if len(node) == n {
			return lit, append([]byte(nil), node...), true
		}
	}
	return nil, nil, false
}

// TestSealedInlineFormRecovery reproduces records an earlier build's compaction wrote -- an interned
// segment whose sealed record literals were copied unopened, plaintext still in the inline-names form --
// and shows they are read correctly on every read path, that compaction of the shard completes instead
// of aborting on them, and that it re-seals them in the interned form so they stop needing recovery.
//
// Not parallel: it swaps the package's compaction transcode.
func TestSealedInlineFormRecovery(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	_, dataKey := deriveDataKey(t)
	dir := t.TempDir()
	opts := Options{
		Shards: 1, Dir: dir, SegmentSize: 1 << 16,
		DataKey:        dataKey,
		EncryptedAttrs: []string{"Secret"},
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const n = 40
	for i := 0; i < n; i++ {
		if err = c.Put([]byte(fmt.Sprintf("k%03d", i)),
			mustAd(t, fmt.Sprintf(`[Id = %d; Val = %d; Secret = [a = %d; b = "x"]]`, i, i*10, i))); err != nil {
			t.Fatal(err)
		}
	}

	compactAll := func() {
		for _, sh := range c.shards {
			c.compactShard(sh, c.currentCodec())
		}
		c.reindexAfterCompaction()
	}
	compactTranscode = preFixTranscode
	compactAll()
	compactTranscode = wire.AppendInternedFromInline
	interned := c.internedSegments()
	if len(interned) == 0 {
		t.Fatal("compaction interned no segment; the test never reached the interned read path")
	}

	before := SealedInlineFormRecoveries()
	c.checkSealedRecords(t, "damaged", n)
	if SealedInlineFormRecoveries() == before {
		t.Fatal("no read recovered a sealed value: the records were not written in the damaged form")
	}

	// The shard compacts: every damaged segment is retired, not left in place by an abort.
	compactAll()
	for seg := range c.internedSegments() {
		if interned[seg] {
			t.Fatal("compaction left a damaged segment in place (aborted on its records)")
		}
	}
	healed := SealedInlineFormRecoveries()
	c.checkSealedRecords(t, "after compaction", n)
	if got := SealedInlineFormRecoveries() - healed; got != 0 {
		t.Errorf("after compaction, %d sealed values still needed recovery; want them re-sealed interned", got)
	}

	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(opts); err != nil {
		t.Fatal(err)
	}
	c.checkSealedRecords(t, "after reopen", n)
	if got := SealedInlineFormRecoveries() - healed; got != 0 {
		t.Errorf("after reopen, %d sealed values needed recovery", got)
	}
}

// internedSegments returns the collection's segments that carry an attribute dictionary.
func (c *Collection) internedSegments() map[*segment]bool {
	out := map[*segment]bool{}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.dict.Load() != nil {
				out[seg] = true
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

// checkSealedRecords reads every record of TestSealedInlineFormRecovery's store through Get, Query,
// Scan, a projected text read and a wire-row read, and checks each returns every record with
// Secret.a equal to its Id.
func (c *Collection) checkSealedRecords(t *testing.T, stage string, n int) {
	t.Helper()
	secretA, err := classad.ParseExpr("Secret.a")
	if err != nil {
		t.Fatal(err)
	}
	secretOK := func(path string, a *classad.ClassAd) {
		t.Helper()
		id, _ := a.EvaluateAttrInt("Id")
		v := secretA.Eval(a)
		if got, ierr := v.IntValue(); ierr != nil || got != id {
			t.Errorf("%s: %s: Secret.a = %v want %d", stage, path, v, id)
		}
	}
	missing := 0
	for i := 0; i < n; i++ {
		if a, ok := c.Get([]byte(fmt.Sprintf("k%03d", i))); ok {
			secretOK("Get", a)
		} else {
			missing++
		}
	}
	queried := 0
	for a := range c.Query(mustQuery(t, `Val >= 200 && Secret.b == "x"`)) {
		secretOK("Query", a)
		queried++
	}
	scanned := 0
	for a := range c.Scan() {
		secretOK("Scan", a)
		scanned++
	}
	// The text and wire-row paths render the record; parse it back to check the value.
	fromExprs := func(path string, exprs [][]byte) {
		t.Helper()
		a, perr := classad.Parse("[" + string(bytes.Join(exprs, []byte(";"))) + "]")
		if perr != nil {
			t.Errorf("%s: %s: rendered ad does not parse: %v", stage, path, perr)
			return
		}
		secretOK(path, a)
	}
	projected := 0
	for ra := range c.ScanRawProjected([]string{"Id", "Secret"}, false, false) {
		fromExprs("ScanRawProjected", ra.Exprs)
		projected++
	}
	rows := 0
	for row := range c.ScanRawWire([]string{"Id", "Secret"}, false) {
		buf, offs, _, _, ok := RenderRawAdInline(row, nil, nil)
		if !ok {
			t.Errorf("%s: ScanRawWire: row does not render", stage)
			continue
		}
		exprs := make([][]byte, 0, len(offs))
		for i := 0; i+1 < len(offs); i++ {
			exprs = append(exprs, buf[offs[i]:offs[i+1]])
		}
		fromExprs("ScanRawWire", exprs)
		rows++
	}
	if missing != 0 || queried != n/2 || scanned != n || projected != n || rows != n {
		t.Fatalf("%s: Get missed %d of %d, Query matched %d of %d, Scan returned %d, ScanRawProjected %d, ScanRawWire %d (want %d)",
			stage, missing, n, queried, n/2, scanned, projected, rows, n)
	}
}
