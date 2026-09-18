package wire

import (
	"encoding/binary"
	"math"

	"github.com/PelicanPlatform/classad/ast"
)

// encoder appends bytes to buf. When inline is false, names (attribute keys, and
// the names inside attribute-reference/function/select nodes) are interned into t
// and stored as ids. When inline is true, names are stored verbatim and t is
// unused, producing a self-contained ad (see flagInlineNames).
type encoder struct {
	buf    []byte
	t      *InternTable
	inline bool
	// seal, when non-nil, is used by encNode to encrypt a value node into an nEncrypted
	// node. Which attributes get encrypted is decided by the caller's predicate. Used
	// only by the inline-names store path.
	seal Sealer
}

// putKey writes an attribute key: an interned id, or an inline name in inline mode.
func (e *encoder) putKey(name string) {
	if e.inline {
		e.putString(name)
	} else {
		e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(name)))
	}
}

// Encode appends the binary form of ad to dst and returns the extended slice,
// using the shared intern table t (mutating it to add any new names). The
// resulting bytes are NOT self-contained: decoding requires the same table.
func Encode(dst []byte, ad *ast.ClassAd, t *InternTable) []byte {
	e := encoder{buf: dst, t: t}
	e.buf = append(e.buf, magicByte, formatVer, 0 /* flags */)
	e.adBody(ad)
	return e.buf
}

// EncodeWithHot is Encode with a populated hot header: attributes whose interned
// id is in hot are indexed by a (id, entries-relative offset) pair written before
// the body, so Ad.Lookup finds them in O(1) instead of scanning. The body layout
// is otherwise identical to Encode, so the result decodes with Decode and the hot
// header is a pure read-time accelerator. hot may be nil (equivalent to Encode).
func EncodeWithHot(dst []byte, ad *ast.ClassAd, t *InternTable, hot map[uint32]struct{}) []byte {
	return EncodeWithHotClosure(dst, ad, t, hot, false)
}

// EncodeWithHotClosure is EncodeWithHot that, when closureComplete, marks the ad with
// flagHotClosure -- a promise that hot holds the complete match closure, so a matcher
// can read it via ForEachHot without scanning. Set closureComplete only when hot truly
// contains every attribute the match reads from ad (see collections' astClosure).
func EncodeWithHotClosure(dst []byte, ad *ast.ClassAd, t *InternTable, hot map[uint32]struct{}, closureComplete bool) []byte {
	if ad == nil {
		return Encode(dst, ad, t)
	}
	var extraFlags byte
	if closureComplete {
		extraFlags = flagHotClosure
	}
	// Write the entries (id, node)* into a scratch buffer, recording the
	// entries-relative offset of each hot attribute's node.
	e := encoder{t: t, buf: getScratch()}
	defer func() { putScratch(e.buf) }()
	var hots []hotPair
	for _, attr := range ad.Attributes {
		id := t.Intern(attr.Name)
		e.buf = binary.AppendUvarint(e.buf, uint64(id))
		nodeOff := len(e.buf) // offset of the node within the entries region
		e.node(attr.Value)
		if _, ok := hot[id]; ok {
			hots = append(hots, hotPair{id, uint32(nodeOff)})
		}
	}
	return frame(dst, extraFlags, hots, len(ad.Attributes), e.buf)
}

// EncodeInline encodes ad with inline attribute names (no interning), producing a
// fully self-contained ad that DecodeInline reads with no InternTable. Used by the
// persistent store so on-disk records are recoverable without a shared table.
func EncodeInline(dst []byte, ad *ast.ClassAd) []byte {
	return EncodeInlineWithHot(dst, ad, nil)
}

// EncodeInlineWithHot is EncodeInline with a populated hot header: attributes whose
// (case-folded) name is in hot are indexed by a (nameHash32, entries-relative
// offset-to-entry) pair, so Ad.LookupByName finds them without scanning. hot keys
// must be lower-cased. hot may be nil.
// EncodeWithHotEnc is EncodeWithHot for at-rest encryption: any attribute for which
// encrypt(name) is true is sealed with seal and stored as an nEncrypted node (its value's
// interned node encoding, sealed), exactly as EncodeInlineWithHotEnc does for inline ads.
// Encrypted attributes are never hot (never indexed), so they never enter the hot header.
// encrypt and seal must both be non-nil to encrypt anything; with seal nil this is EncodeWithHot.
func EncodeWithHotEnc(dst []byte, ad *ast.ClassAd, t *InternTable, hot map[uint32]struct{}, encrypt func(name string) bool, seal Sealer) []byte {
	if ad == nil {
		return Encode(dst, ad, t)
	}
	e := encoder{t: t, seal: seal, buf: getScratch()}
	defer func() { putScratch(e.buf) }()
	var hots []hotPair
	for _, attr := range ad.Attributes {
		id := t.Intern(attr.Name)
		e.buf = binary.AppendUvarint(e.buf, uint64(id))
		nodeOff := len(e.buf)
		if seal != nil && encrypt != nil && encrypt(attr.Name) {
			e.encNode(attr.Value) // sealed; never hot
			continue
		}
		e.node(attr.Value)
		if _, ok := hot[id]; ok {
			hots = append(hots, hotPair{id, uint32(nodeOff)})
		}
	}
	return frame(dst, 0 /* flags */, hots, len(ad.Attributes), e.buf)
}

