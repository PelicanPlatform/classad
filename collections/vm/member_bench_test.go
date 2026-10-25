package vm

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkMemberEval is the per-record cost of member against lists of 1, 10 and 300 integers --
// the row path's inner loop -- through a reused Matcher, for a first argument that is and is not in
// the list.
func BenchmarkMemberEval(b *testing.B) {
	for _, k := range []int{1, 10, 300} {
		q, err := Parse("member(ClusterId, {" + benchInts(1000, 1000+k) + "})")
		if err != nil {
			b.Fatal(err)
		}
		for _, hit := range []bool{true, false} {
			ad := classad.New()
			if hit {
				ad.InsertAttr("ClusterId", int64(1000+k/2))
			} else {
				ad.InsertAttr("ClusterId", int64(5))
			}
			m := q.Matcher()
			b.Run(fmt.Sprintf("list=%d/hit=%v", k, hit), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					m.Matches(ad)
				}
			})
		}
	}
}

func benchInts(lo, hi int) string {
	parts := make([]string, 0, hi-lo)
	for i := lo; i < hi; i++ {
		parts = append(parts, strconv.Itoa(i))
	}
	return strings.Join(parts, ", ")
}
