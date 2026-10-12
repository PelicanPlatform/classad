package classad

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// poolCorpus is the committed `condor_status -any -l` sample used by the collections
// benchmarks: real startd/master/schedd ads in old-ClassAd syntax, separated by blank lines.
const poolCorpus = "../collections/vm/testdata/pool_sample.ads.gz"

// loadPoolText returns each ad of the pool corpus as old-syntax text.
func loadPoolText(tb testing.TB) []string {
	tb.Helper()
	f, err := os.Open(poolCorpus)
	if err != nil {
		tb.Skipf("pool corpus unavailable: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		tb.Fatal(err)
	}
	var ads []string
	var cur strings.Builder
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if cur.Len() > 0 {
				ads = append(ads, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if cur.Len() > 0 {
		ads = append(ads, cur.String())
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	return ads
}

func loadPoolAds(tb testing.TB) []*ClassAd {
	tb.Helper()
	texts := loadPoolText(tb)
	ads := make([]*ClassAd, 0, len(texts))
	for _, s := range texts {
		ad, err := ParseOld(s)
		if err != nil {
			tb.Fatalf("parse corpus ad: %v", err)
		}
		ads = append(ads, ad)
	}
	return ads
}

// poolSlots returns the corpus ads that have a Requirements expression (the slots).
func poolSlots(tb testing.TB) []*ClassAd {
	var slots []*ClassAd
	for _, ad := range loadPoolAds(tb) {
		if _, ok := ad.Lookup("Requirements"); ok {
			if s, _ := ad.EvaluateAttrString("MyType"); s == "Machine" {
				slots = append(slots, ad)
			}
		}
	}
	return slots
}

// unsortedAdText is a synthetic n-attribute ad whose attributes are in random order.
func unsortedAdText(n int) string {
	r := rand.New(rand.NewSource(1))
	perm := r.Perm(n)
	var b strings.Builder
	b.WriteByte('[')
	for i, p := range perm {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString("Attr")
		b.WriteString(strconv.Itoa(p))
		if p%4 == 0 {
			b.WriteString(" = Attr")
			b.WriteString(strconv.Itoa(p + 1))
			b.WriteString(" + 1")
		} else {
			b.WriteString(" = ")
			b.WriteString(strconv.Itoa(p))
		}
	}
	b.WriteByte(']')
	return b.String()
}

func BenchmarkReadPathParseOldPool(b *testing.B) {
	texts := loadPoolText(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseOld(texts[i%len(texts)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadPathParseUnsorted100(b *testing.B) {
	src := unsortedAdText(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadPathFromASTPool(b *testing.B) {
	ads := loadPoolAds(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FromAST(ads[i%len(ads)].AST())
	}
}

func BenchmarkReadPathMarshalJSONPool(b *testing.B) {
	ads := loadPoolAds(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(ads[i%len(ads)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadPathMarshalOldPool(b *testing.B) {
	ads := loadPoolAds(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ads[i%len(ads)].MarshalOld()
	}
}

func BenchmarkReadPathStringPool(b *testing.B) {
	ads := loadPoolAds(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ads[i%len(ads)].String()
	}
}

func BenchmarkReadPathLookupPool(b *testing.B) {
	ads := loadPoolAds(b)
	names := []string{"Memory", "Cpus", "Arch", "OpSys", "Name", "NoSuchAttr"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ads[i%len(ads)].Lookup(names[i%len(names)])
	}
}

func BenchmarkReadPathEvaluateChain(b *testing.B) {
	ad, err := Parse(`[A = 1; B = A + 1; C = B * 2; D = C > 3 && B < 10]`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := ad.EvaluateAttrBool("D"); !ok {
			b.Fatal("D not bool")
		}
	}
}

const benchJobAd = `[
	Owner = "alice"; ProjectName = "bench"; RequestCpus = 1; RequestMemory = 2048;
	RequestDisk = 1000000; RequestGPUs = 0; JobDurationCategory = "Medium";
	Requirements = TARGET.Arch == "X86_64" && TARGET.OpSys == "LINUX" &&
		TARGET.Memory >= RequestMemory && TARGET.Cpus >= RequestCpus && TARGET.Disk >= RequestDisk;
	Rank = TARGET.Memory
]`

func BenchmarkReadPathMatchPool(b *testing.B) {
	slots := poolSlots(b)
	if len(slots) == 0 {
		b.Skip("no slots in corpus")
	}
	job, err := Parse(benchJobAd)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := NewMatchClassAd(job, slots[i%len(slots)])
		_ = m.Match()
		_, _ = m.EvaluateRankLeft()
	}
}

func BenchmarkReadPathInsertBuild100(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	names := make([]string, 100)
	for i, p := range r.Perm(100) {
		names[i] = "Attr" + strconv.Itoa(p)
	}
	b.Run("build", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ad := New()
			for j, n := range names {
				ad.InsertAttr(n, int64(j))
			}
		}
	})
	b.Run("build+marshal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ad := New()
			for j, n := range names {
				ad.InsertAttr(n, int64(j))
			}
			_ = ad.MarshalOld()
		}
	})
}