func EncodeInlineWithHot(dst []byte, ad *ast.ClassAd, hot map[string]struct{}) []byte {
	return EncodeInlineWithHotEnc(dst, ad, hot, nil, nil)
}

// EncodeInlineWithHotEnc is EncodeInlineWithHot with at-rest encryption: the value of
// any attribute for which encrypt(name) is true is sealed with seal and stored as an
// nEncrypted node (decodable only with the matching Opener). A predicate (rather than a
// set) lets the caller encrypt by rule -- e.g. HTCondor's private-attribute prefix --
// not just an enumerable list. An encrypted attribute is never hot: it is opaque to the
// index/match fast path, so it is excluded from the hot header even if listed in hot.
// encrypt and seal must both be non-nil to encrypt anything.
func EncodeInlineWithHotEnc(dst []byte, ad *ast.ClassAd, hot map[string]struct{}, encrypt func(name string) bool, seal Sealer) []byte {
	return encodeInline(dst, ad, hot, encrypt, seal, 0)
}

// EncodeInlineDelta encodes ad as a DELTA record: byte-identical to an ordinary inline ad
// except that flagDelta is set, so a reader knows these are only the attributes one write
// changed and that the rest must be merged in from older versions of the key. The caller is
// responsible for having built ad from exactly the changed attributes.
func EncodeInlineDelta(dst []byte, ad *ast.ClassAd, hot map[string]struct{}, encrypt func(name string) bool, seal Sealer) []byte {
	return encodeInline(dst, ad, hot, encrypt, seal, flagDelta)
}

func encodeInline(dst []byte, ad *ast.ClassAd, hot map[string]struct{}, encrypt func(name string) bool, seal Sealer, extraFlags byte) []byte {
	e := encoder{inline: true, seal: seal, buf: getScratch()}
	defer func() { putScratch(e.buf) }()
	var hots []hotPair
	if ad != nil {
		// Hot entries are written FIRST, so the hot region occupies a physical
		// prefix of the record: a compressed store can then decompress only that
		// prefix to serve a hot-covered projection (see Codec.DecompressPrefix).
		// The partition is stable within each class, so among same-named
		// duplicates the first still comes first.
		isHot := func(attr *ast.AttributeAssignment) bool {
			if seal != nil && encrypt != nil && encrypt(attr.Name) {
				return false // encrypted attributes are never indexed / hot
			}
			return inFolded(hot, attr.Name)
		}
		for _, attr := range ad.Attributes {
			if !isHot(attr) {
				continue
			}
			entryOff := len(e.buf)
			e.putString(attr.Name)
			e.node(attr.Value)
			hots = append(hots, hotPair{nameHash32(attr.Name), uint32(entryOff)})
		}
		for _, attr := range ad.Attributes {
			if isHot(attr) {
				continue
			}
			e.putString(attr.Name)
			if seal != nil && encrypt != nil && encrypt(attr.Name) {
				e.encNode(attr.Value)
				continue
			}
			e.node(attr.Value)
		}
	}
	n := 0
	if ad != nil {
		n = len(ad.Attributes)
	}
	return frame(dst, flagInlineNames|extraFlags, hots, n, e.buf)
}

// inFolded reports whether set (keyed by foldASCII'd names) contains name, without
// allocating the folded form. foldASCII allocates twice for any name carrying an
// uppercase letter -- i.e. for essentially every ClassAd attribute -- and this lookup
// runs once per attribute per encode, so the folded key is built in a stack buffer and
// handed to the map lookup as a []byte->string conversion the compiler does not
// materialize. A name longer than the buffer falls back to the allocating form.
func inFolded(set map[string]struct{}, name string) bool {
	if len(set) == 0 {
		return false
	}
	var buf [96]byte
	if len(name) > len(buf) {
		_, ok := set[foldASCII(name)]
		return ok
	}
	b := buf[:len(name)]
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	_, ok := set[string(b)]
	return ok
}

