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
// their segment's dictionary. Such a value cannot be moved into an inline-names record unopened, and
// nothing in the ciphertext says whether it holds a name at all. So when the result has to carry one,
// the store re-keys it itself: the value is opened, its ids rewritten as names, and sealed again, all
// inside one step of the rewrite (wire.AppendAdInlineFromInternedResealed). That is the store's own
// maintenance, the same thing compaction does to every value it rewrites, done on the writer's behalf;
// the writer gains nothing from it. Its plaintext is never returned, logged, put in an error or kept,
// and nothing the session can observe depends on it: whether the write applies, how it is stored and
// what Commit reports follow only from which attributes the stored record and the write name. Only the
// values that need it are opened -- an interned sealed value the result keeps. One the write replaces
// or removes, or a later part of the chain supersedes, is dropped unopened, and one in an inline-names
// record is already in the form the result needs and is copied byte for byte.
//
// The one failure left is a carried value that will not open or parse, which only damage or a foreign
// key can produce; a keyed read of the record fails on it too. The write is then refused as "decode",
// the reason any undecodable record gives, and the key is left as it was.

// keylessPart is one stored record a keyless compose reads: its decompressed bytes, which the caller
// owns, and, for an interned record, its segment's dictionary.
type keylessPart struct {
	raw     []byte
	resolve func(uint32) (string, bool)
}

// Counters for the keyless compose, so an operator can see how a redacted writer's whole-record writes
// were served: composed from bytes, and how many of those had to re-seal interned sealed values (and
// how many values).
var (
	keylessComposes      atomic.Int64
	keylessOverBoundDel  atomic.Int64 // no longer incremented; see KeylessWriteStats
	keylessResealRecords atomic.Int64
	keylessResealValues  atomic.Int64
)

// KeylessWriteStats reports how a transaction not entitled to sealed values stored the writes that
// needed a whole record: composed counts every one composed from the stored bytes.
//
// overBoundDeltas is always 0. It counted writes stored as a delta past the chain bound because the
// record held a sealed value only the key could re-key; the store now re-seals such a value itself (see
// KeylessResealStats), so no write takes that path. It is kept so existing callers still compile.
func KeylessWriteStats() (composed, overBoundDeltas int64) {
	return keylessComposes.Load(), keylessOverBoundDel.Load()
}

// KeylessResealStats reports, of the whole records a transaction not entitled to sealed values composed
// from the stored bytes, how many carried sealed values from an interned record and so had the store
// re-seal them, and how many values that was in all.
func KeylessResealStats() (records, values int64) {
	return keylessResealRecords.Load(), keylessResealValues.Load()
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

// composeKeyless builds the whole inline-names record formed by parts (a base and the deltas over it,
// oldest first) with patch applied and removed deleted, appending it to dst. resealed counts the sealed
// values it had to re-key.
//
// An interned part is rewritten to inline names first. Each of its sealed values the result keeps --
// not set or removed by the write, not set by a later part -- is re-sealed in the inline form (see
// above); every other one is dropped unopened. Inline parts are copied, sealed values unopened.
func (c *Collection) composeKeyless(dst []byte, parts []keylessPart, patch *classad.ClassAd, removed []string) (out []byte, resealed int, why readFail) {
	if len(parts) == 0 {
		return dst, 0, failDeltaNoVersions
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
	// carry is the rewrite's keep test for an interned part: an unsealed entry is always kept (the merge
	// below lets a later value win), a sealed one only if it reaches the result. It reads later as it
	// stands before the part's own names are added, so it must run before they are.
	carry := func(name, node []byte) bool {
		if !wire.IsEncryptedNode(node) {
			return true
		}
		_, gone := later[strings.ToLower(string(name))]
		return !gone
	}
	inl := make([]wire.Ad, len(parts))
	var sc wire.SubsetScratch
	var names []string
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		a := wire.Ad(p.raw)
		_, _, _, inline, ok := a.SplitBody()
		if !ok || (!inline && p.resolve == nil) {
			return dst, resealed, failDecode
		}
		names = names[:0]
		unresolved := false
		ok = a.ForEachRaw(func(id uint32, name string, _ []byte) bool {
			if !inline {
				n, found := p.resolve(id)
				if !found {
					unresolved = true
					return false
				}
				name = n
			}
			names = append(names, strings.ToLower(name))
			return true
		})
		if !ok || unresolved {
			return dst, resealed, failDecode
		}
		if inline {
			inl[i] = a
		} else {
			rec, n, ok := wire.AppendAdInlineFromInternedResealed(nil, a, p.resolve, carry, c.sealer, &sc)
			resealed += n
			if !ok {
				// Malformed, or a carried sealed value that would not open or parse. Either way the
				// record cannot be composed; the reason is the one a keyed read of it would give.
				return dst, resealed, failDecode
			}
			inl[i] = rec
		}
		for _, n := range names {
			later[n] = struct{}{}
		}
	}
	overlays := inl[1:]
	if patch != nil && len(patch.AST().Attributes) > 0 {
		overlays = append(overlays[:len(overlays):len(overlays)],
			wire.EncodeInlineDelta(nil, patch.AST(), nil, c.shouldEncrypt, c.sealer))
	}
	var msc wire.MergeScratch
	out, ok := wire.AppendAdPatchedInline(dst, inl[0], overlays, removed, &msc)
	if !ok {
		return dst, resealed, failDecode
	}
	return out, resealed, failNone
}

// encodePatchKeyless is encodePatchOnly's whole-record path for a transaction not entitled to sealed
// values: the record is composed from the stored bytes (composeKeyless), never from a redacted read.
func (tx *Txn) encodePatchKeyless(dst []byte, b *txnBuf, h uint64) (raw []byte, isDelta, ok bool, why readFail) {
	c := tx.c
	storeReads.Add(1)
	idx := c.shardOf(b.key, h)
	parts, why := c.shards[idx].keylessParts(c, b.key, h, tx.snapOf(idx))
	if why == failNone {
		var out []byte
		var n int
		if out, n, why = c.composeKeyless(dst, parts, b.patch, b.removed); why == failNone {
			keylessComposes.Add(1)
			if n > 0 {
				keylessResealRecords.Add(1)
				keylessResealValues.Add(int64(n))
			}
			return out, false, true, failNone
		}
	}
	if why == failNotVisible && !c.keyExists(b.key, h) {
		// A create: nothing is stored, so nothing sealed can be lost, and the record is the patch.
		ad := classad.New()
		mergeDelta(ad, b.patch)
		for _, n := range b.removed {
			ad.Delete(n)
		}
		b.ad = ad
		return c.encodeAdInto(dst, ad.AST()), false, true, failNone
	}
	fallbackUnreadableBase.Add(1)
	unreadableByReason[why].Add(1)
	return nil, false, false, why
}
