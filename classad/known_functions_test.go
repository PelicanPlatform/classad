package classad

import (
	"os"
	"regexp"
	"testing"
)

// TestKnownFunctionsCoversDispatch guards that every function name handled by
// evaluateFunctionCall's dispatch switch is also listed in knownFunctions, so
// that the pre-arg-evaluation unknown-function check cannot accidentally reject
// a real builtin (which would make e.g. size(x) error). It scans the dispatch
// switch in evaluator.go for case labels.
func TestKnownFunctionsCoversDispatch(t *testing.T) {
	src, err := os.ReadFile("evaluator.go")
	if err != nil {
		t.Fatalf("read evaluator.go: %v", err)
	}
	// Isolate the dispatch switch (between the marker comment and its default).
	text := string(src)
	start := regexp.MustCompile(`Dispatch to the appropriate function`).FindStringIndex(text)
	if start == nil {
		t.Fatal("could not find dispatch switch marker")
	}
	region := text[start[1]:]
	if end := regexp.MustCompile(`\n\tdefault:`).FindStringIndex(region); end != nil {
		region = region[:end[0]]
	}
	caseRe := regexp.MustCompile(`(?m)^\s*case\s+(.+):`)
	strRe := regexp.MustCompile(`"([^"]+)"`)
	var n int
	for _, m := range caseRe.FindAllStringSubmatch(region, -1) {
		for _, s := range strRe.FindAllStringSubmatch(m[1], -1) {
			n++
			if !knownFunctions[s[1]] {
				t.Errorf("dispatch case %q is missing from knownFunctions", s[1])
			}
		}
	}
	if n == 0 {
		t.Fatal("found no dispatch cases; scanner is broken")
	}
}

// TestUnknownFunctionDoesNotEvaluateArgs guards that an unknown function is an
// error without evaluating its arguments, so a cyclic argument does not turn
// the surrounding =!= comparison into error: 0 =!= A((A0)) is true.
func TestUnknownFunctionDoesNotEvaluateArgs(t *testing.T) {
	ad, err := Parse(`[ A0 = 0 =!= A((A0)) ]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v := ad.EvaluateAttr("A0")
	if got, gerr := v.BoolValue(); gerr != nil || got != true {
		t.Errorf("A0 = %v, want true", v)
	}
}