// foldASCII lower-cases ASCII letters in s (attribute names are case-insensitive).
func foldASCII(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// EncodeStandalone returns a self-contained encoding that embeds a minimal
// intern table, so it can be decoded with DecodeStandalone alone (e.g. for
// transport out of a collection).
func EncodeStandalone(ad *ast.ClassAd) []byte {
	t := NewInternTable()
	// First pass: encode the body against a fresh table so every referenced
	// name is interned, then prepend the table.
	e := encoder{t: t}
	e.adBody(ad)
	body := e.buf

	names := t.snapshotNames()
	out := make([]byte, 0, len(body)+16*len(names)+8)
	out = append(out, magicByte, formatVer, flagStandalone)
	out = binary.AppendUvarint(out, uint64(len(names)))
	for _, n := range names {
		out = binary.AppendUvarint(out, uint64(len(n)))
		out = append(out, n...)
	}
	out = append(out, body...)
	return out
}

// adBody writes [hotCount][hot entries][attrCount][attr entries]. Hot header is
// empty here; the store populates it at compaction. Used for both the top-level
// ad and nested records.
func (e *encoder) adBody(ad *ast.ClassAd) {
	e.buf = binary.AppendUvarint(e.buf, 0) // hotCount (populated by the store later)
	if ad == nil {
		e.buf = binary.AppendUvarint(e.buf, 0)
		return
	}
	e.buf = binary.AppendUvarint(e.buf, uint64(len(ad.Attributes)))
	for _, attr := range ad.Attributes {
		e.putKey(attr.Name)
		e.node(attr.Value)
	}
}

// node writes a single expression node (pre-order, self-delimiting).
func (e *encoder) node(expr ast.Expr) {
	switch v := expr.(type) {
	case nil:
		e.buf = append(e.buf, nUndefined)
	case *ast.UndefinedLiteral:
		e.buf = append(e.buf, nUndefined)
	case *ast.ErrorLiteral:
		e.buf = append(e.buf, nError)
	case *ast.BooleanLiteral:
		if v.Value {
			e.buf = append(e.buf, nBoolTrue)
		} else {
			e.buf = append(e.buf, nBoolFalse)
		}
	case *ast.IntegerLiteral:
		e.buf = append(e.buf, nInt)
		e.buf = binary.AppendVarint(e.buf, v.Value)
	case *ast.RealLiteral:
		e.buf = append(e.buf, nReal)
		e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(v.Value))
	case *ast.StringLiteral:
		e.buf = append(e.buf, nString)
		e.putString(v.Value)
	case *ast.AttributeReference:
		if e.inline {
			e.buf = append(e.buf, nAttrRefStr, byte(v.Scope))
			e.putString(v.Name)
		} else {
			e.buf = append(e.buf, nAttrRef, byte(v.Scope))
			e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(v.Name)))
		}
	case *ast.BinaryOp:
		if id, ok := binOpID[v.Op]; ok {
			e.buf = append(e.buf, nBinOp, id)
		} else {
			e.buf = append(e.buf, nBinOpStr)
			e.putString(v.Op)
		}
		e.node(v.Left)
		e.node(v.Right)
	case *ast.UnaryOp:
		if id, ok := unOpID[v.Op]; ok {
			e.buf = append(e.buf, nUnOp, id)
		} else {
			e.buf = append(e.buf, nUnOpStr)
			e.putString(v.Op)
		}
		e.node(v.Expr)
	case *ast.ListLiteral:
		e.buf = append(e.buf, nList)
		e.buf = binary.AppendUvarint(e.buf, uint64(len(v.Elements)))
		for _, el := range v.Elements {
			e.node(el)
		}
	case *ast.RecordLiteral:
		e.buf = append(e.buf, nRecord)
		e.adBody(v.ClassAd)
	case *ast.FunctionCall:
		if e.inline {
			e.buf = append(e.buf, nFuncStr)
			e.putString(v.Name)
		} else {
			e.buf = append(e.buf, nFunc)
			e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(v.Name)))
		}
		e.buf = binary.AppendUvarint(e.buf, uint64(len(v.Args)))
		for _, a := range v.Args {
			e.node(a)
		}
	case *ast.ConditionalExpr:
		e.buf = append(e.buf, nCond)
		e.node(v.Condition)
		e.node(v.TrueExpr)
		e.node(v.FalseExpr)
	case *ast.ElvisExpr:
		e.buf = append(e.buf, nElvis)
		e.node(v.Left)
		e.node(v.Right)
	case *ast.SelectExpr:
		if e.inline {
			e.buf = append(e.buf, nSelectStr)
			e.node(v.Record)
			e.putString(v.Attr)
		} else {
			e.buf = append(e.buf, nSelect)
			e.node(v.Record)
			e.buf = binary.AppendUvarint(e.buf, uint64(e.t.Intern(v.Attr)))
		}
	case *ast.SubscriptExpr:
		e.buf = append(e.buf, nSubscript)
		e.node(v.Container)
		e.node(v.Index)
	case *ast.ParenExpr:
		e.buf = append(e.buf, nParen)
		e.node(v.Inner)
	default:
		// Unknown node type: encode as undefined rather than panic. The decoder
		// side treats this as an undefined literal. This is a defensive default;
		// every ast.Expr type above is handled.
		e.buf = append(e.buf, nUndefined)
	}
}

func (e *encoder) putString(s string) {
	e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
	e.buf = append(e.buf, s...)
}
