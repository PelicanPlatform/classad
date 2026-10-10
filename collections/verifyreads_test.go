package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// openVerified reopens the store with VerifyReads set as asked and reports what each
// key reads back as.
func (s *corruptStore) censusWith(verify bool) (found, missing, wrong int) {
	s.t.Helper()
	c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 14, VerifyReads: verify})
	if err != nil {
		s.t.Fatalf("open (VerifyReads=%v): %v", verify, err)
	}
	defer c.Close()
	for i, k := range s.keys {
		ad, ok := c.Get([]byte(k))
		if !ok {
			missing++
			continue
		}
		if adDiff(ad, i) != "" {
			wrong++
			continue
		}
		found++
	}
	return
}

// TestAdBodyCorruptionIsServedWithoutVerification is the failure this option exists
// for, stated plainly: a bit flip inside a sealed record's encoded ad is served as a
// plausible value. The record's own checksum would catch it; nothing on the read path
// looks. The sidecar matters -- with one, Open trusts its extent and never walks, so
// the record is read rather than dropped.
func TestAdBodyCorruptionIsServedWithoutVerification(t *testing.T) {
	s := buildCorruptStore(t, 400)
	seg := s.segments()[0]
	idx := len(recordOffsets(t, seg)) / 2
	flipBit(t, seg, adBodyOffset(t, seg, idx))

	found, missing, wrong := s.censusWith(false)
	t.Logf("VerifyReads=false: %d intact, %d missing, %d WRONG", found, missing, wrong)
	if wrong == 0 {
		t.Skip("this build did not serve the corrupt ad; nothing to compare against")
	}

	found2, missing2, wrong2 := s.censusWith(true)
	t.Logf("VerifyReads=true:  %d intact, %d missing, %d WRONG", found2, missing2, wrong2)
	if wrong2 != 0 {
		t.Errorf("VerifyReads still served %d wrong value(s)", wrong2)
	}
	if missing2 != missing+wrong {
		t.Errorf("the corrupt record should become a MISS: missing went %d -> %d, wrong %d -> %d",
			missing, missing2, wrong, wrong2)
	}
}

// TestVerifyReadsCostsNothingWhenClean: verification must not change what a healthy
// store returns. An integrity check that also changes answers is worse than none.
func TestVerifyReadsCostsNothingWhenClean(t *testing.T) {
	s := buildCorruptStore(t, 400)
	f1, m1, w1 := s.censusWith(false)
	f2, m2, w2 := s.censusWith(true)
	if f1 != f2 || m1 != m2 || w1 != w2 {
		t.Errorf("VerifyReads changed a healthy store's answers: (%d,%d,%d) -> (%d,%d,%d)",
			f1, m1, w1, f2, m2, w2)
	}
	if f2 != len(s.keys) {
		t.Errorf("VerifyReads lost keys on a healthy store: %d/%d", f2, len(s.keys))
	}
}

// BenchmarkGet measures the cost of the option on the hot path, so the default is a
// decision with a number behind it rather than a guess.
func BenchmarkGet(b *testing.B) {
	for _, verify := range []bool{false, true} {
		name := "VerifyReads=false"
		if verify {
			name = "VerifyReads=true"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20, VerifyReads: verify})
			if err != nil {
				b.Fatal(err)
			}
			const n = 20000
			keys := make([][]byte, n)
			for i := 0; i < n; i++ {
				keys[i] = []byte(fmt.Sprintf("job%06d", i))
				ad := classad.New()
				ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
				ad.InsertAttrString("Cmd", "/bin/sleep")
				ad.InsertAttr("RequestCpus", int64(1+i%4))
				ad.InsertAttr("QDate", int64(1790000000+i))
				if err := c.Put(keys[i], ad); err != nil {
					b.Fatal(err)
				}
			}
			c.Close()

			c2, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20, VerifyReads: verify})
			if err != nil {
				b.Fatal(err)
			}
			defer c2.Close()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := c2.Get(keys[i%n]); !ok {
					b.Fatal("miss")
				}
			}
		})
	}
}
