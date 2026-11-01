package collections

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Every write path refuses an ad nesting deeper than wire.MaxDepth before it changes anything, so
// the store never holds a record its readers refuse. Values at the limit store and read back.

// limitTerms is the longest `||` chain an attribute may hold (n terms reach depth n-1); overTerms is
// one term too many.
const (
	limitTerms = wire.MaxDepth + 1
	overTerms  = limitTerms + 1
)

// chainText is the n-term expression `a0 || a1 || ...`, which reaches depth n-1.
func chainText(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString(" || ")
		}
		fmt.Fprintf(&b, "a%d", i)
	}
	return b.String()
}

// chainAd is an ad whose Requirements is an n-term chain, plus a scalar.
func chainAd(t testing.TB, n int) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse("[Id = 1; Requirements = " + chainText(n) + "]")
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// chainOld is chainAd in old-ClassAd text, for the wire-native ingest paths.
func chainOld(n int) string {
	return "Id = 1\nRequirements = " + chainText(n) + "\n"
}

// mustTooDeep fails unless err is a refusal naming Requirements, an n-term chain.
func mustTooDeep(t *testing.T, what string, err error, n int) {
	t.Helper()
	if !errors.Is(err, ErrTooDeep) || !errors.Is(err, wire.ErrTooDeep) {
		t.Fatalf("%s: err = %v, want ErrTooDeep", what, err)
	}
	var de *wire.DepthError
	if !errors.As(err, &de) || de.Attr != "Requirements" || de.Depth != n-1 {
		t.Fatalf("%s: err = %v, want Requirements at depth %d", what, err, n-1)
	}
}

