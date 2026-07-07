package collections

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A parameterized performance matrix over the three storage formats (in-memory,
// on-disk, append-only archive), a read/write workload mix, a query profile, and a
// concurrency level. It is driven by `go test -bench=Matrix` like any benchmark; a
// package-level collector accumulates each cell's result and TestMain renders them
// on exit — a markdown table to stdout always, and markdown/CSV/JSON files when
// CLASSAD_BENCH_OUT names an output directory.
//
//	go test -run '^$' -bench BenchmarkMatrix -benchtime 200ms .
//	CLASSAD_BENCH_OUT=/tmp go test -run '^$' -bench BenchmarkMatrix .
//	CLASSAD_MATRIX_N=20000 go test -run '^$' -bench BenchmarkMatrix -benchtime 1s .
//
// Not every cell is meaningful: the archive is append-only, so single-item Get is
// N/A there; a write-only workload does not vary by query profile, so it collapses
// the query axis. Skipped cells are simply absent from the table.

// matrixCell is one measured point in the matrix.
type matrixCell struct {
	Format    string  `json:"format"`
	Workload  string  `json:"workload"`
	Query     string  `json:"query"`
	Conc      int     `json:"conc"`
	NsPerOp   float64 `json:"ns_per_op"`
	OpsPerSec float64 `json:"ops_per_sec"`
	Matches   int     `json:"sample_matches"` // matches from one untimed probe read (read cells)
}

var (
	matrixMu    sync.Mutex
	matrixCells []matrixCell
)

func recordCell(c matrixCell) {
	matrixMu.Lock()
	matrixCells = append(matrixCells, c)
	matrixMu.Unlock()
}

// TestMain renders the collected matrix after the run (only if BenchmarkMatrix
// actually populated it), so a plain `go test` is unaffected.
func TestMain(m *testing.M) {
	code := m.Run()
	if len(matrixCells) > 0 {
		emitMatrix()
	}
	os.Exit(code)
}

// ---- corpus (loaded + pre-sorted once; shared read-only across cells) ----

var (
	corpusMu  sync.Mutex
	corpusAds []*classad.ClassAd
)

// matrixCorpus loads the real-ad corpus once and pre-sorts every ad's AST, so the
// concurrent write path (Put/Append call ad.AST()) never races on an in-place sort.
func matrixCorpus(tb testing.TB) []*classad.ClassAd {
	corpusMu.Lock()
	defer corpusMu.Unlock()
	if corpusAds == nil {
		ads := loadCorpus(tb)
		for _, a := range ads {
			_ = a.AST() // sort once so later concurrent AST() calls are stable
		}
		corpusAds = ads
	}
	return corpusAds
}

// ---- store adapter unifying the three formats behind one op interface ----

type benchStore struct {
	put   func(i int)           // one write op (Put overwrite / Append)
	get   func(i int)           // one single-item read; nil if unsupported (archive)
	query func(q *vm.Query) int // one query, returns match count
	close func()
}

// Indexed on Arch (categorical) + Memory (value); scan queries use Cpus (unindexed).
var (
	matrixCat = []string{"Arch"}
	matrixVal = []string{"Memory"}
)

func fillCollection(tb testing.TB, c *Collection, ads []*classad.ClassAd, n int) {
	const batchSize = 512
	batch := make([]AdUpdate, 0, batchSize)
	flush := func() {
		if len(batch) > 0 {
			if err := c.Update(batch); err != nil {
				tb.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	for i := 0; i < n; i++ {
		batch = append(batch, AdUpdate{Key: []byte("ad-" + strconv.Itoa(i)), Ad: ads[i%len(ads)]})
		if len(batch) == batchSize {
			flush()
		}
	}
	flush()
}

func newBenchStore(tb testing.TB, format string, ads []*classad.ClassAd, n int) *benchStore {
	switch format {
	case "mem":
		c := New(Options{Shards: 16, CategoricalAttrs: matrixCat, ValueAttrs: matrixVal})
		fillCollection(tb, c, ads, n)
		c.Reindex()
		return collectionStore(c, ads, n, func() {})
	case "disk":
		dir := tb.TempDir()
		c, err := Open(Options{Shards: 16, Dir: dir, CategoricalAttrs: matrixCat, ValueAttrs: matrixVal})
		if err != nil {
			tb.Fatal(err)
		}
		fillCollection(tb, c, ads, n)
		c.Reindex()
		return collectionStore(c, ads, n, func() { c.Close() })
	case "archive":
		dir := tb.TempDir()
		a, err := CreateArchive(ArchiveOptions{
			Dir: dir, SegmentSize: 1 << 20,
			CategoricalAttrs: matrixCat, ValueAttrs: matrixVal,
		})
		if err != nil {
			tb.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if err := a.Append(ads[i%len(ads)]); err != nil {
				tb.Fatal(err)
			}
		}
		_ = a.Flush()
		return &benchStore{
			put: func(i int) { _ = a.Append(ads[i%len(ads)]) },
			get: nil,
			query: func(q *vm.Query) int {
				m := 0
				for range a.Query(q) {
					m++
				}
				return m
			},
			close: func() { a.Close() },
		}
	}
	tb.Fatalf("unknown format %q", format)
	return nil
}

func collectionStore(c *Collection, ads []*classad.ClassAd, n int, closeFn func()) *benchStore {
	return &benchStore{
		put: func(i int) { _ = c.Put([]byte("ad-"+strconv.Itoa(i%n)), ads[i%len(ads)]) },
		get: func(i int) { c.Get([]byte("ad-" + strconv.Itoa(i%n))) },
		query: func(q *vm.Query) int {
			m := 0
			for range c.Query(q) {
				m++
			}
			return m
		},
		close: closeFn,
	}
}

// ---- the matrix ----

type workload struct {
	name  string
	wfrac float64 // fraction of ops that are writes
}

type readKind struct {
	name string
	q    *vm.Query // nil ⇒ single-item Get (or, for write-only, no read)
	get  bool
}

func isWrite(i int, frac float64) bool {
	if frac <= 0 {
		return false
	}
	if frac >= 1 {
		return true
	}
	return i%100 < int(frac*100+0.5)
}

func BenchmarkMatrix(b *testing.B) {
	ads := matrixCorpus(b)
	n := envInt("CLASSAD_MATRIX_N", 3000)
	qLow := mustQuery(b, `Cpus >= 0`)                          // unindexed, matches most → full scan
	qHigh := mustQuery(b, `Cpus == 987654`)                    // unindexed, ~no matches → selective scan
	qIdx := mustQuery(b, `Memory >= 1000 && Arch == "X86_64"`) // indexed (Memory + Arch)

	formats := []string{"mem", "disk", "archive"}
	workloads := []workload{
		{"read-only", 0}, {"read-heavy", 0.1}, {"mixed", 0.5}, {"write-heavy", 0.9}, {"write-only", 1.0},
	}
	reads := []readKind{
		{"low-sel", qLow, false},
		{"high-sel", qHigh, false},
		{"indexed", qIdx, false},
		{"single-get", nil, true},
	}
	concs := []int{1, 10, 100}

	for _, f := range formats {
		b.Run("fmt="+f, func(b *testing.B) {
			for _, w := range workloads {
				b.Run("work="+w.name, func(b *testing.B) {
					variants := reads
					if w.wfrac >= 1.0 {
						variants = []readKind{{"none", nil, false}} // write-only: query axis irrelevant
					}
					for _, rk := range variants {
						if rk.get && f == "archive" {
							continue // single-item Get is N/A for the append-only archive
						}
						b.Run("query="+rk.name, func(b *testing.B) {
							for _, cc := range concs {
								b.Run("conc="+strconv.Itoa(cc), func(b *testing.B) {
									runCell(b, ads, n, f, w, rk, cc)
								})
							}
						})
					}
				})
			}
		})
	}
}

func runCell(b *testing.B, ads []*classad.ClassAd, n int, format string, w workload, rk readKind, conc int) {
	st := newBenchStore(b, format, ads, n)
	defer st.close()

	// One untimed probe read for a sanity match count (read cells only).
	matches := 0
	if !rk.get && rk.q != nil {
		matches = st.query(rk.q)
	}

	b.ResetTimer()
	var wg sync.WaitGroup
	per := (b.N + conc - 1) / conc
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < per; j++ {
				i := g*per + j
				if i >= b.N {
					return
				}
				switch {
				case isWrite(i, w.wfrac):
					st.put(i)
				case rk.get && st.get != nil:
					st.get(i)
				case rk.q != nil:
					st.query(rk.q)
				}
			}
		}(g)
	}
	wg.Wait()
	b.StopTimer()

	el := b.Elapsed()
	var ns, ops float64
	if b.N > 0 && el > 0 {
		ns = float64(el.Nanoseconds()) / float64(b.N)
		ops = float64(b.N) / el.Seconds()
	}
	b.ReportMetric(ops, "ops/s")
	recordCell(matrixCell{format, w.name, rk.name, conc, ns, ops, matches})
}

func mustQuery(tb testing.TB, s string) *vm.Query {
	q, err := vm.Parse(s)
	if err != nil {
		tb.Fatalf("parse %q: %v", s, err)
	}
	return q
}

// ---- emission (stdout markdown always; MD/CSV/JSON files on CLASSAD_BENCH_OUT) ----

func emitMatrix() {
	matrixMu.Lock()
	defer matrixMu.Unlock()

	// Keep the last (largest-N, most stable) result per cell, in first-seen order,
	// which is the natural axis order the nested benchmarks ran in.
	type key struct {
		f, w, q string
		c       int
	}
	last := map[key]matrixCell{}
	var order []key
	for _, c := range matrixCells {
		k := key{c.Format, c.Workload, c.Query, c.Conc}
		if _, ok := last[k]; !ok {
			order = append(order, k)
		}
		last[k] = c
	}
	rows := make([]matrixCell, 0, len(order))
	for _, k := range order {
		rows = append(rows, last[k])
	}

	var sb strings.Builder
	sb.WriteString("\n## Benchmark matrix (real-ad corpus)\n\n")
	sb.WriteString("| format | workload | query | conc | ns/op | ops/s | matches |\n")
	sb.WriteString("|--------|----------|-------|-----:|------:|------:|--------:|\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | %s | %s | %d | %.0f | %.0f | %d |\n",
			r.Format, r.Workload, r.Query, r.Conc, r.NsPerOp, r.OpsPerSec, r.Matches)
	}
	fmt.Print(sb.String())

	out := os.Getenv("CLASSAD_BENCH_OUT")
	if out == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(out, "bench_matrix.md"), []byte(sb.String()), 0o644)

	if data, err := json.MarshalIndent(rows, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(out, "bench_matrix.json"), data, 0o644)
	}

	if f, err := os.Create(filepath.Join(out, "bench_matrix.csv")); err == nil {
		cw := csv.NewWriter(f)
		_ = cw.Write([]string{"format", "workload", "query", "conc", "ns_per_op", "ops_per_sec", "matches"})
		for _, r := range rows {
			_ = cw.Write([]string{
				r.Format, r.Workload, r.Query, strconv.Itoa(r.Conc),
				strconv.FormatFloat(r.NsPerOp, 'f', 1, 64),
				strconv.FormatFloat(r.OpsPerSec, 'f', 1, 64),
				strconv.Itoa(r.Matches),
			})
		}
		cw.Flush()
		f.Close()
	}
	fmt.Printf("\nwrote bench_matrix.{md,csv,json} to %s\n", out)
}
