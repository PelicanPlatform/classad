package collections

import (
	"github.com/PelicanPlatform/classad/ast"
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
	if !c.inline {
		return w, true
	}
	ad, err := c.decodeWire(w)
	if err != nil {
		return nil, false
	}
	return wire.Encode(dst[:0], ad, c.intern), true
}

// newStreamEncoder returns a StreamEncoder matching the collection's mode, for the
// direct old-ClassAd ingest path (UpdateOld).
func (c *Collection) newStreamEncoder() *wire.StreamEncoder {
	if c.inline {
		return wire.NewInlineStreamEncoder(c.currentHotNames())
	}
	return wire.NewStreamEncoder(c.intern, c.currentHotSet())
}
