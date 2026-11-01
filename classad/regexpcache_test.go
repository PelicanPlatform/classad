package classad

import (
	"fmt"
	"sync"
	"testing"
)

// evalRe evaluates src against a fixed ad.
func evalRe(t *testing.T, src string) string {
	t.Helper()
	ad, err := Parse(`[ s = "user3"; U = "USER3" ]`)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return expr.Eval(ad).String()
}

// TestRegexpCacheKeyIncludesFlags is the correctness risk a pattern cache actually has: keying on
// the user's pattern and handling flags separately would make two calls that differ ONLY in case
// sensitivity share one compiled expression, and the second caller would silently get the first
// one's semantics.
//
// Both orders are checked, because a cache that collides gives the wrong answer to whichever call
// arrives second -- so running only one order would pass half the time.
func TestRegexpCacheKeyIncludesFlags(t *testing.T) {
	for _, order := range [][]struct{ src, want string }{
		{{`regexp("^USER", s)`, "false"}, {`regexp("^USER", s, "i")`, "true"}},
		{{`regexp("^USER", s, "i")`, "true"}, {`regexp("^USER", s)`, "false"}},
	} {
		// A pattern unique to this subtest, so the two orders do not share cache entries.
		for i, tc := range order {
			if got := evalRe(t, tc.src); got != tc.want {
				t.Errorf("call %d of %q = %s, want %s", i, tc.src, got, tc.want)
			}
		}
	}
	// The same, for every built-in that takes flags, so a new one added without the flags in its key
	// is caught here rather than in production.
	for _, tc := range []struct{ src, want string }{
		{`regexpMember("^USER", {s})`, "false"},
		{`regexpMember("^USER", {s}, "i")`, "true"},
		{`replace("^USER", s, "x")`, `"user3"`},
		{`replace("^USER", s, "x", "i")`, `"x3"`},
	} {
		if got := evalRe(t, tc.src); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.src, got, tc.want)
		}
	}
}

// TestRegexpCacheReusesTheCompilation pins that the cache actually caches. Without this the whole
// change is unobservable except in a benchmark, and a cache that silently stopped working would look
// exactly like one that worked.
func TestRegexpCacheReusesTheCompilation(t *testing.T) {
	const pat = "^cachedprobe[0-9]+$"
	a, errA := compileRegexpCached(pat)
	b, errB := compileRegexpCached(pat)
	if errA != nil || errB != nil {
		t.Fatalf("compile errors: %v / %v", errA, errB)
	}
	if a != b {
		t.Error("the same pattern compiled twice; the cache is not being consulted")
	}
	c, _ := compileRegexpCached(pat + "x")
	if c == a {
		t.Error("two different patterns share a compilation")
	}
}

// TestRegexpCacheCachesFailures covers the path where nothing useful happens at all. An invalid
// pattern is a value-level error in ClassAd, so a constraint containing one is evaluated for every
// record -- and would re-fail the compile every time if failures were not cached.
func TestRegexpCacheCachesFailures(t *testing.T) {
	const bad = "^(unclosedprobe"
	re1, err1 := compileRegexpCached(bad)
	re2, err2 := compileRegexpCached(bad)
	if err1 == nil || err2 == nil {
		t.Fatal("an invalid pattern compiled without error")
	}
	if re1 != nil || re2 != nil {
		t.Error("an invalid pattern produced a non-nil expression")
	}
	if err1 != err2 {
		t.Error("the failure was not cached; each call re-ran the compile")
	}
	// And it is still an error value at the expression level, not a panic or a match.
	if got := evalRe(t, `regexp("^(unclosedprobe", s)`); got != "error" {
		t.Errorf("an invalid pattern in an expression gave %s, want error", got)
	}
}

// TestRegexpCacheIsBounded checks the cache cannot grow without limit. A query names its own
// patterns, so an unbounded cache inside a long-lived daemon is a memory-growth vector rather than a
// cache -- and the clearing must not break the patterns that come after it.
func TestRegexpCacheIsBounded(t *testing.T) {
	for i := 0; i < maxCachedRegexps+200; i++ {
		pat := fmt.Sprintf("^bounded%dprobe$", i)
		if _, err := compileRegexpCached(pat); err != nil {
			t.Fatalf("%s: %v", pat, err)
		}
	}
	if n := regexpCacheN.Load(); n > maxCachedRegexps {
		t.Errorf("cache holds %d entries, over the %d bound", n, maxCachedRegexps)
	}
	// Still correct after a clear, which is the part that would break if the overflow path dropped
	// an entry it then kept serving.
	if got := evalRe(t, `regexp("^user", s)`); got != "true" {
		t.Errorf("after overflow, regexp gave %s, want true", got)
	}
	if got := evalRe(t, `regexp("^nope", s)`); got != "false" {
		t.Errorf("after overflow, a non-match gave %s, want false", got)
	}
}

// TestRegexpCacheConcurrent exercises the read path and the overflow path together under the race
// detector. A *regexp.Regexp is safe for concurrent use, which is what lets one cached entry serve
// every goroutine with no lock on the read path -- and is worth asserting rather than assuming.
func TestRegexpCacheConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				// A shared hot pattern, plus one of its own, so both the hit and the miss path run.
				if got := evalRe(t, `regexp("^user", s)`); got != "true" {
					t.Errorf("concurrent hot pattern gave %s", got)
					return
				}
				if _, err := compileRegexpCached(fmt.Sprintf("^g%dn%d$", g, i)); err != nil {
					t.Errorf("concurrent compile: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
