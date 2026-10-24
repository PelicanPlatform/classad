package db

import (
	"flag"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/aggfuzz/gen"
	"github.com/PelicanPlatform/classad/collections"
)

// Reporting a disagreement.
//
// A seed hash is not a reproducer: it names an input to a generator whose output nobody can read,
// and it is useless the moment the generator changes. What a reader needs is the two things that
// disagreed -- the ads and the query -- small enough to paste into a focused test. So a failure is
// shrunk first: the record list is reduced, by rebuilding the archive from subsets, to a small set
// that still disagrees, and those records are printed in old-ClassAd form.
//
// Shrinking can fail, and failing is informative rather than fatal. The fast paths only exist on a
// columnarized, sealed, indexed archive, so a subset small enough to print may no longer have the
// shape that reaches the path under test -- in which case the disagreement stops reproducing and
// the full shape is reported instead, which is still exactly replayable (every record is a pure
// function of the shape and its index).

// shrinkBudget bounds the archive rebuilds one failure may cost. Each rebuild is a full
// append/merge/columnarize cycle, so this is seconds, not minutes -- and it only runs on a failure.
const shrinkBudget = 150

// reportDisagreement fails the test with the smallest reproducer it can find. The fuzz target uses
// it directly: there one input is one finding, and stopping on it with a reproducer is the point.
func reportDisagreement(t *testing.T, f *aggFixture, q gen.Query, in []byte, tier collections.AggTier, diffs []string) {
	t.Helper()
	out := renderFinding(f, q, in, tier, diffs)
	// Not inside a fuzz worker. Shrinking is a hundred-odd archive rebuilds, which is seconds --
	// far past the per-input budget the fuzz engine allows a worker before it declares the process
	// hung and abandons minimization. The engine saves the failing input either way, and re-running
	// it (`go test -run 'FuzzAggregateStrategies/<hash>'`) comes back through here as an ordinary
	// test, where the shrink does run.
	if !inFuzzWorker() {
		out += shrinkReport(t, f, q)
	} else {
		out += "  repro:  saved by the fuzz engine; re-run it to get the shrunk record list\n"
	}
	t.Fatal(out)
}

// inFuzzWorker reports whether this process is one of `go test -fuzz`'s worker processes, which
// the test binary records in the -test.fuzzworker flag.
func inFuzzWorker() bool {
	f := flag.Lookup("test.fuzzworker")
	return f != nil && f.Value.String() == "true"
}

// renderFinding is the human-readable form of one disagreement, without the shrink.
func renderFinding(f *aggFixture, q gen.Query, in []byte, tier collections.AggTier, diffs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "aggregate strategies disagree with the record scan\n")
	fmt.Fprintf(&b, "  query:  %s\n", q)
	fmt.Fprintf(&b, "  tier:   %s (the dispatcher's choice)\n", tier)
	fmt.Fprintf(&b, "  shape:  %+v\n", f.shape)
	fmt.Fprintf(&b, "  input:  %#v\n", in)
	for i, d := range diffs {
		if i == 8 {
			fmt.Fprintf(&b, "  diff:   ... %d more\n", len(diffs)-i)
			break
		}
		fmt.Fprintf(&b, "  diff:   %s\n", d)
	}
	return b.String()
}

