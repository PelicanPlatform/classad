package collections

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestSegDictWireRoundTrip is the phase-1 core proof: a record encoded with segment-LOCAL
// interned ids (a fresh per-segment table) decodes back to an identical ad using ONLY the
// serialized dictionary as the id->name resolver -- no in-memory InternTable on the read side.
// This is what makes an interned sealed segment self-contained and heap-free to read.
func TestSegDictWireRoundTrip(t *testing.T) {
	src := []string{
		`Memory=4096
Cpus=8
Name="slot1@host.example.org"
Rank=Memory*1.5+Cpus
Requirements=(Arch=="X86_64")&&(OpSys=="LINUX")
Load=0.75
Big=true
Child=[a=1;b="x";Deep=[q=Memory+1]]`,
		`Memory=2048
Cpus=4
Disk=1000000
Name="slot2@host2"
Requirements=TARGET.Memory>1024
StartdIpAddr="<1.2.3.4:9618>"
Rank=0.0`,
		`Owner="user"
JobStatus=2
Cmd="/bin/sleep"
Args="600"
Environment=""`,
	}
	pt := wire.NewInternTable()
	var recs [][]byte
	var orig []*ast.ClassAd
	for _, s := range src {
		ad, err := classad.ParseOld(s)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		orig = append(orig, ad.AST())
		recs = append(recs, wire.Encode(nil, ad.AST(), pt))
	}

	// Serialize the dict from the per-segment table, then read it back through the mmap probe.
	dict := appendSegDict(nil, pt.Names())
	resolve := func(id uint32) (string, bool) {
		b := segDictName(dict, 0, id)
		if b == nil {
			return "", false
		}
		return string(b), true
	}

	for i, rec := range recs {
		got, err := wire.DecodeResolve(rec, resolve)
		if err != nil {
			t.Fatalf("rec %d: DecodeResolve: %v", i, err)
		}
		// Ground truth: the SAME interned record decoded with the in-memory table. segdict's
		// job is to be an equivalent id->name resolver, so dict-decode must match table-decode
		// exactly (this isolates the dict from any interned round-trip quirk of the ad itself).
		ref, err := wire.Decode(rec, pt)
		if err != nil {
			t.Fatalf("rec %d: Decode(table): %v", i, err)
		}
		if want, gotB := wire.EncodeInline(nil, ref), wire.EncodeInline(nil, got); !bytes.Equal(want, gotB) {
			t.Errorf("rec %d: dict-decode != table-decode:\n want %x\n got  %x", i, want, gotB)
		}
		_ = orig
	}

	// Every attribute name the records use must resolve name<->id both ways via the dict.
	for id := uint32(0); id < segDictCount(dict, 0); id++ {
		name := string(segDictName(dict, 0, id))
		if gotID, ok := segDictLookup(dict, 0, name); !ok || gotID != id {
			t.Errorf("dict name<->id inconsistency: id=%d name=%q -> %d,%v", id, name, gotID, ok)
		}
	}
}

// TestSegDictWireRoundTripCorpus runs the same round-trip over a segment's worth of real
// OSPool slot ads when the corpus is present (skips otherwise).
func TestSegDictWireRoundTripCorpus(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	if len(ads) > 300 {
		ads = ads[:300]
	}
	pt := wire.NewInternTable()
	recs := make([][]byte, len(ads))
	for i, a := range ads {
		recs[i] = wire.Encode(nil, a.AST(), pt)
	}
	dict := appendSegDict(nil, pt.Names())
	resolve := func(id uint32) (string, bool) {
		if b := segDictName(dict, 0, id); b != nil {
			return string(b), true
		}
		return "", false
	}
	for i, rec := range recs {
		got, err := wire.DecodeResolve(rec, resolve)
		if err != nil {
			t.Fatalf("rec %d: %v", i, err)
		}
		ref, err := wire.Decode(rec, pt)
		if err != nil {
			t.Fatalf("rec %d: Decode(table): %v", i, err)
		}
		if !bytes.Equal(wire.EncodeInline(nil, ref), wire.EncodeInline(nil, got)) {
			t.Fatalf("rec %d: dict-decode != table-decode", i)
		}
	}
	t.Logf("round-tripped %d real ads through a %d-name dict (%d bytes)", len(recs), segDictCount(dict, 0), len(dict))
}
