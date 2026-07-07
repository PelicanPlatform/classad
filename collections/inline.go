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
func (c *Collection) encodeAd(ad *ast.ClassAd) []byte {
	if c.inline {
		return wire.EncodeInlineWithHot(nil, ad, c.hotNames)
	}
	return wire.EncodeWithHot(nil, ad, c.intern, c.currentHotSet())
}

// decodeWire decodes stored wire bytes back to an ast.ClassAd.
func (c *Collection) decodeWire(w []byte) (*ast.ClassAd, error) {
	if c.inline {
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

// decodeNode decodes raw node bytes (from wireLookup) into an ast.Expr.
func (c *Collection) decodeNode(node []byte) (ast.Expr, error) {
	if c.inline {
		return wire.DecodeNodeInline(node)
	}
	return wire.DecodeNode(node, c.intern)
}

// newStreamEncoder returns a StreamEncoder matching the collection's mode, for the
// direct old-ClassAd ingest path (UpdateOld).
func (c *Collection) newStreamEncoder() *wire.StreamEncoder {
	if c.inline {
		return wire.NewInlineStreamEncoder(c.hotNames)
	}
	return wire.NewStreamEncoder(c.intern, c.currentHotSet())
}
