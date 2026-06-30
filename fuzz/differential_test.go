// Package fuzz hosts the coverage-guided differential fuzz target. It is a thin
// wrapper around the differ: Go's native fuzzing engine mutates ClassAd source
// text, and each input is evaluated in both the Go engine and the reference C++
// libclassad (in-process via cgo). A divergence in evaluation results, or a
// panic in the Go engine, fails the test and the input is saved to the corpus.
//
// Run:
//
//	go test ./fuzz -run=xxx -fuzz=FuzzDifferential
//
// Reproduce a saved crash:
//
//	go test ./fuzz -run=FuzzDifferential/<hash>
//
// Coverage instrumentation guides exploration of the Go parser/evaluator; the
// cgo C++ side is not instrumented, which is fine — the goal is to explore the
// Go engine's behavior space and compare it against the reference.
package fuzz

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/fuzz/differ"
)

// curated seeds covering operator, coercion, function, and undefined/error
// edge cases. Mutation fans out from these into nearby parseable inputs.
var seeds = []string{
	`[ a = 1 + 2 ]`,
	`[ a = 1 / 2 ]`,
	`[ a = 1.0 / 2.0 ]`,
	`[ a = 7 % 3 ]`,
	`[ a = 2 ** 8 ]`,
	`[ a = -5; b = a * a ]`,
	`[ a = 1 < 2; b = 2 <= 2; c = 3 > 4 ]`,
	`[ a = 1 == 1.0; b = "x" == "x"; c = true == 1 ]`,
	`[ a = undefined + 1; b = error + 1; c = undefined == undefined ]`,
	`[ a = undefined =?= undefined; b = error =?= error; c = 1 =?= 1.0 ]`,
	`[ a = true && undefined; b = false || undefined; c = !undefined ]`,
	`[ a = "hello"; b = strcat(a, " world"); c = toUpper(a); d = size(a) ]`,
	`[ a = {1, 2, 3}; b = a[1]; c = size(a); d = member(2, a) ]`,
	`[ a = {1, "x", true, 2.5}; b = a[5] ]`,
	`[ a = floor(2.7); b = ceiling(2.1); c = round(2.5); d = round(3.5) ]`,
	`[ a = int("42"); b = real("3.14"); c = int("nope"); d = string(42) ]`,
	`[ a = ifThenElse(true, 1, 2); b = ifThenElse(undefined, 1, 2) ]`,
	`[ a = pow(2, 10); b = pow(2, -1); c = pow(2.0, 0.5) ]`,
	`[ a = 5; result = a > 3 ? "big" : "small" ]`,
	`[ n = [x = 1 + 1; y = "z"]; m = n.x ]`,
	`[ a = 10000000000000000000 ]`,
	`[ a = 0.1 + 0.2 ]`,
	`[ a = substr("hello", 1, 3); b = substr("hello", -2) ]`,
	`[ a = strcmp("abc", "abd"); b = stricmp("ABC", "abc") ]`,
	`[ a = "a,b,c"; b = split(a) ]`,
	`[ a = 1 =!= 2; b = "x" =!= "y" ]`,
	`[ a = quantize(7, 5); b = quantize(7, {2, 4, 8}) ]`,
}

// seedOpts is shared by seeding and the fuzz body.
var seedOpts = func() differ.Options {
	o := differ.DefaultOptions()
	// Parse-divergences stem from known grammar differences between the two
	// parsers and would otherwise swamp the corpus; this target focuses on
	// evaluation semantics. Run `cafuzz -corpus` to survey parse divergences.
	o.IgnoreParseDivergence = true
	return o
}()

// addIfMatch seeds the fuzzer only with inputs the two engines currently agree
// on. Go's native fuzzing aborts on a failing seed before it ever mutates, so
// seeding from a matching baseline lets mutation actually explore; known
// divergences are surfaced by `cafuzz`, not by the seed set.
func addIfMatch(f *testing.F, src string) {
	if !differ.Compare(src, seedOpts).IsDivergence() {
		f.Add(src)
	}
}

func loadCorpus(f *testing.F) {
	file, err := os.Open("corpus/seeds.txt")
	if err != nil {
		return
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		addIfMatch(f, line)
	}
}

func FuzzDifferential(f *testing.F) {
	for _, s := range seeds {
		addIfMatch(f, s)
	}
	loadCorpus(f)

	opts := seedOpts

	f.Fuzz(func(t *testing.T, src string) {
		// ClassAd source is text. Skip non-UTF-8 mutations: the Go lexer
		// decodes string literals as UTF-8 (so an invalid byte like 0xa2
		// becomes the 3-byte U+FFFD) while libclassad keeps raw bytes -- a
		// known, niche byte-vs-rune divergence in string-literal scanning that
		// is out of scope for evaluation-semantics fuzzing (see README).
		if !utf8.ValidString(src) {
			t.Skip()
		}
		r := differ.Compare(src, opts)
		switch r.Category {
		case differ.GoPanic:
			t.Fatalf("Go engine panicked\n  input: %s\n  %s", src, r.Detail)
		case differ.ValueDivergence:
			t.Fatalf("evaluation divergence\n  input: %s\n  %s\n  go : %s\n  cpp: %s",
				src, r.Detail, r.GoRaw, r.CppRaw)
		case differ.EncodingError:
			t.Fatalf("canonical decode error\n  input: %s\n  %s", src, r.Detail)
		}
	})
}
