// Package gen produces random but valid ClassAd source text for differential
// fuzzing. Purely random bytes almost never parse as a ClassAd, so a grammar
// directed generator is what actually exercises the evaluators. Generation is
// deterministic given a seed, so any divergence the driver finds can be
// reproduced from its seed alone.
//
// The generator deliberately avoids nondeterministic builtins (random, time,
// currentTime, ...) so that a divergence reflects an engine difference rather
// than wall-clock or RNG state. It also stays within types both engines can
// represent.
package gen

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
)

// Config bounds the shape of generated ClassAds.
type Config struct {
	MaxAttrs   int // maximum number of top-level attributes
	MaxDepth   int // maximum expression nesting depth
	MaxListLen int // maximum elements in a generated list
	// AllowNonDeterministic, if set, permits time/random builtins. Off by
	// default because they make differential results meaningless.
	AllowNonDeterministic bool
}

// DefaultConfig is a reasonable default for broad coverage.
func DefaultConfig() Config {
	return Config{MaxAttrs: 6, MaxDepth: 4, MaxListLen: 4}
}

// Generator emits ClassAds from a seeded PRNG.
type Generator struct {
	cfg   Config
	rng   *rand.Rand
	attrs []string // names of attributes already emitted (for references)
}

// New returns a generator seeded deterministically.
func New(seed int64, cfg Config) *Generator {
	return &Generator{cfg: cfg, rng: rand.New(rand.NewSource(seed))}
}

// pure deterministic builtins, grouped by the arity the generator calls them
// with, that both engines implement. (Non-deterministic builtins -- time,
// random, currentTime -- are excluded; formatTime is omitted as timezone/locale
// dependent.) A function is listed in the group for an arity it accepts, even
// if it accepts a range; this exercises a representative call.
var (
	fns1 = []string{"floor", "ceiling", "round", "int", "real", "string", "bool",
		"isUndefined", "isError", "isString", "isInteger", "isReal", "isBoolean",
		"isList", "isClassAd", "toLower", "toUpper", "size", "length",
		"split", "splitUserName", "splitSlotName", "interval", "sum", "avg",
		"min", "max", "stringListSize"}
	fns2 = []string{"strcat", "pow", "quantize", "strcmp", "stricmp", "member",
		"join", "versioncmp", "identicalMember", "stringListMember",
		"stringListIMember", "stringListSize", "stringListSum", "stringListAvg",
		"stringListMin", "stringListMax", "stringListsIntersect",
		"stringListSubsetMatch"}
	fns3 = []string{"ifThenElse", "substr", "version_in_range", "anyCompare",
		"allCompare"}
	// The regex family (regexp/regexpMember/regexps/replace/stringListRegexpMember)
	// is held out of the generator on purpose: Go uses RE2 and libclassad uses
	// PCRE, so they diverge on many patterns in ways that are an engine choice,
	// not a fixable bug. unparse is held out because it requires an attribute-
	// reference argument, which the generator does not produce.
)

var binops = []string{"+", "-", "*", "/", "%", "<", "<=", ">", ">=",
	"==", "!=", "=?=", "=!=", "&&", "||",
	"&", "|", "^", "<<", ">>", ">>>"}

// ClassAd generates one ClassAd as source text (always [ ... ] form).
func (g *Generator) ClassAd() string {
	g.attrs = g.attrs[:0]
	n := 1 + g.rng.Intn(g.cfg.MaxAttrs)
	var b strings.Builder
	b.WriteString("[ ")
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("a%d", i)
		b.WriteString(name)
		b.WriteString(" = ")
		b.WriteString(g.expr(0))
		b.WriteString("; ")
		// Make the attribute available as a reference to later attributes.
		g.attrs = append(g.attrs, name)
	}
	// Always include a final attribute that aggregates earlier ones, so most
	// generated ads end with a non-trivial referencing expression.
	b.WriteString("result = ")
	b.WriteString(g.expr(0))
	b.WriteString(" ]")
	return b.String()
}

