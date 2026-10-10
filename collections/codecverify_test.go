package collections

// What read verification is worth depends on the codec, and the answer is not the same
// for both. A ZSTD frame carries its own content checksum, so a flipped bit in a
// compressed ad is refused with or without the record CRC. Under the identity codec
// nothing covers the ad, and the record CRC is the only thing standing between a
// flipped bit and a plausible wrong value being served.
//
// This matters because the choice is per store and sticky: db's chooseBaseCodec gives
// NEW stores ZSTD but keeps identity for a store that already held data when that
// default changed. So the stores where verification is load-bearing are precisely the
// oldest and busiest ones.

import (
	"fmt"
	"os"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func TestVerificationValueDependsOnTheCodec(t *testing.T) {
	zstd, err := NewZSTDCodec(nil) // what db's chooseBaseCodec builds for a new store
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// wrongWithoutVerification is whether a corrupt ad is SERVED when the record
		// checksum is not consulted.
		wrongWithoutVerification bool
		codec                    Codec
	}{
		{"identity", true, nil},
		{"zstd", false, zstd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			keys := seedForCodec(t, dir, tc.codec, 600)

			// Damage an ad body in a SEALED segment that has a sidecar. The sidecar is
			// what makes this reachable: with one, recovery takes the recorded extent
			// and never walks the records, so the damaged record is READ rather than
			// truncated away with everything after it.
			s := &corruptStore{t: t, dir: dir, keys: keys}
			var seg string
			for _, p := range s.segments() {
				if _, e := os.Stat(p + ".idx"); e == nil {
					seg = p
					break
				}
			}
			if seg == "" {
				t.Fatal("no sealed segment with a sidecar; the test would prove nothing")
			}
			flipBit(t, seg, adBodyOffset(t, seg, len(recordOffsets(t, seg))/2))

			on := censusForCodec(t, dir, tc.codec, true, keys)
			off := censusForCodec(t, dir, tc.codec, false, keys)
			t.Logf("verification on:  %v", on)
			t.Logf("verification off: %v", off)

			if on.wrong != 0 {
				t.Errorf("%s: verification served %d wrong value(s)", tc.name, on.wrong)
			}
			if on.missing != 1 {
				t.Errorf("%s: want the corrupt record to be the one miss, got %d", tc.name, on.missing)
			}
			if got := off.wrong != 0; got != tc.wrongWithoutVerification {
				t.Errorf("%s: wrong value served without verification = %v, want %v (%v)",
					tc.name, got, tc.wrongWithoutVerification, off)
			}
		})
	}
}

type codecCensus struct{ ok, missing, wrong int }

func (c codecCensus) String() string {
	return fmt.Sprintf("%d ok, %d missing, %d wrong", c.ok, c.missing, c.wrong)
}

func seedForCodec(t *testing.T, dir string, codec Codec, n int) []string {
	t.Helper()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("job%05d", i)
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		ad.InsertAttrString("Cmd", "/usr/bin/analysis")
		ad.InsertAttr("QDate", int64(1790000000+i))
		if err := c.Put([]byte(k), ad); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return keys
}

func censusForCodec(t *testing.T, dir string, codec Codec, verify bool, keys []string) codecCensus {
	t.Helper()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14,
		Codec: codec, DisableReadVerification: !verify})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var out codecCensus
	for i, k := range keys {
		ad, found := c.Get([]byte(k))
		switch {
		case !found:
			out.missing++
		case codecAdDiff(ad, i) != "":
			out.wrong++
		default:
			out.ok++
		}
	}
	return out
}

func codecAdDiff(ad *classad.ClassAd, i int) string {
	if got, ok := ad.EvaluateAttrString("Owner"); !ok || got != fmt.Sprintf("user%d", i%7) {
		return fmt.Sprintf("Owner=%q", got)
	}
	if got, ok := ad.EvaluateAttrString("Cmd"); !ok || got != "/usr/bin/analysis" {
		return fmt.Sprintf("Cmd=%q", got)
	}
	if got, ok := ad.EvaluateAttrInt("QDate"); !ok || got != int64(1790000000+i) {
		return fmt.Sprintf("QDate=%d", got)
	}
	return ""
}

// TestZstdLeavesTheRecordHeaderUncovered measures the regions a ZSTD frame does NOT
// span: the stored key, the framing lengths, and the commit sequence. Those are raw
// record bytes, and only the record CRC covers them.
//
// It exists because "ZSTD already protects the ad, so verification is redundant" is
// true for the ad BODY and not for the record. The commit sequence is the case that
// matters: with verification off a seq-corrupted record reads back with its ad intact,
// so an ad-content census scores it as fine -- while the record now claims a commit
// sequence it never had. Anything that pages by commit sequence (a mirror cursor, a
// watch resume, a time-travel read) is steered by exactly that field.
//
// Shards: 1 so that corrupting a key cannot also move the record to a different shard,
// a confound that masked the read path in an earlier attempt at this.
func TestZstdLeavesTheRecordHeaderUncovered(t *testing.T) {
	zstd, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		name string
		// offset of the byte to flip, relative to the record start; -1 means adLen,
		// whose position depends on the key length.
		rel int
		// refusedWithoutVerification is whether the record is already lost when the
		// record checksum is not consulted.
		refusedWithoutVerification bool
	}{
		{"key byte", recKeyOff, true},
		{"commit seq", recSeqOff, false},
		{"adLen", -1, true},
	} {
		t.Run(p.name, func(t *testing.T) {
			on := zstdRegionCensus(t, zstd, p.rel, true)
			off := zstdRegionCensus(t, zstd, p.rel, false)
			t.Logf("verification on:  %v", on)
			t.Logf("verification off: %v", off)

			if on.missing != 1 || on.wrong != 0 {
				t.Errorf("verification should refuse exactly the damaged record, got %v", on)
			}
			if got := off.missing == 1; got != p.refusedWithoutVerification {
				t.Errorf("without verification, refused=%v want %v (%v)",
					got, p.refusedWithoutVerification, off)
			}
			// Whatever happens, a corrupt record must never be SERVED with wrong content.
			if off.wrong != 0 {
				t.Errorf("without verification, %d record(s) came back with wrong content", off.wrong)
			}
		})
	}
}

func zstdRegionCensus(t *testing.T, codec Codec, rel int, verify bool) codecCensus {
	t.Helper()
	dir := t.TempDir()
	keys := seedForCodec(t, dir, codec, 600)
	s := &corruptStore{t: t, dir: dir, keys: keys}

	var seg string
	for _, q := range s.segments() {
		if _, e := os.Stat(q + ".idx"); e == nil {
			seg = q
			break
		}
	}
	if seg == "" {
		t.Fatal("no sealed segment with a sidecar; the test would prove nothing")
	}
	b, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	offs := recordOffsets(t, seg)
	off := offs[len(offs)/2]
	target := off + rel
	if rel < 0 { // adLen sits just past the inline key
		target = off + recKeyOff + int(recKeyLen(b, uint32(off)))
	}
	b[target] ^= 0x01
	if err := os.WriteFile(seg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return censusForCodec(t, dir, codec, verify, keys)
}
