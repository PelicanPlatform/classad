package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
)

// Sealer encrypts and decrypts an attribute value's encoded bytes for at-rest
// encryption (the nEncrypted node). A collection supplies one backed by a segment's
// data-encryption key; the wire layer stays crypto-agnostic. Seal returns a fresh
// nonce and ciphertext; Open reverses it and authenticates (errors on tampering or a
// wrong key). It is used from a single goroutine per encode/decode pass.
// Implementations must be safe for concurrent use: both the parallel query scan and the parallel wire
// scan open sealed values from several goroutines at once, and nothing serializes them. The built-in
// dataKeySealer satisfies this by holding only an immutable key and building a fresh GCM per call.
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

// sealedInlineFormRecoveries counts sealed values read in the inline-names key form from inside an
// interned ad; see readSealedPlain.
var sealedInlineFormRecoveries atomic.Int64

// SealedInlineFormRecoveries reports how many sealed values, since the process started, were read from
// an interned ad in the inline-names key form because they did not parse in the interned form (see
// readSealedPlain). Each is a value an earlier build copied unopened from an inline-names record into an
// interned one. It counts reads, not records: a record read twice counts twice, and a record stops
// counting once a rewrite re-seals it in the interned form.
func SealedInlineFormRecoveries() int64 { return sealedInlineFormRecoveries.Load() }

// refuseIDs is the resolver of a decoder reading inline-names plaintext. An inline encoder writes every
// name inline, so an interned id in such plaintext means it is not what an inline encoder wrote.
func refuseIDs(uint32) (string, bool) { return "", false }

// sealedRead says what readSealedPlain produces from a sealed value's plaintext.
type sealedRead uint8

const (
	sealedExpr sealedRead = iota // an ast.Expr (decoder.node)
	sealedText                   // the value's text appended to dst (decoder.appendNode)
)

// readSealedPlain reads pt, the opened plaintext of a sealed value in the ad d is reading, as an
// expression or as text appended to dst (as says which), and reports whether it was read in the
// inline-names form from an interned ad. On error the returned slice is dst as it was passed.
//
// A sealed value's plaintext is its node encoding in the key form of the ad it was SEALED in, and the
// reader assumes that is the form of the ad it is read from. An earlier build broke that: compaction
// copied sealed values unopened from inline-names records into interned ones. A value holding a record
// literal then carries its member names inline, and reading it with interned key semantics takes a
// name's length for an id and the name's first byte for a node tag. Those values are intact -- only the
// key form is wrong -- so when the interned parse of a sealed value in an interned ad is rejected, it is
// read again in the inline-names form.
//
// A parse is accepted only if it succeeds AND consumes every byte of pt. The encoder seals exactly one
// node, so leftover bytes mean the node was read in the wrong form even when what was read happened to
// be well formed. The inline retry is stricter still: an interned id anywhere in it is refused
// (refuseIDs), as is a nested sealed value (no key), because an inline encoder writes neither. A value
// rejected by both parses reports the first parse's error, exactly as before the retry existed.
//
// The interned form is tried first and wins whenever it parses, so the retry never reinterprets a value
// that is correct in the interned form, which is every value an interned encoder sealed. Both forms
// parsing is not ambiguous in practice: values without record members encode identically in both, and
// for a record the interned parse reads each member name's first byte as a node tag, which is below
// 0x18 only for a name starting with a control character -- something no unquoted attribute name can.
//
// The two decoders are values and the parse is chosen by as rather than passed in as a function, so
// that the common case, one accepted parse, allocates no decoder.
func (d *decoder) readSealedPlain(pt []byte, depth int, as sealedRead, dst []byte) (ast.Expr, []byte, bool, error) {
	sub := decoder{b: pt, inline: d.inline, resolve: d.resolve, open: d.open,
		redactSealed: d.redactSealed, oldStrings: d.oldStrings, inSealed: true}
	v, out, err := sub.readSealedAs(as, depth, dst)
	if err == nil && sub.pos != len(pt) {
		err = fmt.Errorf("%w: %d bytes left over after a sealed value", ErrMalformed, len(pt)-sub.pos)
	}
	if err == nil {
		return v, out, false, nil
	}
	// No retry when the interned parse failed for depth: that is not a key-form mismatch, and the
	// inline reading of a too-deep interned value can SUCCEED -- a record member's id taken for its
	// name's length, the bytes after it for the name -- returning a different, shallower value.
	if d.inline || errors.Is(err, errReadTooDeep) {
		return nil, dst, false, err
	}
	alt := decoder{b: pt, inline: true, resolve: refuseIDs, oldStrings: d.oldStrings, inSealed: true}
	av, aout, aerr := alt.readSealedAs(as, depth, dst)
	if aerr != nil || alt.pos != len(pt) {
		return nil, dst, false, err
	}
	sealedInlineFormRecoveries.Add(1)
	return av, aout, true, nil
}