// expr generates an expression at the given nesting depth.
func (g *Generator) expr(depth int) string {
	if depth >= g.cfg.MaxDepth {
		return g.leaf()
	}
	switch g.rng.Intn(10) {
	case 0, 1, 2:
		return g.leaf()
	case 3, 4:
		// binary operation
		op := binops[g.rng.Intn(len(binops))]
		return "(" + g.expr(depth+1) + " " + op + " " + g.expr(depth+1) + ")"
	case 5:
		// unary
		if g.rng.Intn(2) == 0 {
			return "(-" + g.expr(depth+1) + ")"
		}
		return "(!" + g.expr(depth+1) + ")"
	case 6:
		// ternary
		return "(" + g.expr(depth+1) + " ? " + g.expr(depth+1) + " : " + g.expr(depth+1) + ")"
	case 7:
		return g.call(depth)
	case 8:
		return g.list(depth)
	default:
		// subscript of a list expression
		return g.list(depth) + "[" + strconv.Itoa(g.rng.Intn(g.cfg.MaxListLen+1)) + "]"
	}
}

func (g *Generator) call(depth int) string {
	switch g.rng.Intn(3) {
	case 0:
		f := fns1[g.rng.Intn(len(fns1))]
		return f + "(" + g.expr(depth+1) + ")"
	case 1:
		f := fns2[g.rng.Intn(len(fns2))]
		return f + "(" + g.expr(depth+1) + ", " + g.expr(depth+1) + ")"
	default:
		f := fns3[g.rng.Intn(len(fns3))]
		return f + "(" + g.expr(depth+1) + ", " + g.expr(depth+1) + ", " + g.expr(depth+1) + ")"
	}
}

func (g *Generator) list(depth int) string {
	n := g.rng.Intn(g.cfg.MaxListLen + 1)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = g.expr(depth + 1)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// leaf generates a terminal: a literal or an attribute reference.
func (g *Generator) leaf() string {
	// Prefer references when some attributes exist, to exercise scoping.
	if len(g.attrs) > 0 && g.rng.Intn(3) == 0 {
		return g.attrs[g.rng.Intn(len(g.attrs))]
	}
	switch g.rng.Intn(9) {
	case 0:
		return strconv.FormatInt(g.randInt(), 10)
	case 1:
		return g.randReal()
	case 2:
		return strconv.Quote(g.randString())
	case 3:
		return "true"
	case 4:
		return "false"
	case 5:
		return "undefined"
	case 6:
		return "error"
	case 7:
		// reference to a likely-undefined attribute (exercises undefined paths)
		return "x" + strconv.Itoa(g.rng.Intn(4))
	default:
		return strconv.FormatInt(g.randInt(), 10)
	}
}

// randInt returns a spread of values including edge cases.
func (g *Generator) randInt() int64 {
	switch g.rng.Intn(8) {
	case 0:
		return 0
	case 1:
		return 1
	case 2:
		return -1
	case 3:
		return math.MaxInt64
	case 4:
		return math.MinInt64
	case 5:
		return int64(g.rng.Intn(256)) - 128
	default:
		return g.rng.Int63() - (1 << 30)
	}
}

func (g *Generator) randReal() string {
	switch g.rng.Intn(7) {
	case 0:
		return "0.0"
	case 1:
		return "1.5"
	case 2:
		return "-2.25"
	case 3:
		return "3.14159265358979"
	case 4:
		return "1e10"
	case 5:
		return "1e-10"
	default:
		return strconv.FormatFloat(g.rng.NormFloat64()*1000, 'g', -1, 64)
	}
}

func (g *Generator) randString() string {
	alphabet := []string{"", "a", "abc", "Hello World", "x y", "1", "true",
		"a,b,c", "UPPER", "MiXeD", " ", "/", "@example.com",
		// numeric-looking strings exercise int()/real()/strtod parsing:
		// decimal, leading-prefix, hex (0x), hex float, and inf/nan spellings.
		"0", "-3.7", "1e10", " 5 ", "12abc", "0x1", "0X01", "0xff", "0x1p4",
		"010", "inf", "-inf", "nan", "1e30"}
	return alphabet[g.rng.Intn(len(alphabet))]
}
