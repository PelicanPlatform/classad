package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestSegDictRecordInSegment verifies the on-arena dict record: appendDict writes a keyless
// record that every ordinary walk skips (recIsMarker) yet recovery can find (recIsDict), whose
// body is a serialized dict a segDictHandle probes zero-copy over the arena -- and an interned
// data record in the same segment decodes through that handle. This is the interned segment's
// self-describing layout in miniature.
func TestSegDictRecordInSegment(t *testing.T) {
	table := wire.NewInternTable()
	src := []string{
		`Memory=4096
Cpus=8
Name="slot1@h"
Requirements=(Arch=="X86_64")`,
		`Memory=2048
Cpus=4
Owner="user"
Rank=Memory*1.0`,
	}
	var recs [][]byte
	for _, s := range src {
		ad, err := classad.ParseOld(s)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, wire.Encode(nil, ad.AST(), table))
	}
	dict := appendSegDict(nil, table.Names())

	seg := newSegment(1, 1<<16, identityCodec{})
	// Dict record first (as the compaction transcode will emit it), then the interned records.
	dictOff, ok := seg.appendDict(dict)
	if !ok {
		t.Fatal("appendDict did not fit")
	}
	var recOffs []uint32
	for i, r := range recs {
		off, ok := seg.append(uint64(i+1), noLoc, []byte{byte('a' + i)}, r)
		if !ok {
			t.Fatal("append record did not fit")
		}
		recOffs = append(recOffs, off)
	}

	// The dict record is a marker (skipped by every ordinary walk) AND a dict (found by
	// recovery), keyless, and its body is exactly the serialized dict.
	if !recIsMarker(seg.data, dictOff) || !recIsDict(seg.data, dictOff) {
		t.Fatalf("dict record: recIsMarker=%v recIsDict=%v, want true,true", recIsMarker(seg.data, dictOff), recIsDict(seg.data, dictOff))
	}
	if recKeyLen(seg.data, dictOff) != 0 {
		t.Errorf("dict record keyLen=%d, want 0", recKeyLen(seg.data, dictOff))
	}
	if string(recAd(seg.data, dictOff)) != string(dict) {
		t.Errorf("dict record body != serialized dict")
	}

	// A handle over the arena at the dict body's offset resolves names and backs a resolver
	// decode of the interned data records -- identical to a table-backed decode.
	h := &segDictHandle{data: seg.data, base: recStart(seg.data, dictOff)}
	if int(h.count()) != len(table.Names()) {
		t.Fatalf("handle count=%d want %d", h.count(), len(table.Names()))
	}
	for i, off := range recOffs {
		if recIsMarker(seg.data, off) {
			t.Fatalf("data record %d wrongly reads as marker", i)
		}
		got, err := wire.DecodeResolve(recAd(seg.data, off), h.resolve)
		if err != nil {
			t.Fatalf("rec %d DecodeResolve: %v", i, err)
		}
		ref, err := wire.Decode(recs[i], table)
		if err != nil {
			t.Fatal(err)
		}
		if string(wire.EncodeInline(nil, ref)) != string(wire.EncodeInline(nil, got)) {
			t.Errorf("rec %d: handle-decode != table-decode", i)
		}
	}
}

// recStart returns the arena offset of a record's ad body -- the probe base for a dict record.
func recStart(b []byte, off uint32) uint32 {
	kl := recKeyLen(b, off)
	return off + recKeyOff + kl + 4 // skip header, (empty) key, adLen u32
}
