package collections

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/parser"
)

// errDuplicate signals that an ad has a repeated attribute name, where streaming
// (first-wins on lookup) would diverge from the parser (last-wins). The ad is
// re-encoded via the full parser, which dedups exactly.
var errDuplicate = errors.New("duplicate attribute")

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
	enc := wire.NewStreamEncoder(c.intern, c.currentHotSet())
	seen := make(map[string]struct{}, 128)
	byShard := make(map[int][]pendingPut, len(c.shards))
	for i := range batch {
		wireBytes, err := c.encodeOld(batch[i].Text, enc, seen)
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
func (c *Collection) encodeOld(text string, enc *wire.StreamEncoder, seen map[string]struct{}) ([]byte, error) {
	enc.Reset()
	clear(seen)
	err := encodeOldText(text, enc, seen)
	if err == errDuplicate {
		ad, e := classad.ParseOld(text)
		if e != nil {
			return nil, e
		}
		return wire.EncodeWithHot(nil, ad.AST(), c.intern, c.currentHotSet()), nil
	}
	if err != nil {
		return nil, err
	}
	return enc.Bytes(nil), nil
}

// encodeOldText parses old-ClassAd text (one ad) directly into enc, recording
// each attribute name in seen to detect duplicates.
func encodeOldText(text string, enc *wire.StreamEncoder, seen map[string]struct{}) error {
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
			// assignment to the full parser, which unquotes the name correctly.
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

// markSeen records the case-folded name and returns false if it was already
// present (a duplicate attribute).
func markSeen(seen map[string]struct{}, name string) bool {
	fold := strings.ToLower(name)
	if _, dup := seen[fold]; dup {
		return false
	}
	seen[fold] = struct{}{}
	return true
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
