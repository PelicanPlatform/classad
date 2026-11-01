package collections

import (
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// WRITING WITHOUT THE KEY.
//
// A patch write that cannot be stored as a delta composes a whole record from the stored one. The
// entitled path reads the stored ad with the key, merges, and re-encodes. A transaction not entitled
// to sealed values (BeginRedacted) cannot do that: its read decodes with no key, every sealed value
// comes back undefined, and re-encoding that ad wrote undefined over every secret the record held.
// One such write from an unprivileged session -- a DeleteAttribute of anything, or the SetAttribute
// that reached the chain bound -- destroyed them.
//
// So a redacted transaction composes from the stored BYTES instead. An attribute the write names is
// set (sealed if policy says so; sealing a new value opens nothing) or deleted; every other attribute
// is copied byte for byte, ciphertext included, and no stored sealed value is ever opened.
//
// What such a session may do to a sealed attribute it cannot see follows from that: an attribute it
// NAMES is a deliberate act and is applied -- SetAttribute overwrites it, DeleteAttribute removes it,
// exactly as for any other attribute and as an unprivileged session's writes always have -- and one it
// does not name is never touched. A write cannot destroy a value by accident.
//
// One thing the bytes cannot do. A sealed value's plaintext is its node encoding in the key form of
// the record it was sealed in, and compaction seals into INTERNED records, whose names are ids into
// their segment's dictionary. Such a value cannot be moved into an inline-names record without
// opening it to re-key it, and nothing in the ciphertext says whether it holds a name at all. When the
// result would have to carry one, the write is stored as a delta where it can be (a delta names only
// the attributes it sets, so the chain bound is exceeded rather than a value lost -- the bound is
// advisory, and the next seal collapses the chain with the key), and otherwise it is refused as
// unapplied: a removal, or a store with delta records off. A refusal leaves the key exactly as it was.

// keylessPart is one stored record a keyless compose reads: its decompressed bytes, which the caller
// owns, and, for an interned record, its segment's dictionary.
type keylessPart struct {
	raw     []byte
	resolve func(uint32) (string, bool)
}

// Counters for the keyless compose, so an operator can see how a redacted writer's whole-record writes
// were served: composed from bytes, stored as a delta past the bound because the record held an
// interned sealed value, or refused for the same reason (counted under unreadableByReason).
var (
	keylessComposes     atomic.Int64
	keylessOverBoundDel atomic.Int64
)

// KeylessWriteStats reports how a transaction not entitled to sealed values stored the writes that
// needed a whole record: composed from the stored bytes, or stored as a delta past the chain bound
// because the record held a sealed value only the key could carry. Refusals for that reason appear in
// UnreadableBaseReasons as "sealed-needs-key".
func KeylessWriteStats() (composed, overBoundDeltas int64) {
	return keylessComposes.Load(), keylessOverBoundDel.Load()
}

// keylessParts returns the stored records key's current version is made of at s0, oldest first: the
// record alone, or a delta chain's base followed by its deltas. Nothing is decoded. The bytes are
// copied out (decompressed into fresh buffers) and each dictionary's name cache built while the read
// lock holds the segments alive, so the caller may use both after it returns.
func (sh *shard) keylessParts(c *Collection, key []byte, h, s0 uint64) ([]keylessPart, readFail) {
	if readStoredMissHook != nil && readStoredMissHook(key) {
		return nil, failNotVisible // test-only: see readStoredMissHook
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findVisible(sh.dirGet(h), key, s0)
	if !ok {
		if l, ok = sh.lookupSealedAt(key, h, s0); !ok {
			return nil, failNotVisible
		}
	}
	seg := sh.segForLoc(l)
	if seg == nil {
		return nil, failSegGone
	}
	if !c.deltaRead || !recIsDelta(seg.data, l.off) {
		p, why := keylessPartAt(c, seg, l.off, failReassemble, failDecode)
		if why != failNone {
			return nil, why
		}
		if isDeltaRecord(p.raw) {
			return nil, failDeltaFlagMismatch
		}
		return []keylessPart{p}, failNone
	}
	vers, base, why := sh.chainVersions(key, h, s0)
	if why != failNone {
		return nil, why
	}
	parts := make([]keylessPart, 0, len(vers)-base)
	for i := base; i < len(vers); i++ {
		p, why := keylessPartAt(c, vers[i].seg, vers[i].off, failDeltaReassemble, failDeltaDecompress)
		if why != failNone {
			return nil, why
		}
		// The header chose the base; the payload must agree, as materializeAt checks.
		if isDeltaRecord(p.raw) != (i > base) {
			deltaFlagMismatches.Add(1)
			return nil, failDeltaFlagMismatch
		}
		parts = append(parts, p)
	}
	return parts, failNone
}

// keylessPartAt reads one record for keylessParts. Caller holds the shard read lock.
func keylessPartAt(c *Collection, seg *segment, off uint32, reassemble, decompress readFail) (keylessPart, readFail) {
	stored, codec, ok, why := segStoredOrReassembledWhy(c, seg, off)
	if !ok {
		if why == failReassemble {
			why = reassemble
		}
		return keylessPart{}, why
	}
	raw, err := codec.Decompress(nil, stored) // nil dst: a fresh buffer, never the segment's bytes
	if err != nil {
		return keylessPart{}, decompress
	}
	p := keylessPart{raw: raw}
	if d := seg.dict.Load(); d != nil {
		d.ensureNames() // resolve reads only this cache from here on, never the segment arena
		p.resolve = d.resolve
	}
	return p, failNone
}

// composeKeyless builds, without opening any sealed value, the whole inline-names record formed by
// parts (a base and the deltas over it, oldest first) with patch applied and removed deleted. It
// appends to dst.
//
// An interned part is rewritten to inline names first. Its sealed values cannot be rewritten (see
// above), so each must be one the result does not keep -- set or removed by the write, or set by a
// later part; if any is kept the compose reports failSealedNeedsKey and builds nothing.
func (c *Collection) composeKeyless(dst []byte, parts []keylessPart, patch *classad.ClassAd, removed []string) ([]byte, readFail) {
	if len(parts) == 0 {
		return dst, failDeltaNoVersions
	}
	// later holds the folded names whose value in an earlier part cannot reach the result: the write
	// sets or removes them, or a part after it sets them. Walking newest first grows it as it goes.
	later := make(map[string]struct{})
	if patch != nil {
		for _, a := range patch.AST().Attributes {
			later[strings.ToLower(a.Name)] = struct{}{}
		}
	}
	for _, n := range removed {
		later[strings.ToLower(n)] = struct{}{}
	}
	inl := make([]wire.Ad, len(parts))
	var sc wire.SubsetScratch
	var names []string
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		a := wire.Ad(p.raw)
		_, _, _, inline, ok := a.SplitBody()
		if !ok || (!inline && p.resolve == nil) {
			return dst, failDecode
		}
		names = names[:0]
		carried, unresolved := false, false
		ok = a.ForEachRaw(func(id uint32, name string, node []byte) bool {
			if !inline {
				n, found := p.resolve(id)
				if !found {
					unresolved = true
					return false
				}
				name = n
			}
			folded := strings.ToLower(name)
			if !inline && wire.IsEncryptedNode(node) {
				if _, gone := later[folded]; !gone {
					carried = true
					return false
				}
			}
			names = append(names, folded)
			return true
		})
		switch {
		case carried:
			return dst, failSealedNeedsKey
		case !ok || unresolved:
			return dst, failDecode
		}
		for _, n := range names {
			later[n] = struct{}{}
		}
		if inline {
			inl[i] = a
			continue
		}
		// No key: the rewrite drops this part's sealed entries, every one of which was just shown to
		// be replaced or removed in the result.
		out, ok := wire.AppendAdSubsetInlineFromInterned(nil, a, p.resolve, nil, nil, &sc)
		if !ok {
			return dst, failDecode
		}
		inl[i] = out
	}
	overlays := inl[1:]
	if patch != nil && len(patch.AST().Attributes) > 0 {
		overlays = append(overlays[:len(overlays):len(overlays)],
			wire.EncodeInlineDelta(nil, patch.AST(), nil, c.shouldEncrypt, c.sealer))
	}
	var msc wire.MergeScratch
	out, ok := wire.AppendAdPatchedInline(dst, inl[0], overlays, removed, &msc)
	if !ok {
		return dst, failDecode
	}
	return out, failNone
}

// encodePatchKeyless is encodePatchOnly's whole-record path for a transaction not entitled to sealed
// values: the record is composed from the stored bytes (composeKeyless), never from a redacted read.
// deltaOK says a delta would be a correct encoding of this write (no removal, delta records on), which
// is the way out when the stored record holds a sealed value only the key could carry.
func (tx *Txn) encodePatchKeyless(dst []byte, b *txnBuf, h uint64, deltaOK bool) (raw []byte, isDelta, ok bool, why readFail) {
	c := tx.c
	storeReads.Add(1)
	idx := c.shardOf(b.key, h)
	parts, why := c.shards[idx].keylessParts(c, b.key, h, tx.snapOf(idx))
	if why == failNone {
		var out []byte
		if out, why = c.composeKeyless(dst, parts, b.patch, b.removed); why == failNone {
			keylessComposes.Add(1)
			return out, false, true, failNone
		}
	}
	switch {
	case why == failNotVisible && !c.keyExists(b.key, h):
		// A create: nothing is stored, so nothing sealed can be lost, and the record is the patch.
		ad := classad.New()
		mergeDelta(ad, b.patch)
		for _, n := range b.removed {
			ad.Delete(n)
		}
		b.ad = ad
		return c.encodeAdInto(dst, ad.AST()), false, true, failNone
	case why == failSealedNeedsKey && deltaOK:
		// The key exists (it was just read), so a delta has a base; only the chain bound is exceeded.
		c.deltas.deltaInstead(h)
		keylessOverBoundDel.Add(1)
		return wire.EncodeInlineDelta(dst, b.patch.AST(), nil, c.shouldEncrypt, c.sealer), true, true, failNone
	}
	fallbackUnreadableBase.Add(1)
	unreadableByReason[why].Add(1)
	return nil, false, false, why
}
