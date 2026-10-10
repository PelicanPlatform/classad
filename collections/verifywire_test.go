package collections

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"testing"
)

// scanCensus walks the whole collection the way a query does (ForEachAd, which goes
// through the adBytes/wireAt iterator path -- NOT the point-read path Get uses) and
// reports how many ads came back right, wrong, or not at all.
func (s *corruptStore) scanCensus(verify bool) (found, missing, wrong int) {
	s.t.Helper()
	c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 14, VerifyReads: verify})
	if err != nil {
		s.t.Fatalf("open (VerifyReads=%v): %v", verify, err)
	}
	defer c.Close()

	idxOf := make(map[string]int, len(s.keys))
	for i, k := range s.keys {
		idxOf[k] = i
	}
	seen := make(map[string]bool, len(s.keys))
	c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		i, ok := idxOf[key]
		if !ok {
			return true
		}
		seen[key] = true
		if adDiff(ad, i) != "" {
			wrong++
		} else {
			found++
		}
		return true
	})
	return found, len(s.keys) - len(seen), wrong
}

// TestQueryServesCorruptAdWithoutVerification is the gap VerifyReads did not close
// when it only guarded the point-read path. A scan goes through a different primitive
// entirely, so a corrupt ad was served to every query even with VerifyReads on.
func TestQueryServesCorruptAdWithoutVerification(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	idx := len(recordOffsets(t, seg)) / 2
	flipBit(t, seg, adBodyOffset(t, seg, idx))

	f0, m0, w0 := s.scanCensus(false)
	t.Logf("scan, VerifyReads=false: %d right, %d missing, %d WRONG", f0, m0, w0)
	if w0 == 0 {
		t.Skip("this build did not serve a corrupt ad to the scan; nothing to compare")
	}

	before := VerifyReadSkips()
	f1, m1, w1 := s.scanCensus(true)
	skipped := VerifyReadSkips() - before
	t.Logf("scan, VerifyReads=true:  %d right, %d missing, %d WRONG (%d skipped)", f1, m1, w1, skipped)

	if w1 != 0 {
		t.Errorf("a scan still served %d corrupt ad(s) with VerifyReads on", w1)
	}
	if m1 != m0+w0 {
		t.Errorf("the corrupt record should become a MISS: missing %d -> %d, wrong %d -> %d", m0, m1, w0, w1)
	}
	if skipped == 0 {
		t.Error("VerifyReadSkips did not count the refusal; a shrunken answer must be observable")
	}
}

// TestVerifyReadsLeavesAHealthyScanAlone: an integrity check that changes answers on a
// healthy store would be worse than none.
func TestVerifyReadsLeavesAHealthyScanAlone(t *testing.T) {
	s := buildCorruptStore(t, 400)
	f0, m0, w0 := s.scanCensus(false)
	before := VerifyReadSkips()
	f1, m1, w1 := s.scanCensus(true)
	if f0 != f1 || m0 != m1 || w0 != w1 {
		t.Errorf("VerifyReads changed a healthy scan: (%d,%d,%d) -> (%d,%d,%d)", f0, m0, w0, f1, m1, w1)
	}
	if f1 != len(s.keys) {
		t.Errorf("healthy scan returned %d/%d", f1, len(s.keys))
	}
	if n := VerifyReadSkips() - before; n != 0 {
		t.Errorf("a healthy store produced %d skips", n)
	}
}

// benchStore builds a persistent collection of n ads once, for the scan benchmarks.
func verifyBenchStore(b *testing.B, dir string, n int) {
	b.Helper()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		ad.InsertAttrString("Cmd", "/bin/sleep")
		ad.InsertAttrString("Args", fmt.Sprintf("--input /data/set%d/part-%05d --threads 4", i%29, i))
		ad.InsertAttr("RequestCpus", int64(1+i%4))
		ad.InsertAttr("QDate", int64(1790000000+i))
		if err := c.Put([]byte(fmt.Sprintf("job%06d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkScan measures the per-record cost of verification on the path a QUERY
// takes. This is the number the default hinges on: every record a scan touches is
// already being decompressed, so a hardware CRC should be a much smaller share here
// than on a point Get.
func BenchmarkScan(b *testing.B) {
	const n = 20000
	dir := b.TempDir()
	verifyBenchStore(b, dir, n)

	for _, verify := range []bool{false, true} {
		name := "VerifyReads=false"
		if verify {
			name = "VerifyReads=true"
		}
		b.Run(name, func(b *testing.B) {
			c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20, VerifyReads: verify})
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			b.ResetTimer()
			seen := 0
			for i := 0; i < b.N; i++ {
				c.ForEachAd(func(string, *classad.ClassAd) bool { seen++; return true })
			}
			b.StopTimer()
			b.ReportMetric(float64(seen)/float64(b.N), "records/op")
		})
	}
}

// BenchmarkRecordVerifyCRC isolates the check itself. Differencing two whole scans
// could not resolve it: the per-arm spread across runs was larger than the effect, and
// flipping the arm order reversed the sign -- whichever arm ran second was slower,
// which is the machine, not the code. Measuring the operation directly avoids that.
func BenchmarkRecordVerifyCRC(b *testing.B) {
	dir := b.TempDir()
	verifyBenchStore(b, dir, 2000)
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()

	// Take one shard's first segment and collect its record offsets.
	sh := c.shards[0]
	sh.mu.RLock()
	var seg *segment
	for _, s := range sh.segs {
		if s != nil && s.used > 0 {
			seg = s
			break
		}
	}
	sh.mu.RUnlock()
	if seg == nil {
		b.Skip("no segment to measure")
	}
	var offs []uint32
	var bytes int
	for off := 0; off < seg.used; {
		o := uint32(off)
		total := int(recTotalLen(seg.data, o))
		if total == 0 {
			break
		}
		offs = append(offs, o)
		bytes += total
		off += total
	}
	if len(offs) == 0 {
		b.Skip("no records")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !recVerifyCRC(seg.data, offs[i%len(offs)]) {
			b.Fatal("a healthy record failed its own checksum")
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(bytes)/float64(len(offs)), "bytes/record")
}
