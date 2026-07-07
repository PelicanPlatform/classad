package classad

import (
	"strconv"
	"strings"
	"testing"
)

// bigAd builds an ad with n scalar attributes (a stand-in for a large startd ad).
func bigAd(n int) *ClassAd {
	var src strings.Builder
	src.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			src.WriteByte(';')
		}
		src.WriteString("Attr")
		src.WriteString(strconv.Itoa(i))
		src.WriteString(` = "value_`)
		src.WriteString(strconv.Itoa(i))
		src.WriteByte('"')
	}
	src.WriteByte(']')
	ad, err := Parse(src.String())
	if err != nil {
		panic(err)
	}
	return ad
}

// BenchmarkMarshalOld renders ads of growing size; with the O(n^2) `+=` loop the
// per-op time and bytes grew quadratically. Run with -benchmem to see B/op scale
// ~linearly with attribute count now.
func BenchmarkMarshalOld(b *testing.B) {
	for _, n := range []int{50, 100, 200, 400} {
		ad := bigAd(n)
		b.Run("attrs="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = ad.MarshalOld()
			}
		})
	}
}
