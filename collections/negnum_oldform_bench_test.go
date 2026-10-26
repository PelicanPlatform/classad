package collections

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// oldNegNode is the pre-fold encoding of -mag: nUnOp, op 0 ("-"), then the literal mag (an int64 or a
// float64). The current encoder cannot write it, so it is assembled from the node layout.
func oldNegNode(mag any) []byte {
	b := []byte{0x09, 0x00}
	switch m := mag.(type) {
	case int64:
		return wire.AppendIntNode(b, m)
	case float64:
		return wire.AppendRealNode(b, m)
	}
	panic(fmt.Sprintf("oldNegNode: %T", mag))
}

type rawAttr struct {
	name string
	node []byte
}

// inlineAd frames attributes as an inline-names wire ad, the persistent store's record form, with no
// hot header: [magic][version][flags=inline names][hotCount=0][attrCount] then (name + node)*.
func inlineAd(attrs []rawAttr) []byte {
	b := []byte{0xCA, 0x01, 0x02}
	b = binary.AppendUvarint(b, 0)
	b = binary.AppendUvarint(b, uint64(len(attrs)))
	for _, a := range attrs {
		b = binary.AppendUvarint(b, uint64(len(a.name)))
		b = append(b, a.name...)
		b = append(b, a.node...)
	}
	return b
}

// putWire stores already-encoded wire bytes under key exactly as Put stores an encoded ad.
func putWire(tb testing.TB, c *Collection, key, raw []byte) {
	tb.Helper()
	codec := c.currentCodec()
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.commitOne(pendingPut{hash: h, key: key, ad: codec.Compress(nil, raw), codec: codec, ct: c.namesCurrentTime(raw)})
	c.indexSealedSegments()
	c.collapseSealedChains()
	if err := c.writeError(); err != nil {
		tb.Fatal(err)
	}
}

// appendWire appends already-encoded wire bytes to an archive exactly as Append stores an encoded ad.
func appendWire(tb testing.TB, a *Archive, raw []byte) {
	tb.Helper()
	putWire(tb, a.c, nil, raw)
	a.afterAppend()
}

// negFixture is an archive whose X mixes every spelling of a number a record can hold: folded
// negatives, old-form negatives (ints, -0, reals, -0.0, past 2^53, near the int64 minimum), `-(3)`,
// positives, reals, absent, and -- with expr -- a genuine expression, which only evaluation answers.
type negFixture struct {
	n      int
	expr   bool
	negZ   bool // include an old-form -0.0
	exprs  int  // records whose X is a genuine expression
	oldNeg int  // records whose X is a number spelled as a sign over a literal: old-form, or -(3)
}

func (f *negFixture) record(i int) []byte {
	pad := wire.AppendStringNode(nil, "pppppppppppppppp")
	attrs := []rawAttr{{"N", wire.AppendIntNode(nil, int64(i))}, {"Pad", pad}}
	text := ""
	switch {
	case f.expr && i%50 == 49:
		text = "N - 100"
		f.exprs++
	case i%10 == 0:
		attrs = append(attrs, rawAttr{"X", oldNegNode(int64(i%7 + 1))})
		f.oldNeg++
	case i%10 == 1:
		text = fmt.Sprintf("-%d", i%5+1) // folded by today's encoder
	case i%10 == 2:
		attrs = append(attrs, rawAttr{"X", wire.AppendIntNode(nil, int64(i%13))})
	case i%10 == 3:
		attrs = append(attrs, rawAttr{"X", oldNegNode(int64(0))}) // -0: the int 0
		f.oldNeg++
	case i%10 == 4:
		attrs = append(attrs, rawAttr{"X", oldNegNode(2.5)})
		f.oldNeg++
	case i%20 == 5:
		attrs = append(attrs, rawAttr{"X", oldNegNode(int64(1<<53 + 1))}) // no float64 holds it
		f.oldNeg++
	case i%20 == 15:
		attrs = append(attrs, rawAttr{"X", oldNegNode(int64(math.MaxInt64))})
		f.oldNeg++
	case i%10 == 6:
		text = "-(3)" // a spelling the fold leaves alone
		f.oldNeg++
	case i%10 == 7:
		// absent
	case i%10 == 8 && f.negZ:
		attrs = append(attrs, rawAttr{"X", oldNegNode(0.0)}) // -0.0, which equals 0
		f.oldNeg++
	case i%10 == 8:
		attrs = append(attrs, rawAttr{"X", wire.AppendRealNode(nil, -0.5)})
	case i%10 == 9:
		attrs = append(attrs, rawAttr{"X", wire.AppendRealNode(nil, 1.5)})
	}
	if text == "" {
		return inlineAd(attrs)
	}
	// Spelled as text, X goes through today's encoder: the folded literal, or a node the fold leaves.
	ad, err := classad.Parse(fmt.Sprintf(`[ N = %d; Pad = "pppppppppppppppp"; X = %s ]`, i, text))
	if err != nil {
		panic(err)
	}
	return wire.EncodeInlineWithHot(nil, ad.AST(), nil)
}