// readSealedAs is one parse of readSealedPlain.
func (d *decoder) readSealedAs(as sealedRead, depth int, dst []byte) (ast.Expr, []byte, error) {
	if as == sealedText {
		out, err := d.appendNode(dst, depth)
		return nil, out, err
	}
	v, err := d.node(depth)
	return v, dst, err
}

// OpenEncryptedNode returns the plain node bytes sealed inside an nEncrypted
// node -- the sealed payload IS the value's ordinary node encoding, so opening
// it needs no re-encode. Returns (nil, false) if node is not an nEncrypted
// node, open is nil, or authentication fails. A relay that ships wire-form
// rows to a consumer without the data key uses this to emit the entry with its
// value in the clear (the consumer cannot open it).
func OpenEncryptedNode(node []byte, open Sealer) ([]byte, bool) {
	if open == nil || len(node) == 0 || node[0] != nEncrypted {
		return nil, false
	}
	c := &cursor{b: node, pos: 1, ok: true}
	nonce := c.readNameBytes() // the same uvarint-length-prefixed bytes, bounds-checked
	ct := c.readNameBytes()
	if !c.ok {
		return nil, false
	}
	plain, err := open.Open(nonce, ct)
	if err != nil {
		return nil, false
	}
	return plain, true
}

// IsEncryptedNode reports whether node is an nEncrypted (at-rest sealed) node.
func IsEncryptedNode(node []byte) bool {
	return len(node) > 0 && node[0] == nEncrypted
}

// DecodeNodeInlineEnc decodes raw node bytes from an inline-names ad (as returned by
// LookupByName), opening an nEncrypted node with open. A nil open leaves an encrypted
// node an error (the fast path treats that as absent).
func DecodeNodeInlineEnc(node []byte, open Sealer) (ast.Expr, error) {
	d := &decoder{b: node, inline: true, open: open}
	return d.node(0)
}

// DecodeInlineEnc is DecodeInline for an ad that may contain nEncrypted attributes:
// open supplies the segment's key so encrypted values decrypt to their real nodes.
// A nil open leaves encrypted attributes opaque and DecodeInline errors on them --
// use DecodeInlineEnc only on the DAEMON read path that holds the key.
func DecodeInlineEnc(b []byte, open Sealer) (*ast.ClassAd, error) {
	return decodeInlineEnc(b, open, false)
}

// DecodeInlineRedact is DecodeInlineEnc with no key that substitutes undefined for a sealed attribute
// instead of failing the ad -- the inline-names counterpart of DecodeResolveEncRedact, for a reader that
// is not entitled to sealed values.
func DecodeInlineRedact(b []byte) (*ast.ClassAd, error) {
	return decodeInlineEnc(b, nil, true)
}

func decodeInlineEnc(b []byte, open Sealer, redactSealed bool) (*ast.ClassAd, error) {
	d := &decoder{b: b, open: open, redactSealed: redactSealed}
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