// shrinkReport reduces the record list to a subset that still disagrees and renders those records.
func shrinkReport(t *testing.T, f *aggFixture, q gen.Query) string {
	t.Helper()
	idx := make([]int, f.shape.N)
	for i := range idx {
		idx[i] = i
	}
	var b strings.Builder
	small, ok := shrinkRecords(t, f.shape, q, idx)
	if !ok {
		fmt.Fprintf(&b, "  repro:  did not reduce; replay with gen.ShapeByID(%d).Ads() (all %d records)\n",
			f.shape.ID, f.shape.N)
		return b.String()
	}
	fmt.Fprintf(&b, "  repro:  %d of %d records still disagree -- gen.ShapeByID(%d).AdsAt(%v)\n",
		len(small), f.shape.N, f.shape.ID, small)
	// The text form is lossy in exactly one way, and saying so is cheaper than someone pasting it
	// into a test and getting a different answer: a NaN prints as real("NaN") and a negative number
	// as unary minus over a literal, both of which re-parse to the same VALUE but as expressions
	// rather than the stored literals the columns are built from.
	fmt.Fprintf(&b, "  ads (old-ClassAd form; NaN and negative numbers re-parse as expressions,\n")
	fmt.Fprintf(&b, "       not as the stored literals -- gen.Shape.Ad re-inserts them):\n")
	for k, i := range small {
		if k == 40 {
			fmt.Fprintf(&b, "    ... %d more\n", len(small)-k)
			break
		}
		fmt.Fprintf(&b, "    --- record %d ---\n", i)
		for _, line := range strings.Split(strings.TrimRight(f.shape.Ad(i).MarshalOld(), "\n"), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return b.String()
}

// reproduces builds a throwaway archive from the named records and reports whether the query still
// disagrees there.
func reproduces(t *testing.T, shape gen.Shape, q gen.Query, idx []int, nth int) bool {
	t.Helper()
	if len(idx) == 0 {
		return false
	}
	dir := filepath.Join(t.TempDir(), fmt.Sprintf("shrink%04d", nth))
	cat, tbl := buildFuzzArchive(t, dir, shape, shape.AdsAt(idx))
	defer cat.Close()
	_, _, diffs, err := checkQuery(&aggFixture{shape: shape, cat: cat, tbl: tbl}, q)
	return err == nil && len(diffs) > 0
}

// shrinkRecords reduces the record list to a subset that still disagrees, ddmin-style: try the
// chunks of an n-way split, then their complements, then split finer. ok is false when even the
// full list no longer reproduces (which happens when the disagreement depends on something the
// rebuild does not reconstruct) or when the budget runs out without reducing anything.
func shrinkRecords(t *testing.T, shape gen.Shape, q gen.Query, idx []int) ([]int, bool) {
	t.Helper()
	spent := 0
	try := func(cand []int) bool {
		if spent >= shrinkBudget {
			return false
		}
		spent++
		return reproduces(t, shape, q, cand, spent)
	}
	if !try(idx) {
		return nil, false // the rebuilt archive does not reproduce at all
	}
	n := 2
	for len(idx) > 1 && spent < shrinkBudget {
		chunks := split(idx, n)
		reduced := false
		for _, c := range chunks { // a chunk on its own
			if try(c) {
				idx, n, reduced = c, 2, true
				break
			}
		}
		if !reduced {
			for i := range chunks { // everything but one chunk
				comp := complementOf(chunks, i)
				if try(comp) {
					idx, n, reduced = comp, max(n-1, 2), true
					break
				}
			}
		}
		if reduced {
			continue
		}
		if n >= len(idx) {
			break // already at single records and neither direction helps
		}
		n = min(2*n, len(idx))
	}
	return idx, true
}

func split(idx []int, n int) [][]int {
	if n > len(idx) {
		n = len(idx)
	}
	out := make([][]int, 0, n)
	for i := 0; i < n; i++ {
		lo, hi := i*len(idx)/n, (i+1)*len(idx)/n
		if lo < hi {
			out = append(out, idx[lo:hi])
		}
	}
	return out
}

func complementOf(chunks [][]int, skip int) []int {
	var out []int
	for i, c := range chunks {
		if i != skip {
			out = append(out, c...)
		}
	}
	return out
}

// findings buckets disagreements by signature over a whole sweep. One wrong strategy produces a
// disagreement per matching query, and a test that reports all of them buries the second bug under
// the first; a test that stops at the first never finds the second at all. So every finding is
// counted under a redacted signature, one example of each is kept, and only the very first is
// shrunk -- the shrink is archive rebuilds, and paying for it per signature would dominate the run.
type findings struct {
	mu    sync.Mutex
	seen  map[string]int
	order []string
	first map[string]string // signature -> the example, already rendered
	repro string            // the shrunk reproducer for the first finding of the run
}

// signatureOf redacts a diff down to its shape: group keys, numbers and quoted values vary per
// query, the WAY the answers differ does not.
func signatureOf(tier collections.AggTier, diffs []string) string {
	if len(diffs) == 0 {
		return tier.String()
	}
	d := diffs[0]
	d = regexp.MustCompile(`"[^"]*"`).ReplaceAllString(d, `"_"`)
	d = regexp.MustCompile(`-?[0-9][0-9.eE+-]*`).ReplaceAllString(d, "N")
	return tier.String() + " | " + d
}

func (fs *findings) add(t *testing.T, f *aggFixture, q gen.Query, in []byte, tier collections.AggTier, diffs []string) {
	t.Helper()
	sig := signatureOf(tier, diffs)
	fs.mu.Lock()
	n := fs.seen[sig]
	fs.seen[sig] = n + 1
	shrink := len(fs.order) == 0
	if n == 0 {
		fs.order = append(fs.order, sig)
		fs.first[sig] = renderFinding(f, q, in, tier, diffs)
	}
	fs.mu.Unlock()
	if shrink {
		r := shrinkReport(t, f, q)
		fs.mu.Lock()
		fs.repro = r
		fs.mu.Unlock()
	}
}

func (fs *findings) report(t *testing.T) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.order) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d distinct disagreement(s) between an aggregate strategy and the record scan:\n", len(fs.order))
	for _, sig := range fs.order {
		fmt.Fprintf(&b, "\n== %s  (%d queries)\n%s", sig, fs.seen[sig], fs.first[sig])
	}
	if fs.repro != "" {
		fmt.Fprintf(&b, "\n%s", fs.repro)
	}
	t.Error(b.String())
}
