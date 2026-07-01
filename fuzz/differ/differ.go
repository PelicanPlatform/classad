//go:build libclassad

// Package differ runs one ClassAd through both evaluation engines (the native
// Go implementation and the reference C++ libclassad via cgo) and classifies
// any divergence between them.
package differ

import (
	"fmt"
	"time"

	classad "github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/fuzz/canon"
	cppengine "github.com/PelicanPlatform/classad/fuzz/oracle/cgo"
)

// Category labels the kind of divergence found.
type Category int

const (
	// Match: both engines parsed and produced equal canonical results (or both
	// rejected the input at parse time).
	Match Category = iota
	// ParseDivergence: exactly one engine accepted the input at parse time.
	ParseDivergence
	// ValueDivergence: both parsed, but at least one attribute evaluated to a
	// different canonical value.
	ValueDivergence
	// EncodingError: an engine produced output that could not be decoded
	// (indicates a bug in the harness or a wildly unexpected value).
	EncodingError
	// GoPanic: the Go engine panicked while parsing or evaluating. Always a bug
	// in the Go implementation (it should return error/undefined, never panic).
	GoPanic
	// CppTimeout: libclassad failed to terminate (it infinite-loops on some
	// cyclic self-references that the Go engine resolves to error -- a C++ bug).
	// The result is uncomparable, so this is treated as a non-divergence.
	CppTimeout
	// KnownQuirk: the engines disagree, but entirely because of a documented
	// libclassad quirk the Go engine deliberately does not mirror (see
	// fuzz/CPP_QUIRKS.md). Treated as a non-divergence.
	KnownQuirk
)

// cppEvalTimeout bounds a single libclassad evaluation. Normal evaluation takes
// microseconds; only a cyclic-evaluation hang reaches this.
const cppEvalTimeout = 2 * time.Second

func (c Category) String() string {
	switch c {
	case Match:
		return "match"
	case ParseDivergence:
		return "parse-divergence"
	case ValueDivergence:
		return "value-divergence"
	case EncodingError:
		return "encoding-error"
	case GoPanic:
		return "go-panic"
	case CppTimeout:
		return "cpp-timeout"
	case KnownQuirk:
		return "known-quirk"
	default:
		return "unknown"
	}
}

// Result is the outcome of comparing the two engines on one input.
type Result struct {
	Category  Category
	GoParsed  bool
	CppParsed bool
	GoCanon   canon.Value
	CppCanon  canon.Value
	GoRaw     string // canonical encoding string from the Go engine
	CppRaw    string // canonical encoding string from the C++ engine
	GoErr     error  // Go parse error, if any
	Detail    string // human-readable description of the first divergence
}

// IsDivergence reports whether the result is anything other than a clean match.
// A CppTimeout is not a divergence: libclassad hung (a C++ bug) so the result
// is uncomparable.
func (r Result) IsDivergence() bool {
	return r.Category != Match && r.Category != CppTimeout && r.Category != KnownQuirk
}

// Options tunes comparison behavior.
type Options struct {
	Tol canon.FloatTolerance
	// IgnoreParseDivergence suppresses reporting when only one engine parses.
	// Useful when focusing purely on evaluation semantics, since the two
	// parsers have known grammar differences that would otherwise dominate.
	IgnoreParseDivergence bool
}

// DefaultOptions is the standard comparison configuration.
func DefaultOptions() Options {
	return Options{Tol: canon.DefaultTolerance}
}

// goEval parses and evaluates src with the native Go engine.
func goEval(src string) (val canon.Value, raw string, parsed bool, err error) {
	ad, perr := classad.Parse(src)
	if perr != nil {
		return canon.Value{}, "", false, perr
	}
	v := canon.FromGoClassAd(ad)
	return v, canon.Encode(v), true, nil
}

// goEvalSafe wraps goEval, converting a panic in the Go engine into a reported
// finding rather than crashing the fuzzer.
func goEvalSafe(src string) (val canon.Value, raw string, parsed bool, panicked string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = fmt.Sprintf("%v", rec)
		}
	}()
	val, raw, parsed, err = goEval(src)
	return
}

