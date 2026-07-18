package collections

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/parser"
)

// errDuplicate signals that an ad has a repeated attribute name, where streaming
// (first-wins on lookup) would diverge from the parser (last-wins). The ad is
// re-encoded via the full parser, which dedups exactly.
var errDuplicate = errors.New("duplicate attribute")

// errDeferOld signals that a value needs the reference old-ClassAd parser (classad.ParseOld)
// to encode correctly -- specifically a non-scalar, non-lone-string value that contains a
// backslash, whose embedded string literals the fast expression path (ByteLexer / ParseExpr)
// would lex with new-ClassAd escape rules and thus diverge from ParseOld. Backslash-free
// expressions lex identically in both, so they keep the fast path. Like errDuplicate, the
// whole ad is re-encoded via ParseOld.
var errDeferOld = errors.New("defer to old-ClassAd parser")

// OldAdUpdate is one insert-or-update whose ad is supplied in "old ClassAd"
// serialization (newline-separated `Name = Value` lines, as sent over a TCP
// socket), rather than as a parsed *classad.ClassAd.
type OldAdUpdate struct {
	Key  []byte
	Text string
}

// UpdateOld applies a batch of inserts/updates whose ads arrive in old-ClassAd
// form. It encodes each ad directly to the wire form, attribute by attribute,
// without materializing an intermediate ast.ClassAd: scalar-literal attributes
// (the common case) are written straight to wire, and only genuinely computed
// values are parsed with the expression parser. This is the efficient path for
// ads read from a socket. Commit semantics match Update.
func (c *Collection) UpdateOld(batch []OldAdUpdate) error {
	if len(batch) == 0 {
		return nil
	}
	codec := c.currentCodec()
	enc := c.newStreamEncoder()
	seen := make(map[uint32]struct{}, 128)
	var unesc []byte // reused string-unescape scratch (see fastString)
	byShard := make(map[int][]pendingPut, len(c.shards))
	for i := range batch {
		wireBytes, err := c.encodeOld(batch[i].Text, enc, seen, &unesc)
		if err != nil {
			return fmt.Errorf("collections: ad %d: %w", i, err)
		}
		stored := codec.Compress(nil, wireBytes)
		h := c.h.Hash(batch[i].Key)
		byShard[int(h&c.mask)] = append(byShard[int(h&c.mask)], pendingPut{hash: h, key: batch[i].Key, ad: stored, codec: codec})
	}
	for si, writes := range byShard {
		c.shards[si].commit(writes)
	}
	return c.writeError()
}

// encodeOld encodes one old-ClassAd text into wire bytes, streaming directly when
// possible and falling back to the full parser for ads with duplicate attribute
// names (which the parser dedups last-wins). enc and seen are reused across a
// batch.
func (c *Collection) encodeOld(text string, enc *wire.StreamEncoder, seen map[uint32]struct{}, unesc *[]byte) ([]byte, error) {
	enc.Reset()
	clear(seen)
	err := encodeOldText(text, enc, seen, unesc)
	if err == errDuplicate || err == errDeferOld {
		ad, e := classad.ParseOld(text)
		if e != nil {
			return nil, e
		}
		return c.encodeAd(ad.AST()), nil
	}
	if err != nil {
		return nil, err
	}
	return enc.Bytes(nil), nil
}

