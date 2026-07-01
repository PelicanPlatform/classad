//go:build libclassad

// Command cafuzz is the standalone differential driver for the ClassAd
// evaluation engines. It generates random valid ClassAds (or reads them from a
// corpus), evaluates each in both the Go engine and the reference C++
// libclassad (in-process via cgo), and reports every divergence, bucketed by a
// signature so that the same underlying bug is not reported thousands of times.
//
// Because libclassad runs in-process, a hard crash there would take cafuzz down
// with it. To leave a reproducer, the current input is journaled to a file
// before each evaluation (see -journal); on restart the journal names the ad
// that crashed.
//
// Examples:
//
//	cafuzz -n 100000                 # 100k generated ads from seed 1
//	cafuzz -n 1000 -seed 42 -v       # verbose: print every divergence
//	cafuzz -corpus seeds.txt         # run a file of ads, one per line
//	cafuzz -ad '[ a = 1/2 ]'         # one ad, print both engines' results
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/fuzz/canon"
	"github.com/PelicanPlatform/classad/fuzz/differ"
	"github.com/PelicanPlatform/classad/fuzz/gen"
)

func main() { os.Exit(run()) }

func run() int {
	var (
		n           = flag.Int("n", 10000, "number of random ClassAds to generate")
		seed        = flag.Int64("seed", 1, "PRNG seed for generation")
		corpus      = flag.String("corpus", "", "file of ClassAds (one per line) to run instead of generating")
		oneAd       = flag.String("ad", "", "evaluate a single ClassAd and print both engines' results")
		verbose     = flag.Bool("v", false, "print every divergence, not just bucket summaries")
		maxReport   = flag.Int("max-per-bucket", 3, "max example inputs to keep per divergence bucket")
		ignoreParse = flag.Bool("ignore-parse", false, "ignore parse-only divergences (focus on evaluation)")
		journal     = flag.String("journal", "", "write each input here before evaluating (crash reproducer)")
	)
	flag.Parse()

	opts := differ.DefaultOptions()
	opts.IgnoreParseDivergence = *ignoreParse

	if *oneAd != "" {
		runOne(*oneAd, opts)
		return 0
	}

	var jw *os.File
	if *journal != "" {
		var err error
		jw, err = os.Create(*journal)
		if err != nil {
			fmt.Fprintln(os.Stderr, "journal:", err)
			return 2
		}
		defer jw.Close()
	}

	buckets := newBucketSet(*maxReport)
	total := 0

	eval := func(src string) {
		total++
		journalWrite(jw, src)
		r := differ.Compare(src, opts)
		if r.IsDivergence() {
			buckets.add(r, src)
			if *verbose {
				fmt.Printf("[%s] %s\n    %s\n", r.Category, src, r.Detail)
			}
		}
	}

	if *corpus != "" {
		f, err := os.Open(*corpus)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			eval(line)
		}
	} else {
		g := gen.New(*seed, gen.DefaultConfig())
		for i := 0; i < *n; i++ {
			eval(g.ClassAd())
		}
	}

	buckets.report(total)
	if buckets.count() > 0 {
		return 1
	}
	return 0
}

// journalWrite records src as the sole content of the journal file before it is
// evaluated, so a hard crash in libclassad leaves the culprit on disk. All
// errors are best-effort: journaling must never abort the run.
func journalWrite(jw *os.File, src string) {
	if jw == nil {
		return
	}
	if _, err := jw.Seek(0, 0); err != nil {
		return
	}
	if err := jw.Truncate(0); err != nil {
		return
	}
	fmt.Fprintln(jw, src)
	if err := jw.Sync(); err != nil {
		return
	}
}

func runOne(src string, opts differ.Options) {
	r := differ.Compare(src, opts)
	fmt.Printf("input:    %s\n", src)
	fmt.Printf("category: %s\n", r.Category)
	fmt.Printf("go:       parsed=%v  %s\n", r.GoParsed, canon.Describe(r.GoCanon))
	if r.GoErr != nil {
		fmt.Printf("go-err:   %v\n", r.GoErr)
	}
	fmt.Printf("cpp:      parsed=%v  %s\n", r.CppParsed, canon.Describe(r.CppCanon))
	if r.Detail != "" {
		fmt.Printf("detail:   %s\n", r.Detail)
	}
	fmt.Printf("go-raw:   %q\ncpp-raw:  %q\n", r.GoRaw, r.CppRaw)
}

// --- divergence bucketing -------------------------------------------------

type bucket struct {
	category differ.Category
	sig      string
	count    int
	examples []string
}

type bucketSet struct {
	m         map[string]*bucket
	maxPerBkt int
}

func newBucketSet(maxPerBkt int) *bucketSet {
	return &bucketSet{m: map[string]*bucket{}, maxPerBkt: maxPerBkt}
}

// signature collapses results that look like the same bug. We key on the
// category plus a normalized form of the detail (attribute name stripped), so
// e.g. "int vs real division" collapses regardless of which attribute or
// operands triggered it.
func signature(r differ.Result) string {
	d := r.Detail
	if i := strings.Index(d, ": "); i >= 0 {
		d = d[i+2:] // drop the `attr "x"` prefix
	}
	// Drop the concrete numbers/strings inside go=.../cpp=..., keep the kinds.
	d = stripValues(d)
	return r.Category.String() + "|" + d
}

// stripValues replaces "kind(...)" payloads with just "kind(...)" so the
// signature reflects the type shapes, not concrete operands.
func stripValues(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '(' {
			depth++
			b.WriteByte('(')
			continue
		}
		if c == ')' {
			if depth > 0 {
				depth--
			}
			b.WriteByte(')')
			continue
		}
		if depth == 0 {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (bs *bucketSet) add(r differ.Result, src string) {
	sig := signature(r)
	b := bs.m[sig]
	if b == nil {
		b = &bucket{category: r.Category, sig: sig}
		bs.m[sig] = b
	}
	b.count++
	if len(b.examples) < bs.maxPerBkt {
		b.examples = append(b.examples, src)
	}
}

func (bs *bucketSet) count() int { return len(bs.m) }

func (bs *bucketSet) report(total int) {
	buckets := make([]*bucket, 0, len(bs.m))
	totalDiv := 0
	for _, b := range bs.m {
		buckets = append(buckets, b)
		totalDiv += b.count
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].count > buckets[j].count })

	fmt.Printf("\n==== differential summary ====\n")
	fmt.Printf("inputs evaluated:   %d\n", total)
	fmt.Printf("divergent inputs:   %d\n", totalDiv)
	fmt.Printf("distinct buckets:   %d\n\n", len(buckets))
	for _, b := range buckets {
		fmt.Printf("[%-16s] x%-7d %s\n", b.category, b.count, b.sig)
		for _, ex := range b.examples {
			fmt.Printf("        e.g. %s\n", ex)
		}
	}
}