// Compare evaluates src in both engines and classifies the outcome.
func Compare(src string, opts Options) Result {
	var r Result

	goVal, goRaw, goParsed, goPanic, goErr := goEvalSafe(src)
	r.GoParsed = goParsed
	r.GoCanon = goVal
	r.GoRaw = goRaw
	r.GoErr = goErr
	if goPanic != "" {
		r.Category = GoPanic
		r.Detail = "go engine panicked: " + goPanic
		return r
	}

	cppRaw, cppParsed, cppTimedOut := cppengine.EvalAdTimeout(src, cppEvalTimeout)
	if cppTimedOut {
		// libclassad infinite-looped (a known C++ bug on some cyclic
		// self-references the Go engine resolves to error). The result is
		// uncomparable, not a Go divergence; report it so it can be surfaced.
		r.Category = CppTimeout
		r.Detail = "cpp engine timed out (libclassad cyclic-evaluation hang)"
		return r
	}
	r.CppParsed = cppParsed
	r.CppRaw = cppRaw

	// Parse-level agreement first.
	if goParsed != cppParsed {
		r.Category = ParseDivergence
		if opts.IgnoreParseDivergence {
			r.Category = Match
		}
		r.Detail = fmt.Sprintf("go parsed=%v, cpp parsed=%v", goParsed, cppParsed)
		return r
	}
	if !goParsed && !cppParsed {
		r.Category = Match // both reject: not interesting
		return r
	}

	// Both parsed: decode the C++ side and compare structurally.
	cppVal, derr := canon.Parse(cppRaw)
	if derr != nil {
		r.Category = EncodingError
		r.Detail = "failed to decode cpp canonical output: " + derr.Error()
		return r
	}
	r.CppCanon = cppVal

	// Fast path: identical encodings.
	if goRaw == cppRaw {
		r.Category = Match
		return r
	}

	// Slow path: structural compare with float tolerance.
	if canon.Equal(goVal, cppVal, opts.Tol) {
		r.Category = Match
		return r
	}

	// A divergence explained entirely by a documented libclassad quirk the Go
	// engine does not mirror is not a real divergence.
	if explainedByListIsQuirk(src, goVal, cppVal) {
		r.Category = KnownQuirk
		r.Detail = "CPP_QUIRKS #9: =?=/=!= on a list literal vs a function-produced list"
		return r
	}

	r.Category = ValueDivergence
	r.Detail = firstAttrDiff(goVal, cppVal, opts.Tol)
	return r
}

// firstAttrDiff finds the first differing top-level attribute for a readable
// report. Both values are expected to be classad values.
func firstAttrDiff(g, c canon.Value, tol canon.FloatTolerance) string {
	if g.Kind != canon.KClassad || c.Kind != canon.KClassad {
		return fmt.Sprintf("go=%s cpp=%s", canon.Describe(g), canon.Describe(c))
	}
	gm := map[string]canon.Value{}
	for _, kv := range g.Map {
		gm[kv.Key] = kv.Val
	}
	cm := map[string]canon.Value{}
	for _, kv := range c.Map {
		cm[kv.Key] = kv.Val
	}
	// Attributes present in one but not the other.
	for _, kv := range g.Map {
		if _, ok := cm[kv.Key]; !ok {
			return fmt.Sprintf("attr %q: go=%s cpp=<absent>", kv.Key, canon.Describe(kv.Val))
		}
	}
	for _, kv := range c.Map {
		if _, ok := gm[kv.Key]; !ok {
			return fmt.Sprintf("attr %q: go=<absent> cpp=%s", kv.Key, canon.Describe(kv.Val))
		}
	}
	// Attributes present in both but differing.
	for _, kv := range g.Map {
		cv := cm[kv.Key]
		if !canon.Equal(kv.Val, cv, tol) {
			return fmt.Sprintf("attr %q: go=%s cpp=%s", kv.Key, canon.Describe(kv.Val), canon.Describe(cv))
		}
	}
	return "structures differ but no single attribute isolated"
}
