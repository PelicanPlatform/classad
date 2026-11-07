package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkSelectChainScan measures what seeding a select's member costs on the hot
// scan path. Chaining is resolved by the partial decode's seed set, not per record, so
// the cost is exactly one more attribute in that set -- and only when the record
// defines it.
//
// The arms separate the two cases that matter:
//
//	bothMem    the selected record defines the member AND the ad defines the same
//	           name. The chain is never walked, so the extra seed buys nothing and
//	           costs a whole extra attribute decode per record: the worst case, and
//	           the only arm whose row set is the same before and after the fix.
//	outerMem   the ad defines the name and the record does not, so reading the extra
//	           name is what makes the query answerable at all. Its row set differs from
//	           a store that does not chain, so its timing is not comparable with one.
//	noOuterMem neither defines it: the extra seed is one failed lookup in the
//	           record's wire index and nothing else.
//	noSelect   a constraint with no select at all, which the change must not touch.
func BenchmarkSelectChainScan(b *testing.B) {
	const n = 20000
	ad := func(i int, outerMem, innerMem bool) string {
		var s strings.Builder
		fmt.Fprintf(&s, `[ Name = "k%06d"; Ord = %d; ClusterId = %d; ProcId = %d; JobStatus = %d`,
			i, i, 1000+i/7, i%7, 1+i%5)
		if outerMem {
			fmt.Fprintf(&s, `; Mem = %d`, 1024*(1+i%100))
		}
		if innerMem {
			fmt.Fprintf(&s, `; Res = [ Mem = %d; Other = %d; Disk = %d ]`, 1024*(1+i%100), i%3, 100+i%97)
		} else {
			fmt.Fprintf(&s, `; Res = [ Other = %d; Disk = %d ]`, i%3, 100+i%97)
		}
		for f := 0; f < 24; f++ {
			fmt.Fprintf(&s, `; Filler%02d = "filler %06d-%02d"`, f, i, f)
		}
		s.WriteString(" ]")
		return s.String()
	}
	for _, arm := range []struct {
		name               string
		outerMem, innerMem bool
		query              string
	}{
		{"bothMem/Res.Mem", true, true, `Res.Mem >= 102400`},
		{"outerMem/Res.Mem", true, false, `Res.Mem >= 102400`},
		{"noOuterMem/Res.Mem", false, false, `Res.Mem >= 102400`},
		{"outerMem/noSelect", true, false, `Mem >= 102400`},
	} {
		b.Run(arm.name, func(b *testing.B) {
			c := New(Options{Shards: 1, SegmentSize: 1 << 20})
			for i := 0; i < n; i++ {
				if err := c.Put([]byte(fmt.Sprintf("k%06d", i)), mustAd(b, ad(i, arm.outerMem, arm.innerMem))); err != nil {
					b.Fatal(err)
				}
			}
			q, err := vm.Parse(arm.query)
			if err != nil {
				b.Fatal(err)
			}
			// Report the match count once, so an arm that silently stopped matching
			// cannot read as a speedup.
			rows := 0
			for range c.Query(q) {
				rows++
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				got := 0
				for range c.Query(q) {
					got++
				}
				if got != rows {
					b.Fatalf("row count moved: %d != %d", got, rows)
				}
			}
			b.StopTimer()
			// Reported after the loop: ResetTimer discards metrics set before it. An arm
			// that silently stopped matching must not read as a speedup.
			b.ReportMetric(float64(rows), "rows")
		})
	}
}
