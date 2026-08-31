package collections

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestZSTDCodecEncoderLazy proves the read path never allocates an encoder: a codec holds
// no encoder until it Compresses, a codec that only ever Decompresses stays encoder-free,
// and releaseEncoder drops a warmed encoder so its match-finder history can be reclaimed --
// while every path still round-trips.
func TestZSTDCodecEncoderLazy(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("the quick brown classad jumped over the lazy dictionary; "), 64)

	// A fresh codec holds no encoder.
	writer, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	wz := writer.(*zstdCodec)
	if wz.enc.Load() != nil {
		t.Fatal("a freshly built codec already holds an encoder; it should be lazy")
	}

	// Compressing builds it.
	comp := writer.Compress(nil, payload)
	if wz.enc.Load() == nil {
		t.Fatal("Compress did not build the encoder")
	}

	// A separate codec used only to Decompress never builds one.
	reader, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	rz := reader.(*zstdCodec)
	got, err := reader.Decompress(nil, comp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("decode-only codec round-trip mismatch")
	}
	if rz.enc.Load() != nil {
		t.Fatal("a decode-only codec built an encoder just to Decompress")
	}

	// Release drops the encoder; a later Compress rebuilds it and still round-trips.
	wz.releaseEncoder()
	if wz.enc.Load() != nil {
		t.Fatal("releaseEncoder left an encoder resident")
	}
	comp2 := writer.Compress(nil, payload)
	if wz.enc.Load() == nil {
		t.Fatal("Compress after release did not rebuild the encoder")
	}
	got2, err := reader.Decompress(nil, comp2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, payload) {
		t.Fatal("round-trip after release mismatch")
	}
}

// TestReleaseEncodersExceptKeepsCurrent checks the registry sweep releases every registered
// dictionary codec's warmed encoder except the current write codec's, and that a released
// codec still decodes (only its encoder went away, not its decoder).
func TestReleaseEncodersExceptKeepsCurrent(t *testing.T) {
	t.Parallel()
	reg := newDictReg(identityCodec{})
	mk := func() *zstdCodec {
		c, err := NewZSTDCodec(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*zstdCodec)
	}
	a, b := mk(), mk()
	if _, err := reg.register(a, []byte("dict-a")); err != nil { // dir=="" so no file is written
		t.Fatal(err)
	}
	if _, err := reg.register(b, []byte("dict-b")); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("dictionary codec warm-up payload; "), 32)
	compB := b.Compress(nil, payload) // warm both
	_ = a.Compress(nil, payload)
	if a.enc.Load() == nil || b.enc.Load() == nil {
		t.Fatal("warm-up did not build both encoders")
	}

	reg.releaseEncodersExcept(a) // a is the current write codec
	if a.enc.Load() == nil {
		t.Error("released the current codec's encoder")
	}
	if b.enc.Load() != nil {
		t.Error("idle codec's encoder was not released")
	}

	// The released codec still decodes what it wrote.
	got, err := b.Decompress(nil, compB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("idle codec's decode path broke after its encoder was released")
	}
}

// TestIdleEncoderReleasedAcrossRetrain exercises the real collection path in BOTH stores
// (in-memory New and persistent Open) over an append-only log: two retrains leave the first
// trained generation's codec read-only, and the retrain sweep must drop its warmed encoder
// while every record from all three generations still reads back intact.
func TestIdleEncoderReleasedAcrossRetrain(t *testing.T) {
	run := func(t *testing.T, dir string) {
		var c *Collection
		if dir == "" {
			c = New(Options{AppendOnly: true, SegmentSize: 1 << 12, ValueAttrs: []string{"N"}})
		} else {
			var err error
			c, err = Open(Options{AppendOnly: true, Dir: dir, SegmentSize: 1 << 12, ValueAttrs: []string{"N"}})
			if err != nil {
				t.Fatal(err)
			}
		}
		defer c.Close()

		const per = 500
		put := func(base int) {
			for i := 0; i < per; i++ {
				ad, err := classad.Parse(fmt.Sprintf(
					`[ N=%d; Owner="user_%d"; JobStatus="Completed"; Cmd="/usr/bin/some_long_repeated_command_path"; Args="--flag=value --other=thing" ]`,
					base+i, (base+i)%20))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Put([]byte("k"), ad); err != nil {
					t.Fatal(err)
				}
			}
		}

		put(0)
		if _, err := c.RetrainDict(1000); err != nil {
			t.Fatalf("retrain 1: %v", err)
		}
		z1, ok := c.currentCodec().(*zstdCodec)
		if !ok {
			t.Skipf("retrain 1 did not yield a zstd codec (%T)", c.currentCodec())
		}
		put(per) // appended under generation 1: warms z1 (the current codec)
		if z1.enc.Load() == nil {
			t.Fatal("appends under generation 1 did not warm its encoder")
		}

		if _, err := c.RetrainDict(1000); err != nil {
			t.Fatalf("retrain 2: %v", err)
		}
		gen2 := c.currentCodec()
		if gen2 == Codec(z1) {
			t.Fatal("retrain 2 did not swap the write codec")
		}
		put(2 * per) // appended under generation 2

		// Generation 1 is now read-only; retrain 2's sweep must have released its encoder.
		if z1.enc.Load() != nil {
			t.Error("idle generation-1 encoder still resident after a later retrain")
		}
		if z2, ok := gen2.(*zstdCodec); ok && z2.enc.Load() == nil {
			t.Error("the current write codec's encoder was wrongly released")
		}

		// Every record from all three generations reads back intact.
		total := 0
		for ad := range c.Scan() {
			own, _ := ad.EvaluateAttrString("Owner")
			if !strings.HasPrefix(own, "user_") {
				t.Fatalf("record corrupted after retrain: Owner=%q", own)
			}
			total++
		}
		if total != 3*per {
			t.Fatalf("scan yielded %d records, want %d", total, 3*per)
		}
	}

	t.Run("memory", func(t *testing.T) { run(t, "") })
	t.Run("persistent", func(t *testing.T) { run(t, t.TempDir()) })
}