// encodeOldText parses old-ClassAd text (one ad) directly into enc, recording
// each attribute name in seen to detect duplicates.
func encodeOldText(text string, enc *wire.StreamEncoder, seen map[uint32]struct{}, unesc *[]byte) error {
	for len(text) > 0 {
		var line string
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			line, text = text[:nl], text[nl+1:]
		} else {
			line, text = text, ""
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return fmt.Errorf("malformed line %q", line)
		}
		name := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if !isBareName(name) {
			// Quoted/odd attribute names are rare in old-form ads; defer the whole
			// assignment to the full parser, which unquotes the name correctly. But
			// ParseClassAd is new-ClassAd (strict escapes), so if the line has a backslash
			// its string lexing could diverge from ParseOld -- defer the whole ad instead.
			if strings.IndexByte(line, '\\') >= 0 {
				return errDeferOld
			}
			ad, err := parser.ParseClassAd("[" + line + "]")
			if err != nil || ad == nil || len(ad.Attributes) != 1 {
				return fmt.Errorf("malformed assignment %q", line)
			}
			if !markSeen(seen, ad.Attributes[0].Name) {
				return errDuplicate
			}
			enc.Expr(ad.Attributes[0].Name, ad.Attributes[0].Value)
			continue
		}
		if !markSeen(seen, name) {
			return errDuplicate
		}
		if fastScalar(enc, name, val) {
			continue
		}
		if fastString(enc, name, val, unesc) {
			continue
		}
		// Not a scalar or a lone string literal. If it contains a backslash, an embedded
		// string literal could lex differently under old-ClassAd rules (literal escapes) than
		// the fast expression path's new-ClassAd lexer, so defer the whole ad to ParseOld.
		// Backslash-free values (the overwhelming majority -- Requirements, Rank, etc.) lex
		// identically, so they keep the fast path.
		if strings.IndexByte(val, '\\') >= 0 {
			return errDeferOld
		}
		// Computed value: parse the expression straight to wire (no ast.Expr). On any
		// error -- a construct the native parser does not handle, or malformed text --
		// fall back to the reference parser, which is authoritative for both the
		// encoding and the error message. ExprWire leaves no partial entry on error.
		if err := enc.ExprWire(name, val); err == nil {
			continue
		}
		expr, err := parser.ParseExpr(val)
		if err != nil {
			return fmt.Errorf("value of %q: %w", name, err)
		}
		enc.Expr(name, expr)
	}
	return nil
}

// fastScalar writes name=val directly to enc when val is unambiguously a scalar
// literal that parses identically to the expression parser, returning true. It is
// deliberately conservative: anything uncertain returns false so the caller falls
// back to the parser (which is always correct).
func fastScalar(enc *wire.StreamEncoder, name, val string) bool {
	if val == "" {
		return false
	}
	switch {
	case strings.EqualFold(val, "true"):
		enc.Bool(name, true)
		return true
	case strings.EqualFold(val, "false"):
		enc.Bool(name, false)
		return true
	case strings.EqualFold(val, "undefined"):
		enc.Undefined(name)
		return true
	case strings.EqualFold(val, "error"):
		enc.Error(name)
		return true
	}
	switch classifyNumberOrString(val) {
	case numInt:
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			enc.Int(name, i)
			return true
		}
	case numReal:
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			enc.Real(name, f)
			return true
		}
	case simpleStr:
		enc.String(name, val[1:len(val)-1])
		return true
	}
	return false
}

// fastString handles a value that is exactly one double-quoted OLD-ClassAd string
// literal, copying its content into *unesc byte-for-byte as the reference old-ClassAd
// tokenizer (C++ Lexer::tokenizeStringOld, mirrored by classad.ParseOld) does, and
// emitting a string node -- avoiding the full expression parser for what, on real ads,
// is the largest and most common non-scalar value (AddressV1, OSIssue, and other
// strings). Old-ClassAd string lexing does NO escape interpretation: a backslash is
// kept literal (so OSIssue = "\S", the agetty escapes /etc/issue carries, ingests
// instead of being rejected), EXCEPT immediately before the closing quote where it
// escapes it (\" -> a literal quote, the string continues). It returns false, deferring
// to the parser, only for a value that is not a lone string literal or is unterminated;
// non-ASCII bytes are decoded to runes exactly as the lexer's scanString does (an invalid
// byte becomes U+FFFD), so a lone string literal is ALWAYS handled here and never reaches the
// strict expression fallback (which would octal-interpret \1 and diverge from ParseOld).
//
// Keeping this byte-identical to ParseOld is the invariant TestUpdateOldMatchesParseOld
// and FuzzIngestOld enforce; the escape handling here must therefore track the lexer's
// lenientEscapes path exactly.
//
// Escape-free plain-ASCII strings are already handled by fastScalar (simpleStr)
// without a copy; this covers the with-backslash and non-ASCII cases.
func fastString(enc *wire.StreamEncoder, name, val string, unesc *[]byte) bool {
	if len(val) < 2 || val[0] != '"' {
		return false
	}
	buf := (*unesc)[:0]
	for i := 1; i < len(val); {
		r, size := utf8.DecodeRuneInString(val[i:])
		switch r {
		case '"':
			if i != len(val)-1 {
				return false // content after the closing quote: not a lone string literal
			}
			*unesc = buf
			enc.StringBytes(name, buf)
			return true
		case '\\':
			// Literal backslash, unless it immediately precedes the closing quote, where
			// it escapes it (a lone trailing backslash therefore leaves the string
			// unterminated, exactly as in tokenizeStringOld).
			if i+1 < len(val) && val[i+1] == '"' {
				buf = append(buf, '"')
				i += 2
			} else {
				buf = append(buf, '\\')
				i++
			}
		default:
			buf = utf8.AppendRune(buf, r) // invalid byte -> U+FFFD, matching scanString
			i += size
		}
	}
	return false // no closing quote
}

