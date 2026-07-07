package collections

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// TestDecodeRawMatchesFull checks that decodeAdRaw produces, for every attribute,
// exactly the same canonical value text that the full decode-then-format path
// does -- so switching a query response to the raw path changes nothing on the
// wire.
func TestDecodeRawMatchesFull(t *testing.T) {
	t.Parallel()
	sample := loadCorpus(t)
	c := populate(t, sample, 500)
	checked := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		forEachVisible(s0, wins, func(ad []byte, codec Codec) bool {
			full, err := c.decodeAd(ad, codec)
			if err != nil {
				t.Fatalf("decodeAd: %v", err)
			}
			exprs, myType, targetType, ok := c.decodeAdRaw(ad, codec, nil)
			if !ok {
				t.Fatal("decodeAdRaw returned ok=false for an in-memory ad")
			}
			raw := map[string]string{}
			for _, e := range exprs {
				name, val, found := strings.Cut(e, " = ")
				if !found {
					t.Fatalf("malformed raw expr %q", e)
				}
				raw[name] = val
			}
			for _, attr := range full.GetAttributes() {
				fe, _ := full.Lookup(attr)
				want := fe.String()
				switch attr {
				case "MyType":
					if ast.QuoteString(myType) != want {
						t.Errorf("MyType: raw %q != full %q", ast.QuoteString(myType), want)
					}
				case "TargetType":
					if ast.QuoteString(targetType) != want {
						t.Errorf("TargetType: raw %q != full %q", ast.QuoteString(targetType), want)
					}
				default:
					got, exists := raw[attr]
					if !exists {
						t.Errorf("raw missing attribute %s", attr)
					} else if got != want {
						t.Errorf("attribute %s: raw %q != full %q", attr, got, want)
					}
				}
			}
			checked++
			return checked < 200
		})
		releaseWindows(wins)
	}
	if checked == 0 {
		t.Fatal("no ads checked")
	}
	t.Logf("verified raw==full on %d ads", checked)
}

// BenchmarkDecodeSend compares the two send paths on real ~21 KB OSPool ads:
// "full" is decode -> *classad.ClassAd -> format every attribute (what the query
// handler does today via PutClassAd); "raw" is decodeAdRaw straight to the same
// "Name = Value" strings, never building a ClassAd.
func BenchmarkDecodeSend(b *testing.B) {
	sample := loadCorpus(b)
	c := populate(b, sample, 2000)

	b.Run("full-classad", func(b *testing.B) {
		b.ReportAllocs()
		var sink int
		for i := 0; i < b.N; i++ {
			for _, sh := range c.shards {
				s0, wins := sh.snapshot()
				forEachVisible(s0, wins, func(ad []byte, codec Codec) bool {
					a, err := c.decodeAd(ad, codec)
					if err != nil {
						b.Fatal(err)
					}
					for _, attr := range a.GetAttributes() {
						e, _ := a.Lookup(attr)
						sink += len(attr) + len(e.String())
					}
					return true
				})
				releaseWindows(wins)
			}
		}
		_ = sink
	})

	b.Run("raw-text", func(b *testing.B) {
		b.ReportAllocs()
		var buf []byte
		var sink int
		for i := 0; i < b.N; i++ {
			for _, sh := range c.shards {
				s0, wins := sh.snapshot()
				forEachVisible(s0, wins, func(ad []byte, codec Codec) bool {
					exprs, _, _, ok := c.decodeAdRaw(ad, codec, buf[:0])
					if !ok {
						b.Fatal("decodeAdRaw ok=false")
					}
					for _, e := range exprs {
						sink += len(e)
					}
					return true
				})
				releaseWindows(wins)
			}
		}
		_ = sink
	})
}
