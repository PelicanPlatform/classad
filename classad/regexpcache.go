package classad

import (
	"regexp"
	"sync"
	"sync/atomic"
)

// A COMPILED-PATTERN CACHE for the regular-expression built-ins.
//
// Every one of them -- regexp, regexps, regexpMember, stringListRegexpMember, replace, replaceAll --
// built its pattern string and called regexp.Compile, on every call. For a constraint the pattern is
// a literal and the call happens once per RECORD, so a scan recompiled the same expression a million
// times. Measured on `regexp("^user", s)`: 1529 ns and 34 allocations per call, against 143 ns and 1
// allocation for a strcmp doing comparable work, and 4034 ns for a pattern with an alternation and a
// repetition. The compile was an order of magnitude more than the match.
//
// The cache is keyed on the FULL pattern -- flags prefix included -- because that is exactly the
// string handed to regexp.Compile, so two calls agreeing on the key cannot disagree about the
// expression. Keying on the user's pattern and handling flags separately would be a second place for
// the flag encoding to live.
//
// FAILURES ARE CACHED TOO. An invalid pattern is a value-level error in ClassAd, not an exception, so
// a constraint containing one is evaluated for every record and would otherwise re-fail the compile
// every time -- the same waste as the success case, on the path where nothing useful happens at all.
//
// BOUNDED, because a query names its own patterns. A caller that generates one pattern per record
// would grow an unbounded cache inside a long-lived daemon, which is a memory-growth vector rather
// than a cache. On overflow the whole map is dropped: crude compared with an LRU, but a cache this
// shape has a handful of live patterns in every realistic case, and the hot one is re-added by its
// next use. An eviction policy would be more code defending against a case that does not happen,
// and would need its own locking on the read path.
const maxCachedRegexps = 1024

type cachedRegexp struct {
	re  *regexp.Regexp
	err error
}

var (
	regexpCache  sync.Map // full pattern -> *cachedRegexp
	regexpCacheN atomic.Int64
)

// compileRegexpCached compiles fullPattern, reusing a previous compilation of the same string.
//
// A *regexp.Regexp is safe for concurrent use, which is what makes one cached entry serve every
// goroutine without a lock on the read path.
func compileRegexpCached(fullPattern string) (*regexp.Regexp, error) {
	if v, ok := regexpCache.Load(fullPattern); ok {
		// Comma-ok rather than a bare assertion: nothing else stores into this map, so a value of
		// another type cannot occur -- and falling through to compile is a better answer to "cannot
		// occur" than a panic in a daemon.
		if c, isEntry := v.(*cachedRegexp); isEntry {
			return c.re, c.err
		}
	}
	re, err := regexp.Compile(fullPattern)
	// Racing callers may both compile the same pattern. That is wasted work once, not a correctness
	// problem: the two results are equivalent and LoadOrStore keeps whichever arrived first.
	if regexpCacheN.Load() >= maxCachedRegexps {
		regexpCache.Clear()
		regexpCacheN.Store(0)
	}
	if _, loaded := regexpCache.LoadOrStore(fullPattern, &cachedRegexp{re: re, err: err}); !loaded {
		regexpCacheN.Add(1)
	}
	return re, err
}
