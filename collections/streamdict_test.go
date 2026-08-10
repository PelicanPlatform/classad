package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestStreamDictionaryFit reports how well the collection's ZSTD dictionary serves each of a
// columnar block's three compressed regions.
//
// The dictionary is trained by TrainDict on CollectSamples output: whole ClassAd RECORDS, flattened
// to inline wire, concatenated as the builder's history (TrainDictSize does no ZDICT cover
// selection -- the content is the samples). The same codec then compresses three regions whose byte
// distributions are nothing alike:
//
//	cold numeric  column-major fixed-width little-endian integers
//	strings       concatenated length-prefixed string values
//	cold tail     uvarint(id)+node pairs, i.e. record-shaped
//
// A dictionary of record bytes should help the cold tail (same shape) and the strings (their values
// appear in records), and do little for column-major integers. This measures that rather than
// assuming it, since a dictionary that does not fit a region is wasted work on every compress.
func TestStreamDictionaryFit(t *testing.T) {
	// A fixture that populates ALL THREE regions: many numeric attributes but few hot slots (so
	// most are cold), several string attributes, and values that escape their fitted width.
	c := New(Options{Shards: 1, SegmentSize: 1 << 14})
	defer c.Close()
	for i := 0; i < 4000; i++ {
		var b strings.Builder
		for k := 0; k < 20; k++ {
			v := (i*7 + k) % 1000
			if k == 3 && i%50 == 49 {
				v = 1 << 30 // escapes a narrow fitted width, landing in the cold tail
			}
			fmt.Fprintf(&b, "Num%02d = %d\n", k, v)
		}
		for k := 0; k < 6; k++ {
			fmt.Fprintf(&b, "Str%02d = \"user%03d-path-%d-with-some-length\"\n", k, i%200, k)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, b.String())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RetrainDict(2000); err != nil {
		t.Fatalf("RetrainDict: %v", err)
	}
	mc, _ := vm.Parse("Num00 >= 0")
	for i := 0; i < 25; i++ {
		for range c.Query(mc) {
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 2) { // only 2 hot slots => 18 cold numeric columns
		t.Fatal("BuildAndEnableSchemaScan false")
	}

	plain, err := NewZSTDCodec(nil) // same codec, no dictionary
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		act := sh.act
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			cs := seg.colblk.Load()
			if cs == nil {
				continue
			}
			b := cs.block
			found = true
			for _, s := range []struct {
				name string
				comp []byte
			}{
				{"cold-numeric", b.coldNumComp},
				{"strings     ", b.strComp},
				{"cold-tail   ", b.coldComp},
			} {
				raw, err := b.codec.Decompress(nil, s.comp)
				if err != nil {
					t.Fatalf("%s: decompress: %v", s.name, err)
				}
				if len(raw) == 0 {
					t.Logf("%s raw=0 (empty region)", s.name)
					continue
				}
				noDict := plain.Compress(nil, raw)
				t.Logf("%s raw=%6d  with-dict=%6d (%.2fx)  no-dict=%6d (%.2fx)  dict gain=%+.1f%%",
					s.name, len(raw), len(s.comp), float64(len(raw))/float64(len(s.comp)),
					len(noDict), float64(len(raw))/float64(len(noDict)),
					100*(float64(len(noDict))-float64(len(s.comp)))/float64(len(noDict)))
			}
			// One block is enough to characterise the shapes.
			if found {
				return
			}
		}
	}
	if !found {
		t.Skip("no sealed segment carried a columnar block")
	}
}
