package wire

import (
	"encoding/binary"
	"fmt"

	"github.com/PelicanPlatform/classad/ast"
)

// Sealer encrypts and decrypts an attribute value's encoded bytes for at-rest
// encryption (the nEncrypted node). A collection supplies one backed by a segment's
// data-encryption key; the wire layer stays crypto-agnostic. Seal returns a fresh
// nonce and ciphertext; Open reverses it and authenticates (errors on tampering or a
// wrong key). It is used from a single goroutine per encode/decode pass.
type Sealer interface {
	Seal(plaintext []byte) (nonce, ciphertext []byte, err error)
	Open(nonce, ciphertext []byte) (plaintext []byte, err error)
}

// encNode writes v as an nEncrypted node: v's ordinary node encoding is produced with
// a sub-encoder sharing this encoder's mode (and intern table, so any interned name in
// v resolves against the same table on decode), then sealed. Layout after the tag:
// uvarint(len nonce) + nonce + uvarint(len ct) + ct. If sealing fails, it falls back to
// the plaintext node so a data value is never lost (Seal failure is not expected).
func (e *encoder) encNode(v ast.Expr) {
	sub := encoder{t: e.t, inline: e.inline}
	sub.node(v)
	nonce, ct, err := e.seal.Seal(sub.buf)
	if err != nil {
		e.node(v)
		return
	}
	e.buf = append(e.buf, nEncrypted)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(nonce)))
	e.buf = append(e.buf, nonce...)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(ct)))
	e.buf = append(e.buf, ct...)
}

// DecodeInlineEnc is DecodeInline for an ad that may contain nEncrypted attributes:
// open supplies the segment's key so encrypted values decrypt to their real nodes.
// A nil open leaves encrypted attributes opaque and DecodeInline errors on them --
// use DecodeInlineEnc only on the DAEMON read path that holds the key.
func DecodeInlineEnc(b []byte, open Sealer) (*ast.ClassAd, error) {
	d := &decoder{b: b, open: open}
	flags, err := d.headerFlags()
	if err != nil {
		return nil, err
	}
	if flags&flagStandalone != 0 {
		return nil, fmt.Errorf("%w: standalone ad; use DecodeStandalone", ErrMalformed)
	}
	if flags&flagInlineNames == 0 {
		return nil, fmt.Errorf("%w: DecodeInlineEnc requires an inline-names ad", ErrMalformed)
	}
	d.inline = true
	return d.adBody(0)
}