func newNegArchive(tb testing.TB, f *negFixture, opts ArchiveOptions) *Archive {
	tb.Helper()
	if !mmapSupported {
		tb.Skip("persistence is unix-only")
	}
	if opts.Dir == "" {
		opts.Dir = tb.TempDir()
	}
	if opts.SegmentSize == 0 {
		opts.SegmentSize = 1 << 12
	}
	a, err := CreateArchive(opts)
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < f.n; i++ {
		appendWire(tb, a, f.record(i))
	}
	// A tail with nothing unusual, so every record of interest is in a sealed segment.
	for i := 0; i < 80; i++ {
		appendWire(tb, a, inlineAd([]rawAttr{{"N", wire.AppendIntNode(nil, int64(f.n+i))}, {"X", wire.AppendIntNode(nil, 7)}}))
	}
	a.Reindex()
	return a
}

// BenchmarkOldFormNegatives measures the reads old-form negatives slowed down: a selective value-index
// probe on them, and the column aggregates over them (each falls back to an evaluating scan when the
// columns decline, as a caller does).
func BenchmarkOldFormNegatives(b *testing.B) {
	idx := newNegArchive(b, &negFixture{n: 20000, expr: true, negZ: true}, ArchiveOptions{ValueAttrs: []string{"X"}})
	defer idx.Close()
	col := newNegArchive(b, &negFixture{n: 20000, negZ: true}, ArchiveOptions{})
	defer col.Close()
	col.AddSchemaRequireAttrs("X")
	if !col.BuildAndEnableSchemaScan(1<<16, 8) {
		b.Fatal("schema scan did not enable")
	}
	for col.c.ColumnarizeSealed() > 0 {
	}
	selective := mustQuery(b, `X < -1000`)
	neg := mustQuery(b, `X < 0`)
	scanCount := func(a *Archive, q *vm.Query) int {
		n := 0
		for ad := range a.c.Scan() {
			if q.Matches(ad) {
				n++
			}
		}
		return n
	}
	b.Run("index-probe", func(b *testing.B) {
		for b.Loop() {
			n := 0
			for range idx.QueryProjectStats(selective, []string{"N"}, nil) {
				n++
			}
		}
	})
	b.Run("count", func(b *testing.B) {
		for b.Loop() {
			if _, ok := col.c.CountQuery(neg); !ok {
				scanCount(col, neg)
			}
		}
	})
	b.Run("stats", func(b *testing.B) {
		for b.Loop() {
			if _, ok := col.c.NumStatsQuery(neg, "X"); !ok {
				for ad := range col.c.Scan() {
					if neg.Matches(ad) {
						_ = ad.EvaluateAttr("X")
					}
				}
			}
		}
	})
	b.Run("group", func(b *testing.B) {
		all := mustQuery(b, `N >= 0`)
		for b.Loop() {
			if _, ok := col.c.GroupStatsQuery(all, "X", nil); !ok {
				groups := map[string]int{}
				for ad := range col.c.Scan() {
					groups[ad.EvaluateAttr("X").String()]++
				}
			}
		}
	})
}