type valClass int

const (
	notScalar valClass = iota
	numInt
	numReal
	simpleStr
)

// classifyNumberOrString conservatively recognizes non-negative integer/real
// literals and escape-free double-quoted strings. Signed numbers are excluded
// (the parser renders a leading -/+ as a unary operator, not a literal), as are
// integers with a leading zero (octal ambiguity) and strings containing a
// backslash or an inner quote (which need real unescaping).
func classifyNumberOrString(v string) valClass {
	if v[0] == '"' {
		if len(v) >= 2 && v[len(v)-1] == '"' && isPlainASCII(v[1:len(v)-1]) {
			return simpleStr
		}
		return notScalar
	}
	if v[0] < '0' || v[0] > '9' {
		return notScalar
	}
	hasDot, hasExp, leadingZero := false, false, v[0] == '0' && len(v) > 1
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && !hasDot && !hasExp:
			hasDot = true
		case (c == 'e' || c == 'E') && !hasExp && i > 0:
			hasExp = true
			if i+1 < len(v) && (v[i+1] == '+' || v[i+1] == '-') {
				i++
			}
		default:
			return notScalar
		}
	}
	if hasDot || hasExp {
		return numReal // e.g. 0.5 is fine; leading zero only matters for ints
	}
	if leadingZero {
		return notScalar // ambiguous octal-looking integer -> let the parser decide
	}
	return numInt
}

// markSeen records a case-insensitive hash of the name and returns false if a
// fold-equal name was already present (a duplicate attribute). Hashing instead of
// keying the map on strings.ToLower(name) avoids allocating a lowercased copy for
// every attribute -- which, on the concurrent ingest path, was ~10% of CPU and a
// meaningful source of memory-allocator lock contention. A hash collision between
// two distinct names in one ad merely reports a false duplicate, which routes the
// ad to the parser fallback (encodeOld) -- correct, just slightly slower -- so
// collisions never cost correctness, only a rare slow path.
func markSeen(seen map[uint32]struct{}, name string) bool {
	h := foldHash(name)
	if _, dup := seen[h]; dup {
		return false
	}
	seen[h] = struct{}{}
	return true
}

// foldHash is an allocation-free case-insensitive FNV-1a hash over a name's bytes
// (ASCII-folded), matching ClassAd attribute-name case-insensitivity.
func foldHash(name string) uint32 {
	const off, prime = 2166136261, 16777619
	h := uint32(off)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		h = (h ^ uint32(c)) * prime
	}
	return h
}

// isPlainASCII reports whether s consists only of printable ASCII (0x20–0x7e),
// excluding backslash and double-quote. Such a string's bytes are its value
// verbatim; anything else (escapes, control bytes, non-ASCII/UTF-8) is deferred to
// the lexer, whose rune/escape handling the fast path must not second-guess.
func isPlainASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '\\' || c == '"' {
			return false
		}
	}
	return true
}

// isBareName reports whether name is a bare ClassAd identifier (letter/underscore
// then letters/digits/underscores), the case where interning the raw text is
// correct without unquoting.
func isBareName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return len(name) > 0
}
