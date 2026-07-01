// Package canon defines a canonical, engine-independent encoding for the
// result of evaluating a ClassAd attribute. Both evaluation engines under test
// (the native Go implementation and the reference C++ libclassad, reached via
// cgo) encode their results into this format so the differential fuzzer can
// compare them without being fooled by cosmetic formatting differences
// (e.g. Go prints reals as "0.5" while libclassad prints "5.000...E-01", and
// lists as "[1 2 3]" vs "{ 1,2,3 }").
//
// The encoding is a single self-delimiting line. Strings and composite values
// are length/count prefixed so no escaping is required and parsing is
// unambiguous:
//
//	U                      undefined
//	E                      error
//	B0 | B1                boolean
//	I<int64>;              integer (decimal)
//	R<%.17g>;              real (round-trippable double)
//	G<%.17g>;              relative time, seconds
//	A<%.17g>,<offset>;     absolute time: epoch seconds, tz offset seconds
//	S<len>,<bytes>         string: byte length then raw bytes
//	L<count>,<elem>...     list: element count then encoded elements
//	C<count>,<klen>,<key><val>...  classad: entry count then (keylen,key,value),
//	                                entries sorted by attribute name
//
// The C++ shim (fuzz/oracle/cgo/shim.cc) emits byte-identical output for the
// scalar cases, so the common path is a plain string compare. When strings
// differ, callers re-parse both sides into a Value tree and compare with a
// float tolerance to suppress last-ULP noise from transcendental functions.
package canon

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kind enumerates the canonical value types. The set is the union of what both
// engines can produce; an engine that cannot represent a kind (e.g. the Go
// engine historically had no absolute-time value) simply never emits it, which
// surfaces as a divergence.
type Kind int

const (
	KUndef Kind = iota
	KError
	KBool
	KInt
	KReal
	KReltime
	KAbstime
	KString
	KList
	KClassad
)

func (k Kind) String() string {
	switch k {
	case KUndef:
		return "undefined"
	case KError:
		return "error"
	case KBool:
		return "bool"
	case KInt:
		return "int"
	case KReal:
		return "real"
	case KReltime:
		return "reltime"
	case KAbstime:
		return "abstime"
	case KString:
		return "string"
	case KList:
		return "list"
	case KClassad:
		return "classad"
	default:
		return "unknown"
	}
}

// Value is the parsed form of a canonical encoding.
type Value struct {
	Kind Kind
	B    bool
	I    int64
	R    float64
	Off  int64 // tz offset (seconds) for KAbstime
	S    string
	List []Value
	Map  []Attr // sorted by Key for KClassad
}

// Attr is one attribute of a canonical classad value.
type Attr struct {
	Key string
	Val Value
}

// Encode serializes v into the canonical line format.
func Encode(v Value) string {
	var b strings.Builder
	encode(&b, v)
	return b.String()
}

func encode(b *strings.Builder, v Value) {
	switch v.Kind {
	case KUndef:
		b.WriteByte('U')
	case KError:
		b.WriteByte('E')
	case KBool:
		if v.B {
			b.WriteString("B1")
		} else {
			b.WriteString("B0")
		}
	case KInt:
		b.WriteByte('I')
		b.WriteString(strconv.FormatInt(v.I, 10))
		b.WriteByte(';')
	case KReal:
		b.WriteByte('R')
		b.WriteString(formatReal(v.R))
		b.WriteByte(';')
	case KReltime:
		b.WriteByte('G')
		b.WriteString(formatReal(v.R))
		b.WriteByte(';')
	case KAbstime:
		b.WriteByte('A')
		b.WriteString(formatReal(v.R))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(v.Off, 10))
		b.WriteByte(';')
	case KString:
		b.WriteByte('S')
		b.WriteString(strconv.Itoa(len(v.S)))
		b.WriteByte(',')
		b.WriteString(v.S)
	case KList:
		b.WriteByte('L')
		b.WriteString(strconv.Itoa(len(v.List)))
		b.WriteByte(',')
		for _, e := range v.List {
			encode(b, e)
		}
	case KClassad:
		b.WriteByte('C')
		b.WriteString(strconv.Itoa(len(v.Map)))
		b.WriteByte(',')
		for _, kv := range v.Map {
			b.WriteString(strconv.Itoa(len(kv.Key)))
			b.WriteByte(',')
			b.WriteString(kv.Key)
			encode(b, kv.Val)
		}
	default:
		b.WriteByte('?')
	}
}

