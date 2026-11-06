package collections

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestListValueShapeReport measures what list-valued attributes look like in a corpus of real ads: which
// attributes hold lists, how often, of what element kinds, how long, how much of a record they are, and
// how often a block holds the same list more than once (which is what a ListView layout with per-block
// dedup would save). Opt-in: CLASSAD_LISTCOL_MEASURE=1. CLASSAD_LISTCOL_ADS names extra old-format ad
// files (comma-separated, optionally .gz) to measure alongside the committed pool sample.
func TestListValueShapeReport(t *testing.T) {
	if os.Getenv("CLASSAD_LISTCOL_MEASURE") == "" {
		t.Skip("set CLASSAD_LISTCOL_MEASURE=1 to run")
	}
	files := []string{committedCorpus}
	if extra := os.Getenv("CLASSAD_LISTCOL_ADS"); extra != "" {
		files = append(files, strings.Split(extra, ",")...)
	}
	for _, f := range files {
		ads := readOldAds(t, f)
		t.Logf("==== %s: %d ads", f, len(ads))
		for _, rows := range []int{16, 128} {
			reportListShapes(t, ads, rows)
		}
	}
}

func readOldAds(t *testing.T, path string) []*classad.ClassAd {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			t.Fatal(gerr)
		}
		defer gz.Close()
		src = gz
	}
	data, err := io.ReadAll(src)
	if err != nil {
		t.Fatal(err)
	}
	// condor_history output separates ads with a "*** ..." banner line; condor_status -l with a blank line.
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "***") {
			l = ""
		}
		lines = append(lines, l)
	}
	r := classad.NewOldReader(strings.NewReader(strings.Join(lines, "\n")))
	var ads []*classad.ClassAd
	for r.Next() {
		ads = append(ads, r.ClassAd())
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return ads
}

type listAttrStats struct {
	name                        string
	present                     int
	qualifies                   int            // all elements literals of ONE of int/real/string/bool (or empty)
	kinds                       map[string]int // element kind -> element count
	listsWithUndef              int
	listsIntRealMix             int
	listsNested                 int // an element that is a list or an ad
	listsExpr                   int // an element that is an expression
	empty                       int
	lens                        []int
	nodeBytes, recBytes         int
	nonCanonical                int // a qualifying list whose wire bytes differ from a re-encoding of its values
	blockPresent, blockDistinct int
	childPlain, childDedup      int // elements stored, plain offsets vs ListView with exact per-block dedup
}

// listFrameOverhead is how many bytes EncodeInline puts before the node of a one-attribute ad named "x".
func listFrameOverhead() int {
	u := wire.EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "x", Value: &ast.UndefinedLiteral{}}}})
	return len(u) - 1
}

func elemKind(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.IntegerLiteral:
		return "int"
	case *ast.RealLiteral:
		return "real"
	case *ast.StringLiteral:
		return "string"
	case *ast.BooleanLiteral:
		return "bool"
	case *ast.UndefinedLiteral:
		return "undefined"
	case *ast.ErrorLiteral:
		return "error"
	case *ast.ListLiteral:
		return "list"
	case *ast.RecordLiteral:
		return "ad"
	case *ast.ParenExpr:
		return elemKind(v.Inner)
	case *ast.UnaryOp:
		if k := elemKind(v.Expr); (k == "int" || k == "real") && (v.Op == "-" || v.Op == "+") {
			return "signed-" + k
		}
	}
	return "expr"
}

