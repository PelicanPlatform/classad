package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A delta chain's participants do not all share one encoding. Records are written inline, each
// carrying its own attribute names, but COMPACTION re-encodes what it rewrites as interned
// against a per-segment dictionary. Once a key's base has been through a compaction while its
// deltas are still inline, the chain spans both forms.
//
// Decoding the whole chain one way fails on the other, and production said so in the decoder's
// own words -- "DecodeInlineEnc requires an inline-names ad", on the base -- while refusing
// every subsequent write to every key whose base had been compacted, around 90 a minute.
func TestChainDecodesAcrossACompactedBase(t *testing.T) {
	c, n := compactedBaseFixture(t)
	key := func(i int) []byte { return []byte(fmt.Sprintf("%d.0", i)) }

	// Now patch, so each key's current record is an inline delta on top of an interned base.
	for i := range n {
		p := classad.New()
		p.InsertAttr("JobStatus", int64(4))
		tx := c.Begin()
		tx.PatchAttrs(key(i), p, nil)
		if r := tx.Commit(); r.Conflicted() || r.HasUnapplied() {
			t.Fatalf("patch %d did not land: unapplied=%v", i, r.HasUnapplied())
		}
	}

	// Every key must still read, with the base's attributes AND the patch applied.
	for i := range n {
		ad, ok := c.Get(key(i))
		if !ok {
			t.Fatalf("%s is unreadable after its base was compacted; reasons = %v",
				key(i), UnreadableBaseReasons())
		}
		if v, ok := ad.EvaluateAttrInt("JobStatus"); !ok || v != 4 {
			t.Fatalf("%s JobStatus = %d (present %v), want 4", key(i), v, ok)
		}
		if v, ok := ad.EvaluateAttrInt("ClusterId"); !ok || v != int64(i) {
			t.Fatalf("%s lost its base attributes: ClusterId = %d (present %v)", key(i), v, ok)
		}
		if v, ok := ad.EvaluateAttrString("Owner"); !ok || v != fmt.Sprintf("user%d", i%7) {
			t.Fatalf("%s Owner = %q (present %v): the interned base resolved wrong", key(i), v, ok)
		}
	}

	// And a further write on top must not be refused.
	before := UnreadableBaseRefusals()
	for i := range n {
		res := patchTx(t, c, fmt.Sprintf("%d.0", i), "CompletionDate", 1789838852, "Pad00")
		if res.HasUnapplied() {
			t.Fatalf("write to %s refused; reasons = %v", key(i), UnreadableBaseReasons())
		}
	}
	if got := UnreadableBaseRefusals() - before; got != 0 {
		t.Errorf("%d refusals while writing over compacted bases", got)
	}
}

// The wire path is served differently from a point read: it tries to splice participant BYTES
// first and only decodes when that refuses. AppendAdMergedInline does refuse an interned
// participant, so a mixed-encoding chain lands in the decode fallback -- which is exactly the
// code this fix repairs. Covered separately because the point-read test above exercises the
// object path and would not notice the wire path regressing.
func TestWirePathDoesNotSpliceAMixedChain(t *testing.T) {
	c, n := compactedBaseFixture(t)
	key := func(i int) []byte { return []byte(fmt.Sprintf("%d.0", i)) }

	for i := range n {
		p := classad.New()
		p.InsertAttr("JobStatus", int64(7))
		tx := c.Begin()
		tx.PatchAttrs(key(i), p, nil)
		if r := tx.Commit(); r.Conflicted() || r.HasUnapplied() {
			t.Fatalf("patch %d did not land", i)
		}
	}

	// Read through the RAW WIRE path, which is what reaches the splice.
	seen := 0
	for w := range c.ScanRawWire(nil, false) {
		ad, err := c.decodeWireAd(w)
		if err != nil {
			// A scan that cannot decode its own output is the mixed-chain splice producing
			// bytes that are not a valid ad.
			t.Fatalf("scan produced undecodable bytes: %v", err)
		}
		js, ok := ad.EvaluateAttrInt("JobStatus")
		if !ok {
			t.Fatal("scanned ad has no JobStatus: the merge dropped the patch")
		}
		if js != 7 {
			t.Fatalf("scanned JobStatus = %d, want 7", js)
		}
		if _, ok := ad.EvaluateAttrString("Owner"); !ok {
			t.Fatal("scanned ad lost Owner: the interned base was read as inline names")
		}
		seen++
	}
	if seen != n {
		t.Errorf("scanned %d ads, want %d", seen, n)
	}
}

// compactedBaseFixture returns a collection in which every key's whole record has been through a
// COMPACTION, which re-encodes what it rewrites as interned against a per-segment dictionary.
// Keys are overwritten first because compaction only rewrites a shard with dead bytes to
// reclaim; without that it has nothing to do and nothing is interned, and a test built on it
// passes while exercising nothing.
func compactedBaseFixture(t *testing.T) (*Collection, int) {
	t.Helper()
	c, _ := openDelta(t, 16)
	t.Cleanup(func() { _ = c.Close() })

	const n = 300
	put := func(i, round int) {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ProcId", int64(0))
		ad.InsertAttr("JobStatus", int64(1))
		ad.InsertAttr("Round", int64(round))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("seed conflicted")
		}
	}
	for i := range n {
		put(i, 0)
	}
	for round := 1; round <= 3; round++ {
		for i := range n {
			put(i, round)
		}
	}
	c.Compact()

	interned := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.dict.Load() != nil {
				interned++
			}
		}
		sh.mu.RUnlock()
	}
	if interned == 0 {
		t.Fatal("no interned segment after Compact: the chain would not span encodings and these tests would prove nothing")
	}
	return c, n
}
