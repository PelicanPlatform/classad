package cgo

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/fuzz/canon"
)

// TestCppQuirks pins the reference libclassad behavior for the surprising cases
// catalogued in fuzz/CPP_QUIRKS.md that the Go engine intentionally MIRRORS.
//
// Unlike the pure-Go parity tests in classad/cpp_parity_test.go (which only
// record what the Go engine should produce), this test runs each quirk input
// through BOTH engines:
//
//   - cppWant pins libclassad's current result. If a future libclassad release
//     changes the behavior -- e.g. decides one of these quirks is a bug and
//     fixes it -- the C++ assertion fails, which is our signal to revisit the
//     quirk, the Go mirror, and CPP_QUIRKS.md.
//   - It then asserts the Go engine still matches libclassad, so the mirror
//     cannot silently drift from the reference.
//
// This test requires CGO_ENABLED=1 (the whole package links libclassad).
func TestCppQuirks(t *testing.T) {
	cases := []struct {
		expr    string // evaluated as [ x = <expr> ]
		cppWant string // canon.Describe of libclassad's current value for x
	}{
		// A non-empty, all-delimiter string is treated as a non-empty (and
		// non-matchable) list by stringListSubsetMatch, even though
		// stringListSize reports it as empty and an empty string is the empty
		// subset. (Internally inconsistent in libclassad.)
		{`stringListSubsetMatch(" ", "a")`, "false"},
		{`stringListSubsetMatch("  ", "a")`, "false"},
		{`stringListSubsetMatch(",", "a")`, "false"},
		{`stringListSubsetMatch("", "a")`, "true"},
		{`stringListSubsetMatch(" a", "a")`, "true"},
		{`stringListSize(" ")`, "int(0)"},
	}

	for _, tc := range cases {
		src := "[ x = " + tc.expr + " ]"

		cppVal, parsed, err := Eval(src)
		if err != nil {
			t.Errorf("%s: C++ decode error: %v", tc.expr, err)
			continue
		}
		if !parsed {
			t.Errorf("%s: libclassad failed to parse", tc.expr)
			continue
		}
		cppX, ok := attr(cppVal, "x")
		if !ok {
			t.Errorf("%s: C++ result has no attribute x: %s", tc.expr, canon.Describe(cppVal))
			continue
		}
		if got := canon.Describe(cppX); got != tc.cppWant {
			t.Errorf("%s: libclassad now returns %s, recorded quirk was %s -- "+
				"revisit the Go mirror and fuzz/CPP_QUIRKS.md", tc.expr, got, tc.cppWant)
			// Keep going to also report the Go side.
		}

		// The Go engine must mirror whatever libclassad currently does.
		ad, perr := classad.Parse(src)
		if perr != nil {
			t.Errorf("%s: Go failed to parse: %v", tc.expr, perr)
			continue
		}
		goX, ok := attr(canon.FromGoClassAd(ad), "x")
		if !ok {
			t.Errorf("%s: Go result has no attribute x", tc.expr)
			continue
		}
		if !canon.Equal(goX, cppX, canon.FloatTolerance{}) {
			t.Errorf("%s: Go=%s C++=%s -- the mirror diverged from the reference",
				tc.expr, canon.Describe(goX), canon.Describe(cppX))
		}
	}
}

// attr returns the value of attribute key in a canonical classad value.
func attr(v canon.Value, key string) (canon.Value, bool) {
	if v.Kind != canon.KClassad {
		return canon.Value{}, false
	}
	for _, kv := range v.Map {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return canon.Value{}, false
}