// mustHoldChain fails unless key reads back holding an n-term Requirements, through both the
// object and the raw (rendered) read paths.
func mustHoldChain(t *testing.T, c *Collection, key string, n int) {
	t.Helper()
	got, ok := c.Get([]byte(key))
	if !ok {
		t.Fatalf("%s: does not read back", key)
	}
	e, ok := got.Lookup("Requirements")
	if !ok || e.String() != chainAd(t, n).AST().Attributes[1].Value.String() {
		t.Fatalf("%s: Requirements read back changed", key)
	}
	want := fmt.Sprintf(" || a%d)", n-1)
	found := false
	for raw := range c.ScanRaw() {
		for _, x := range raw.Exprs {
			if bytes.HasPrefix(x, []byte("Requirements = ")) && bytes.HasSuffix(x, []byte(want)) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("%s: the raw scan did not render the %d-term Requirements", key, n)
	}
}

// depthStores is every kind of collection the write paths differ across.
func depthStores(t *testing.T) map[string]func() *Collection {
	stores := map[string]func() *Collection{
		"memory": func() *Collection { return New(Options{Shards: 2}) },
	}
	if !mmapSupported {
		return stores
	}
	open := func(o Options) func() *Collection {
		return func() *Collection {
			o.Dir = t.TempDir()
			c, err := Open(o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
	}
	stores["persistent"] = open(Options{Shards: 2})
	stores["delta"] = open(Options{Shards: 2, DeltaMax: 4})
	// Requirements sealed at rest: whether a value is encrypted must not change whether it fits.
	stores["encrypted"] = open(Options{Shards: 2, DataKey: bytes.Repeat([]byte{7}, 32), EncryptedAttrs: []string{"Requirements"}})
	return stores
}

// TestPutRefusesTooDeep: Put, Update and UpdateOld store a value at the limit and refuse one a level
// past it, leaving the key -- and every other ad of a batch -- exactly as it was.
func TestPutRefusesTooDeep(t *testing.T) {
	for name, mk := range depthStores(t) {
		t.Run(name, func(t *testing.T) {
			c := mk()
			if err := c.Put([]byte("k"), chainAd(t, limitTerms)); err != nil {
				t.Fatalf("Put at the limit: %v", err)
			}
			mustHoldChain(t, c, "k", limitTerms)

			mustTooDeep(t, "Put over an existing key", c.Put([]byte("k"), chainAd(t, overTerms)), overTerms)
			mustHoldChain(t, c, "k", limitTerms)
			mustTooDeep(t, "Put of a new key", c.Put([]byte("new"), chainAd(t, 5000)), 5000)
			if _, ok := c.Get([]byte("new")); ok || c.Len() != 1 {
				t.Fatalf("a refused Put left a trace: present=%v, Len=%d", ok, c.Len())
			}

			err := c.Update([]AdUpdate{{Key: []byte("u1"), Ad: chainAd(t, 3)}, {Key: []byte("u2"), Ad: chainAd(t, overTerms)}})
			mustTooDeep(t, "Update", err, overTerms)
			if _, ok := c.Get([]byte("u1")); ok {
				t.Fatal("a refused Update applied the batch's other ad")
			}

			err = c.UpdateOld([]OldAdUpdate{{Key: []byte("o1"), Text: chainOld(3)}, {Key: []byte("o2"), Text: chainOld(overTerms)}})
			mustTooDeep(t, "UpdateOld", err, overTerms)
			if _, ok := c.Get([]byte("o1")); ok || c.Len() != 1 {
				t.Fatalf("a refused UpdateOld left a trace: o1 present=%v, Len=%d", ok, c.Len())
			}
			if err := c.UpdateOld([]OldAdUpdate{{Key: []byte("o3"), Text: chainOld(limitTerms)}}); err != nil {
				t.Fatalf("UpdateOld at the limit: %v", err)
			}
			mustHoldChain(t, c, "o3", limitTerms)
		})
	}
}

// TestTxnRefusesTooDeep: in a multi-op transaction a too-deep write is reported Unapplied with its
// error, nothing of it -- nor of an earlier write to the same key in the transaction -- is stored, and
// the other writes commit. A later write to a refused key behaves as if the refusal never happened
// (the delta tracker did not count a record that was never written).
func TestTxnRefusesTooDeep(t *testing.T) {
	for name, mk := range depthStores(t) {
		t.Run(name, func(t *testing.T) {
			c := mk()
			if err := c.Put([]byte("base"), chainAd(t, 3)); err != nil {
				t.Fatal(err)
			}
			deepPatch := classad.New()
			deepPatch.Insert("Requirements", chainAd(t, overTerms).AST().Attributes[1].Value)
			tx := c.Begin()
			tx.Put([]byte("ok"), chainAd(t, limitTerms))
			tx.Put([]byte("twice"), chainAd(t, 3))
			tx.Put([]byte("twice"), chainAd(t, overTerms))
			tx.Put([]byte("fresh"), chainAd(t, overTerms))
			tx.PatchAttrs([]byte("base"), deepPatch, nil)
			tx.PutPatch([]byte("patched"), chainAd(t, overTerms), []string{"Requirements"}, false)
			res := tx.Commit()
			if res.Committed != 1 || res.Conflicted() {
				t.Fatalf("Committed = %d, conflicts %q; want only ok", res.Committed, res.Conflicts)
			}
			if len(res.Unapplied) != 4 || len(res.UnappliedReasons) != 4 || len(res.UnappliedErrs) != 4 {
				t.Fatalf("Unapplied %q / %q / %v, want four parallel entries", res.Unapplied, res.UnappliedReasons, res.UnappliedErrs)
			}
			for i, k := range res.Unapplied {
				mustTooDeep(t, string(k), res.UnappliedErrs[i], overTerms)
				if !strings.HasPrefix(res.UnappliedReasons[i], "too-deep: ") || !strings.Contains(res.UnappliedReasons[i], "Requirements") {
					t.Fatalf("%s: reason %q does not say why", k, res.UnappliedReasons[i])
				}
			}
			mustHoldChain(t, c, "ok", limitTerms)
			for _, k := range []string{"twice", "fresh", "patched"} {
				if _, ok := c.Get([]byte(k)); ok {
					t.Fatalf("refused key %s is present", k)
				}
			}
			got, _ := c.Get([]byte("base"))
			if e, _ := got.Lookup("Requirements"); e.String() != chainAd(t, 3).AST().Attributes[1].Value.String() {
				t.Fatalf("base was changed by a refused patch: %v", e)
			}

			// The refused keys take ordinary writes afterwards: a patch onto the refused new key
			// creates it, and a patch onto base deltas over the base that is really there.
			for _, k := range []string{"fresh", "base"} {
				p := classad.New()
				p.InsertAttr("X", 1)
				ptx := c.Begin()
				ptx.PatchAttrs([]byte(k), p, nil)
				if r := ptx.Commit(); r.Committed != 1 || r.HasUnapplied() || r.Conflicted() {
					t.Fatalf("%s: follow-up patch: %+v", k, r)
				}
				got, ok := c.Get([]byte(k))
				if !ok {
					t.Fatalf("%s: does not read after the follow-up patch", k)
				}
				if x, _ := got.EvaluateAttrInt("X"); x != 1 {
					t.Fatalf("%s: follow-up patch lost: %v", k, got)
				}
			}

			// PutOld declines a too-deep ad (the caller falls back to parsing it and Put, which
			// Commit refuses as above) and takes one at the limit.
			tx = c.Begin()
			if tx.PutOld([]byte("old"), chainOld(overTerms)) {
				t.Fatal("PutOld took a too-deep ad")
			}
			if !tx.PutOld([]byte("old"), chainOld(limitTerms)) {
				t.Fatal("PutOld declined an ad at the limit")
			}
			if r := tx.Commit(); r.Committed != 1 {
				t.Fatalf("PutOld commit: %+v", r)
			}
			mustHoldChain(t, c, "old", limitTerms)
		})
	}
}

// TestArchiveAppendRefusesTooDeep: an append-only archive refuses a too-deep ad and keeps appending.
func TestArchiveAppendRefusesTooDeep(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a, err := OpenArchive(ArchiveOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	mustTooDeep(t, "Append", a.Append(chainAd(t, overTerms)), overTerms)
	mustTooDeep(t, "AppendDeferred", a.AppendDeferred(chainAd(t, overTerms)), overTerms)
	if aerr := a.Append(chainAd(t, limitTerms)); aerr != nil {
		t.Fatalf("Append at the limit: %v", aerr)
	}
	all, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range a.Query(all) {
		n++
	}
	if n != 1 {
		t.Fatalf("archive holds %d ads, want 1", n)
	}
}

// TestTooDeepLegacyRecord pins what happens to a record written before the write side refused depth:
// it stays unreadable (the readers' bound is unchanged), Fsck -- which checks framing and checksums,
// not values -- finds nothing wrong with it, and compaction copies it forward unchanged rather than
// dropping it, reporting it once.
func TestTooDeepLegacyRecord(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	reports := captureUndecodable(t)
	for i := range 200 {
		if perr := c.Put(fmt.Appendf(nil, "k%03d", i), chainAd(t, 2)); perr != nil {
			t.Fatal(perr)
		}
	}
	legacy := wire.EncodeInline(nil, chainAd(t, overTerms).AST()) // what the encoder wrote before
	putRawWire(c, []byte("legacy"), legacy)
	for i := range 100 {
		if perr := c.Put(fmt.Appendf(nil, "k%03d", i), chainAd(t, 3)); perr != nil { // something to reclaim
			t.Fatal(perr)
		}
	}
	if _, ok := c.Get([]byte("legacy")); ok {
		t.Fatal("setup: the legacy record reads")
	}
	rep, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, runs, bad := rep.Totals(); runs != 0 || bad != 0 {
		t.Fatalf("Fsck reported damage: %s", rep)
	}
	before := CompactPreservedUndecodable()
	c.compactShard(c.shards[0], c.currentCodec())
	c.reindexAfterCompaction()
	if got := CompactPreservedUndecodable() - before; got != 1 || len(*reports) != 1 || (*reports)[0].Key != "legacy" {
		t.Fatalf("compaction preserved %d, reported %+v; want the legacy record once", got, *reports)
	}
	if _, w := preservedRecord(t, c, []byte("legacy")); !bytes.Equal(w, legacy) {
		t.Fatal("compaction changed the legacy record's bytes")
	}
	if _, ok := c.Get([]byte("legacy")); ok {
		t.Fatal("the legacy record reads after compaction")
	}
}
