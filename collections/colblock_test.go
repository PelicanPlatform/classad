package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

func mustAdOld(t testing.TB, src string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(src)
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

func mustZSTD(t *testing.T) Codec {
	t.Helper()
	c, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// encodeRows builds adSchema row records for a set of wire ads.
func encodeRows(s *adSchema, wires [][]byte) [][]byte {
	recs := make([][]byte, len(wires))
	for i, w := range wires {
		recs[i] = s.encode(wire.Ad(w))
	}
	return recs
}

// hotHalf picks half the numeric fields as "hot" (the popularity input; here just a split to
// exercise both hot and cold paths).
func hotHalf(s *adSchema) []int {
	var hot []int
	n := 0
	for i := range s.fields {
		if k := s.fields[i].kind; k == akInt || k == akReal {
			if n%2 == 0 {
				hot = append(hot, i)
			}
			n++
		}
	}
	return hot
}

// TestColumnarBlockRoundTrip: reconstructing each record from the columnar block reproduces the
// original ad exactly (every attribute, same value), for both codecs and hot/cold splits.
func TestColumnarBlockRoundTrip(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 40; i++ {
		ad := mustAdOld(t, fmt.Sprintf(
			"Cpus = %d\nMemory = %d\nBig = %t\nLoad = %f\nArch = \"X86_64\"\nMachine = \"node%02d.example.org\"\nExtra = %d\nReq = (Cpus >= 1)",
			1+i%8, 1024+i*64, i%2 == 0, float64(i)/3, i, i*100000)) // Extra overflows small widths -> escapes
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: true})
	recs := encodeRows(s, wires)

	for _, codec := range []Codec{identityCodec{}, mustZSTD(t)} {
		for _, hot := range [][]int{nil, hotHalf(s)} { // all-cold and half-hot
			blk := encodeColumnarBlock(s, recs, resolveColLayout(s, hot), codec, nil)
			for k := range recs {
				got, err := blk.reconstruct(k, nil)
				if err != nil {
					t.Fatalf("%s reconstruct(%d): %v", codec.Name(), k, err)
				}
				assertRoundTrip(t, s, wires[k], fmt.Sprintf("%s hot=%d rec[%d]", codec.Name(), len(hot), k))
				// The reconstructed row record must be byte-identical to the encoded row.
				if string(got) != string(recs[k]) {
					t.Errorf("%s hot=%d rec[%d]: reconstructed record differs from row form", codec.Name(), len(hot), k)
				}
			}
		}
	}
}

// TestColumnarBlockScanInt: scanning a numeric column (hot or cold) yields the same matches as
// reading the value straight from each row record.
func TestColumnarBlockScanInt(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 300; i++ {
		ad := mustAdOld(t, fmt.Sprintf("Cpus = %d\nMemory = %d\nDisk = %d", 1+i%16, 1024+(i%50)*512, i*1000))
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95})
	recs := encodeRows(s, wires)
	memID, _ := c.intern.LookupID("Memory")
	fidx, ok := s.byID[memID]
	if !ok || s.fields[fidx].kind != akInt {
		t.Skip("Memory not an int schema field")
	}

	// truth: read Memory from each row record.
	truth := make([]int64, len(recs))
	present := make([]bool, len(recs))
	f := s.fields[fidx]
	for k, r := range recs {
		if testBit(r[:s.escBytes], fidx) {
			continue
		}
		present[k], truth[k] = true, readIntLE(r[s.escBytes+f.off:], f.width, f.unsigned)
	}

	// Once with Memory HOT, once COLD.
	for _, hot := range [][]int{{fidx}, nil} {
		blk := encodeColumnarBlock(s, recs, resolveColLayout(s, hot), mustZSTD(t), nil)
		seen := 0
		err := blk.scanInt(fidx, nil, func(k int, p bool, v int64) {
			seen++
			if p != present[k] {
				t.Errorf("hot=%d rec %d present=%v want %v", len(hot), k, p, present[k])
			}
			if p && v != truth[k] {
				t.Errorf("hot=%d rec %d value=%d want %d", len(hot), k, v, truth[k])
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen != len(recs) {
			t.Errorf("hot=%d scanned %d, want %d", len(hot), seen, len(recs))
		}
	}
}
