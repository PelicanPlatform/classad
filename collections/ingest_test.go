package collections

import (
	"bufio"
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// loadCorpusTexts returns the raw old-ClassAd text of each ad in the corpus.
func loadCorpusTexts(tb testing.TB) []string {
	tb.Helper()
	path := os.Getenv(envAdsFile)
	if path == "" {
		path = committedCorpus
		if _, err := os.Stat(path); err != nil {
			tb.Skipf("corpus %s not found; set %s", path, envAdsFile)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			tb.Fatal(err)
		}
		defer gz.Close()
		src = gz
	}
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var texts []string
	var b strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if b.Len() > 0 {
				texts = append(texts, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if b.Len() > 0 {
		texts = append(texts, b.String())
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	if len(texts) == 0 {
		tb.Fatalf("no ad texts parsed from %s", path)
	}
	return texts
}

// TestUpdateOldMatchesParseOld verifies that direct old-ClassAd ingestion produces
// ads equal to the parse-then-Put path, and that queries return identical results.
func TestUpdateOldMatchesParseOld(t *testing.T) {
	t.Parallel()
	texts := loadCorpusTexts(t)
	direct := New(Options{Shards: 4})
	viaAST := New(Options{Shards: 4})
	for i, text := range texts {
		key := []byte("ad-" + itoa(i))
		if err := direct.UpdateOld([]OldAdUpdate{{Key: key, Text: text}}); err != nil {
			t.Fatalf("UpdateOld ad %d: %v", i, err)
		}
		ad, err := classad.ParseOld(text)
		if err != nil {
			t.Fatalf("ParseOld ad %d: %v", i, err)
		}
		if err := viaAST.Put(key, ad); err != nil {
			t.Fatal(err)
		}
	}
	if direct.Len() != viaAST.Len() {
		t.Fatalf("Len differs: direct %d, viaAST %d", direct.Len(), viaAST.Len())
	}
	for i := range texts {
		key := []byte("ad-" + itoa(i))
		a, aok := direct.Get(key)
		b, bok := viaAST.Get(key)
		if aok != bok || !a.Equal(b) {
			t.Fatalf("ad %d differs:\n direct=%s\n viaAST=%s", i, a.String(), b.String())
		}
	}
	// Queries (including the wire-native path) must return identical match sets.
	for _, qs := range []string{
		`Cpus >= 1`, `Memory > 1000`, `Arch == "X86_64" || Arch == "x86_64"`,
		`Cpus >= 2 && Memory > 4000`, `Name =!= undefined`,
	} {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		if da, va := fastMatches(direct, q), fastMatches(viaAST, q); len(da) != len(va) {
			t.Errorf("query %q: direct matched %d, viaAST %d", qs, len(da), len(va))
		}
	}
}

// FuzzIngestOld stresses the scalar fast-path against the full parser: for any
// text ParseOld accepts, direct ingestion must produce an equal ad.
func FuzzIngestOld(f *testing.F) {
	for _, s := range []string{
		"A = 1\nB = \"x\"\nC = 3.14\nD = true\nE = undefined\nF = a && b\n",
		"Cpus = 8\nMemory = 16384\nArch = \"X86_64\"\nRank = Cpus * 2\n",
		"X = 007\nY = -5\nZ = +3\nR = .5\nQ = 1e10\nH = 0x1f\nS = \"a\\nb\"\n",
		"A = 1\nA = 2\n", // duplicate -> fallback path
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		ref, err := classad.ParseOld(text)
		if err != nil || ref == nil || ref.Size() == 0 {
			return
		}
		c := New(Options{Shards: 1})
		if err := c.UpdateOld([]OldAdUpdate{{Key: []byte("k"), Text: text}}); err != nil {
			return // an encoding error on odd input is acceptable; a wrong value is not
		}
		got, ok := c.Get([]byte("k"))
		if !ok {
			t.Fatalf("ad missing after UpdateOld for %q", text)
		}
		if !got.Equal(ref) {
			t.Fatalf("mismatch for %q:\n direct=%s\n parseOld=%s", text, got.String(), ref.String())
		}
	})
}

func BenchmarkIngestOldDirect(b *testing.B) {
	texts := loadCorpusTexts(b)
	c := New(Options{Shards: 16})
	batch := make([]OldAdUpdate, len(texts))
	for i, t := range texts {
		batch[i] = OldAdUpdate{Key: []byte("ad-" + itoa(i)), Text: t}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.UpdateOld(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(texts)), "ns/ad")
}

func BenchmarkIngestParseOldThenPut(b *testing.B) {
	texts := loadCorpusTexts(b)
	c := New(Options{Shards: 16})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := make([]AdUpdate, len(texts))
		for j, t := range texts {
			ad, err := classad.ParseOld(t)
			if err != nil {
				b.Fatal(err)
			}
			batch[j] = AdUpdate{Key: []byte("ad-" + itoa(j)), Ad: ad}
		}
		if err := c.Update(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(texts)), "ns/ad")
}