func reportListShapes(t *testing.T, ads []*classad.ClassAd, rows int) {
	stats := map[string]*listAttrStats{}
	get := func(name string) *listAttrStats {
		k := strings.ToLower(name)
		s := stats[k]
		if s == nil {
			s = &listAttrStats{name: name, kinds: map[string]int{}}
			stats[k] = s
		}
		return s
	}
	totalRec := 0
	listRecs := 0
	// per block: attribute -> distinct list bytes
	for b0 := 0; b0 < len(ads); b0 += rows {
		end := min(b0+rows, len(ads))
		seen := map[string]map[string]int{}
		for _, ad := range ads[b0:end] {
			rec := wire.EncodeInline(nil, ad.AST())
			totalRec += len(rec)
			hasList := false
			for _, a := range ad.AST().Attributes {
				l, ok := a.Value.(*ast.ListLiteral)
				if !ok {
					continue
				}
				hasList = true
				s := get(a.Name)
				s.present++
				node := wire.EncodeInline(nil, &ast.ClassAd{Attributes: []*ast.AttributeAssignment{{Name: "x", Value: l}}})
				node = node[listFrameOverhead():] // the list node alone
				s.nodeBytes += len(node)
				s.recBytes += len(rec)
				s.lens = append(s.lens, len(l.Elements))
				if len(l.Elements) == 0 {
					s.empty++
				}
				kinds := map[string]bool{}
				for _, e := range l.Elements {
					k := elemKind(e)
					kinds[k] = true
					s.kinds[k]++
				}
				if kinds["undefined"] {
					s.listsWithUndef++
				}
				if kinds["int"] && kinds["real"] {
					s.listsIntRealMix++
				}
				if kinds["list"] || kinds["ad"] {
					s.listsNested++
				}
				if kinds["expr"] {
					s.listsExpr++
				}
				single := len(kinds) <= 1
				for k := range kinds {
					if k != "int" && k != "real" && k != "string" && k != "bool" {
						single = false
					}
				}
				if single {
					s.qualifies++
					// Canonical: the list node as encoded equals the re-encoding of its element values.
					var re []byte
					for _, e := range l.Elements {
						switch v := e.(type) {
						case *ast.IntegerLiteral:
							re = wire.AppendIntNode(re, v.Value)
						case *ast.RealLiteral:
							re = wire.AppendRealNode(re, v.Value)
						case *ast.StringLiteral:
							re = wire.AppendStringNode(re, v.Value)
						case *ast.BooleanLiteral:
							re = wire.AppendBoolNode(re, v.Value)
						}
					}
					if !bytes.HasSuffix(node, re) || len(node) != 1+uvarintLen(uint64(len(l.Elements)))+len(re) {
						s.nonCanonical++
					}
				}
				key := string(node)
				if seen[strings.ToLower(a.Name)] == nil {
					seen[strings.ToLower(a.Name)] = map[string]int{}
				}
				if _, dup := seen[strings.ToLower(a.Name)][key]; !dup {
					s.blockDistinct++
					s.childDedup += len(l.Elements)
				}
				seen[strings.ToLower(a.Name)][key]++
				s.blockPresent++
				s.childPlain += len(l.Elements)
			}
			if hasList {
				listRecs++
			}
		}
	}
	names := make([]string, 0, len(stats))
	for k := range stats {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return stats[names[i]].nodeBytes > stats[names[j]].nodeBytes })
	t.Logf("-- block rows=%d: %d ads, %d with any list, record bytes %d", rows, len(ads), listRecs, totalRec)
	t.Logf("%-30s %5s %5s %-34s %4s %4s %4s %4s %4s %-16s %8s %6s %5s %10s %8s", "attribute", "pres", "qual",
		"element kinds", "undf", "mix", "nest", "expr", "empt", "len min/p50/max", "bytes", "%rec", "nonc", "dist/pres", "child")
	sumBytes, sumPlain, sumDedup, sumPresent, sumDistinct := 0, 0, 0, 0, 0
	for _, k := range names {
		s := stats[k]
		sort.Ints(s.lens)
		kinds := []string{}
		for kk, n := range s.kinds {
			kinds = append(kinds, fmt.Sprintf("%s:%d", kk, n))
		}
		sort.Strings(kinds)
		t.Logf("%-30s %5d %5d %-34s %4d %4d %4d %4d %4d %-16s %8d %5.2f%% %5d %5d/%-4d %4d/%-4d", s.name, s.present, s.qualifies,
			strings.Join(kinds, ","), s.listsWithUndef, s.listsIntRealMix, s.listsNested, s.listsExpr, s.empty,
			fmt.Sprintf("%d/%d/%d", s.lens[0], s.lens[len(s.lens)/2], s.lens[len(s.lens)-1]),
			s.nodeBytes, 100*float64(s.nodeBytes)/float64(totalRec), s.nonCanonical, s.blockDistinct, s.blockPresent,
			s.childDedup, s.childPlain)
		sumBytes += s.nodeBytes
		sumPlain += s.childPlain
		sumDedup += s.childDedup
		sumPresent += s.blockPresent
		sumDistinct += s.blockDistinct
	}
	t.Logf("all lists: %d bytes = %.2f%% of record bytes; block-distinct %d of %d present lists; child elements plain %d, ListView-dedup %d (%.1f%% saved)",
		sumBytes, 100*float64(sumBytes)/float64(max(totalRec, 1)), sumDistinct, sumPresent, sumPlain, sumDedup,
		100*(1-float64(sumDedup)/float64(max(sumPlain, 1))))
}

