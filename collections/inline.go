package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// This file centralizes the one difference between an in-memory and a persistent
// collection's wire encoding: in-memory ads use interned attribute ids (against
// the shared c.intern table); persistent ads store inline names (self-contained,
// so segment files recover without a table). Every wire touchpoint routes through
// these helpers so the rest of the store is oblivious to the mode.

// encodeAd encodes an ad to wire bytes for storage, with the collection's hot set.
// When encryption at rest is enabled, the designated attributes' values are sealed
// with the DB data key (persistent/inline collections only).
func (c *Collection) encodeAd(ad *ast.ClassAd) []byte {
	if c.inline {
		if c.sealer != nil {
			return wire.EncodeInlineWithHotEnc(nil, ad, c.currentHotNames(), c.shouldEncrypt, c.sealer)
		}
		return wire.EncodeInlineWithHot(nil, ad, c.currentHotNames())
	}
	hot, closureComplete := c.hotSetForEncode(ad)
	return wire.EncodeWithHotClosure(nil, ad, c.intern, hot, closureComplete)
}

// decodeWire decodes stored wire bytes back to an ast.ClassAd, opening any encrypted
// attributes with the DB data key.
func (c *Collection) decodeWire(w []byte) (*ast.ClassAd, error) {
	if c.inline {
		if c.sealer != nil {
			return wire.DecodeInlineEnc(w, c.sealer)
		}
		return wire.DecodeInline(w)
	}
	return wire.Decode(w, c.intern)
}

// wireLookup returns the raw node bytes for a named attribute in an ad's wire
// bytes, by inline name or interned id.
func (c *Collection) wireLookup(a wire.Ad, name string) ([]byte, bool) {
	if c.inline {
		return a.LookupByName(name)
	}
	id, ok := c.intern.LookupID(name)
	if !ok {
		return nil, false
	}
	return a.Lookup(id)
}

// decodeNode decodes raw node bytes (from wireLookup) into an ast.Expr, opening an
// encrypted node with the DB data key. The collection always holds the key; a higher
// layer (the server) decides whether a given reader may see the decrypted value.
func (c *Collection) decodeNode(node []byte) (ast.Expr, error) {
	if c.inline {
		if c.sealer != nil {
			return wire.DecodeNodeInlineEnc(node, c.sealer)
		}
		return wire.DecodeNodeInline(node)
	}
	return wire.DecodeNode(node, c.intern)
}

// recordToInterned re-encodes a decompressed stored record `w` into interned wire that the
// id-keyed columnar accelerator (buildAdSchema / adSchema.encode / bruteNumCount) can read.
// An interned (in-memory) collection's records are already interned and returned as-is. An
// inline (persistent) collection's records store names, not ids -- and may carry encrypted
// attribute values -- so the record is decoded (opening encryption) and re-encoded against the
// shared intern table. Returns (nil,false) on a record that fails to decode. Used only at
// accelerator build time (schema sampling and per-segment block transcode), not on the scan
// hot path.
func (c *Collection) recordToInterned(dst, w []byte) ([]byte, bool) {
	return c.recordToInternedDict(nil, dst, w)
}

// recordToInternedDict is recordToInterned honoring a per-segment interned record: dict!=nil
// resolves segment-local ids (via decodeWireDict) before re-encoding to the collection's
// GLOBAL intern table. dict==nil is the legacy path (in-memory already global-interned -> as
// is; inline -> decode + re-encode). Used by the adschema block build over sealed segments,
// which may now be interned.
func (c *Collection) recordToInternedDict(dict *segDictHandle, dst, w []byte) ([]byte, bool) {
	if dict == nil && !c.inline {
		return w, true // in-memory: already global-interned
	}
	ad, err := c.decodeWireDict(dict, w)
	if err != nil {
		return nil, false
	}
	return wire.Encode(dst[:0], ad, c.intern), true
}

// --- per-segment interned dispatch (interning) ---
//
// A sealed segment may be INTERNED with its own attribute dictionary (segment.dict != nil):
// its records carry segment-local ids resolved to names via that dict, not inline names and
// not the collection's global table. These wrappers take the segment's dict handle and, when
// it is non-nil, resolve through it (zero-copy over the mmap); a nil handle means the segment
// is inline/global -- the legacy path, byte-for-byte unchanged. Scan/read call sites thread
// the segment's dict (segment.dict.Load()) from the visibility window. An interned segment of
// an encryption-at-rest collection carries SEALED value nodes, so the resolver paths pass the
// collection's sealer to open them (nil for a plaintext collection -- opens nothing).

func (c *Collection) decodeWireDict(dict *segDictHandle, w []byte) (*ast.ClassAd, error) {
	if dict != nil {
		// c.sealer is nil for a plaintext collection (opens nothing) and set for an
		// encrypted one (opens the sealed value nodes an interned+encrypted segment carries).
		return wire.DecodeResolveEnc(w, dict.resolve, c.sealer)
	}
	return c.decodeWire(w)
}

// encodeInterned encodes ad in the form the compaction/reseal transcode writes an interned
// segment: segment-local ids against `table` with a hot header, sealing designated attribute
// values when the collection encrypts at rest. Mirrors encodeAd's persistent branch, but
// interned rather than inline.
func (c *Collection) encodeInterned(dst []byte, ad *ast.ClassAd, table *wire.InternTable, hot map[uint32]struct{}) []byte {
	if c.sealer != nil {
		return wire.EncodeWithHotEnc(dst, ad, table, hot, c.shouldEncrypt, c.sealer)
	}
	return wire.EncodeWithHot(dst, ad, table, hot)
}