// formatReal renders a float64 round-trippably and canonicalizes the special
// values so the two engines agree on spelling.
func formatReal(r float64) string {
	switch {
	case math.IsNaN(r):
		return "nan"
	case math.IsInf(r, 1):
		return "inf"
	case math.IsInf(r, -1):
		return "-inf"
	default:
		return strconv.FormatFloat(r, 'g', 17, 64)
	}
}

// Parse decodes a canonical encoding produced by either engine.
func Parse(s string) (Value, error) {
	p := &parser{s: s}
	v, err := p.value()
	if err != nil {
		return Value{}, err
	}
	if p.pos != len(s) {
		return Value{}, fmt.Errorf("canon: trailing data at %d in %q", p.pos, s)
	}
	return v, nil
}

type parser struct {
	s   string
	pos int
}

func (p *parser) value() (Value, error) {
	if p.pos >= len(p.s) {
		return Value{}, fmt.Errorf("canon: unexpected end of input")
	}
	tag := p.s[p.pos]
	p.pos++
	switch tag {
	case 'U':
		return Value{Kind: KUndef}, nil
	case 'E':
		return Value{Kind: KError}, nil
	case 'B':
		bit, err := p.byteVal()
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KBool, B: bit == '1'}, nil
	case 'I':
		n, err := p.intField(';')
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KInt, I: n}, nil
	case 'R':
		r, err := p.realField(';')
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KReal, R: r}, nil
	case 'G':
		r, err := p.realField(';')
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KReltime, R: r}, nil
	case 'A':
		r, err := p.realField(',')
		if err != nil {
			return Value{}, err
		}
		off, err := p.intField(';')
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KAbstime, R: r, Off: off}, nil
	case 'S':
		n, err := p.intField(',')
		if err != nil {
			return Value{}, err
		}
		s, err := p.take(int(n))
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KString, S: s}, nil
	case 'L':
		n, err := p.intField(',')
		if err != nil {
			return Value{}, err
		}
		list := make([]Value, 0, n)
		for i := int64(0); i < n; i++ {
			e, err := p.value()
			if err != nil {
				return Value{}, err
			}
			list = append(list, e)
		}
		return Value{Kind: KList, List: list}, nil
	case 'C':
		n, err := p.intField(',')
		if err != nil {
			return Value{}, err
		}
		m := make([]Attr, 0, n)
		for i := int64(0); i < n; i++ {
			klen, err := p.intField(',')
			if err != nil {
				return Value{}, err
			}
			key, err := p.take(int(klen))
			if err != nil {
				return Value{}, err
			}
			v, err := p.value()
			if err != nil {
				return Value{}, err
			}
			m = append(m, Attr{Key: key, Val: v})
		}
		return Value{Kind: KClassad, Map: m}, nil
	default:
		return Value{}, fmt.Errorf("canon: unknown tag %q", tag)
	}
}

func (p *parser) byteVal() (byte, error) {
	if p.pos >= len(p.s) {
		return 0, fmt.Errorf("canon: unexpected end of input")
	}
	b := p.s[p.pos]
	p.pos++
	return b, nil
}

func (p *parser) field(delim byte) (string, error) {
	i := strings.IndexByte(p.s[p.pos:], delim)
	if i < 0 {
		return "", fmt.Errorf("canon: missing %q delimiter", delim)
	}
	f := p.s[p.pos : p.pos+i]
	p.pos += i + 1
	return f, nil
}

func (p *parser) intField(delim byte) (int64, error) {
	f, err := p.field(delim)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(f, 10, 64)
}

func (p *parser) realField(delim byte) (float64, error) {
	f, err := p.field(delim)
	if err != nil {
		return 0, err
	}
	switch f {
	case "nan":
		return math.NaN(), nil
	case "inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(f, 64)
}

func (p *parser) take(n int) (string, error) {
	if n < 0 || p.pos+n > len(p.s) {
		return "", fmt.Errorf("canon: string length %d out of range", n)
	}
	s := p.s[p.pos : p.pos+n]
	p.pos += n
	return s, nil
}