// TestListColumnBytesReport measures what the list columns cost on disk over the pool sample: one store,
// columnarized with and then without them under the same schema, its segment files summed each time, and
// the compressed list sections themselves. Opt-in: CLASSAD_LISTCOL_MEASURE=1; CLASSAD_BENCH_N sets the
// record count (default 4000).
func TestListColumnBytesReport(t *testing.T) {
	if os.Getenv("CLASSAD_LISTCOL_MEASURE") == "" {
		t.Skip("set CLASSAD_LISTCOL_MEASURE=1 to run")
	}
	sample := loadCorpus(t)
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 1, SegmentSize: 8 << 20, GroupSchemaCount: -1}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	n := envInt(envWorkingN, 4000)
	for i := 0; i < n; i += 512 {
		var batch []AdUpdate
		for j := i; j < i+512 && j < n; j++ {
			batch = append(batch, AdUpdate{Key: []byte(fmt.Sprintf("ad-%d", j)), Ad: sample[j%len(sample)]})
		}
		if uerr := c.Update(batch); uerr != nil {
			t.Fatal(uerr)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema")
	}
	c.ColumnarizeSealed()
	segBytes := func() (total int64, lists int64, payload int64) {
		for _, sh := range c.shards {
			sh.mu.RLock()
			for _, seg := range sh.segs {
				if seg == nil || seg == sh.act {
					continue
				}
				total += int64(seg.used)
				if cn := seg.colNative.Load(); cn != nil {
					for _, b := range cn.seg.blocks {
						lists += int64(len(b.listComp)) + 8 + 4*int64(len(b.listIDs))
					}
				}
			}
			sh.mu.RUnlock()
		}
		return
	}
	withTotal, withLists, _ := segBytes()
	prev := colListColumns
	colListColumns = false
	defer func() { colListColumns = prev }()
	recolumnarizeAll(t, c)
	withoutTotal, _, _ := segBytes()
	t.Logf("%d pool ads: sealed segment bytes with list columns %d, without %d (+%d, %.2f%%); list sections %d bytes",
		n, withTotal, withoutTotal, withTotal-withoutTotal, 100*float64(withTotal-withoutTotal)/float64(withoutTotal), withLists)
	c.Close()

	// What the lists cost the records: the same ads with every list-valued attribute removed, in a store
	// of their own. Moving the lists out of the records could save at most this.
	stripped := make([]*classad.ClassAd, len(sample))
	for i, ad := range sample {
		cp := classad.New()
		for _, a := range ad.AST().Attributes {
			if _, isList := a.Value.(*ast.ListLiteral); !isList {
				cp.Insert(a.Name, a.Value)
			}
		}
		stripped[i] = cp
	}
	c2, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 8 << 20, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for i := 0; i < n; i += 512 {
		var batch []AdUpdate
		for j := i; j < i+512 && j < n; j++ {
			batch = append(batch, AdUpdate{Key: []byte(fmt.Sprintf("ad-%d", j)), Ad: stripped[j%len(stripped)]})
		}
		if err := c2.Update(batch); err != nil {
			t.Fatal(err)
		}
	}
	c2.BuildAndEnableSchemaScan(4096, 8)
	c2.ColumnarizeSealed()
	c = c2
	noListTotal, _, _ := segBytes()
	t.Logf("same ads without their list attributes: %d sealed segment bytes; the lists cost the records about %d (%.2f%%)",
		noListTotal, withoutTotal-noListTotal, 100*float64(withoutTotal-noListTotal)/float64(withoutTotal))
}