func (c *Collection) wireLookupDict(dict *segDictHandle, a wire.Ad, name string) ([]byte, bool) {
	if dict != nil {
		id, ok := dict.lookup(name)
		if !ok {
			return nil, false
		}
		return a.Lookup(id)
	}
	return c.wireLookup(a, name)
}

func (c *Collection) decodeNodeDict(dict *segDictHandle, node []byte) (ast.Expr, error) {
	if dict != nil {
		return wire.DecodeNodeResolveEnc(node, dict.resolve, c.sealer)
	}
	return c.decodeNode(node)
}

// decodeAdDict is decodeAd (decompress + decode to a ClassAd) that resolves an interned
// segment's ids via its dict. dict==nil => the existing decodeAd path (inline/global).
func (c *Collection) decodeAdDict(dict *segDictHandle, stored []byte, codec Codec) (*classad.ClassAd, error) {
	if dict == nil {
		return c.decodeAd(stored, codec)
	}
	dec, err := codec.Decompress(nil, stored)
	if err != nil {
		return nil, err
	}
	a, err := wire.DecodeResolveEnc(dec, dict.resolve, c.sealer)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(a), nil
}

// wireToInline is toSelfContained for already-DECOMPRESSED wire (no codec): an interned
// segment's decompressed record becomes inline wire so a downstream consumer that reads it by
// name -- CollectSamples -> buildAdSchema / ForEachNamed(c.intern) -- works without the dict.
// nil dict (inline segment / in-memory global) returns w unchanged.
func (c *Collection) wireToInline(dict *segDictHandle, w []byte) []byte {
	if dict == nil {
		return w
	}
	a, err := c.decodeWireDict(dict, w) // sealer-aware: opens sealed values for an encrypted segment
	if err != nil {
		return w
	}
	if c.sealer != nil {
		// Re-seal on the way to inline so the sample never carries plaintext values (which would
		// leak into a retrained dict or a columnar block); matches the active segment's form.
		return wire.EncodeInlineWithHotEnc(nil, a, nil, c.shouldEncrypt, c.sealer)
	}
	return wire.EncodeInline(nil, a)
}

// renderKey is the key a render path is entitled to: none for a redacted read, the collection's for a
// privileged one. Passing it to the renderer -- rather than opening the whole ad first -- is what makes an
// unentitled read unable to produce a secret at all, instead of producing one and filtering it by name.
func (c *Collection) renderKey(redact bool) wire.Sealer {
	if redact {
		return nil
	}
	return c.sealer
}

// wireToInlineNoKey is wireToInline for a read that is NOT entitled to sealed values: it decodes with no
// key at all, so a sealed value becomes undefined (see wire.DecodeResolveEncRedact) and no plaintext
// secret is ever materialized.
//
// wireToInline opens every sealed value with the collection's key and immediately re-seals it, which is
// right for a rewrite but wrong for serving an unentitled reader: the secret was decrypted in process and
// then filtered out by NAME afterwards. Filtering is a deny-list; not holding the key is not.
//
// Nothing is re-sealed here because nothing sealed survives the decode, so the inline form carries
// undefined where the secret was.
func (c *Collection) wireToInlineNoKey(dict *segDictHandle, w []byte) []byte {
	if dict == nil {
		return w
	}
	a, err := wire.DecodeResolveEncRedact(w, dict.resolve, nil)
	if err != nil {
		return w
	}
	return wire.EncodeInline(nil, a)
}

// toSelfContained converts a stored record's (compressed) ad bytes into a form that decodes
// WITHOUT the source segment's dict, for DEFERRED/cold paths (e.g. a watch event) that copy ad
// bytes out of the scan and decode them later, when the segment and its dict are no longer in
// scope. For an inline segment or an in-memory (global-interned) collection -- dict==nil -- the
// bytes already self-decode, so they are returned unchanged. For an interned segment the record
// is decoded via the dict, re-encoded inline, and recompressed under the SAME codec, so a later
// decodeAd(bytes, codec) yields the identical ad. Best-effort: returns the input unchanged on
// any decode/encode error (a later decode then fails exactly as it would have anyway). The hot
// scan paths do NOT use this -- they thread the dict and dispatch -- so its decode+re-encode
// cost lands only on cold event capture.
func (c *Collection) toSelfContained(dict *segDictHandle, ad []byte, codec Codec) []byte {
	if dict == nil {
		return ad
	}
	dec, err := codec.Decompress(nil, ad)
	if err != nil {
		return ad
	}
	a, err := c.decodeWireDict(dict, dec) // sealer-aware: opens sealed values for an encrypted segment
	if err != nil {
		return ad
	}
	if c.sealer != nil {
		// Re-seal so a deferred event's copied bytes stay sealed at rest, matching the source.
		return codec.Compress(nil, wire.EncodeInlineWithHotEnc(nil, a, nil, c.shouldEncrypt, c.sealer))
	}
	return codec.Compress(nil, wire.EncodeInline(nil, a))
}

// newStreamEncoder returns a StreamEncoder matching the collection's mode, for the
// direct old-ClassAd ingest path (UpdateOld).
func (c *Collection) newStreamEncoder() *wire.StreamEncoder {
	if c.inline {
		return wire.NewInlineStreamEncoder(c.currentHotNames())
	}
	return wire.NewStreamEncoder(c.intern, c.currentHotSet())
}
