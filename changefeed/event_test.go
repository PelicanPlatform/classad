package changefeed

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// TestEventCodecRoundTrip: a Change survives Change->Event->NDJSON bytes->Event->Change, including
// the typed ad, the cursor (base64), and TS.
func TestEventCodecRoundTrip(t *testing.T) {
	ad, err := classad.ParseOld(`Owner = "alice"; JobStatus = 4; ClusterId = 12`)
	if err != nil {
		t.Fatal(err)
	}
	in := replicate.Change{
		Kind: replicate.KindUpsert, Src: "ap40", Ver: 7, Key: "12.0", Ad: ad,
		Cursor: []byte{0x01, 0x02, 0xff}, TS: 1700000100000,
	}

	ev, err := ToEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := DecodeEvents(&buf, func(e Event) bool { got = e; return false }); err != nil {
		t.Fatal(err)
	}
	out, err := ToChange(got)
	if err != nil {
		t.Fatal(err)
	}

	if out.Kind != in.Kind || out.Src != in.Src || out.Ver != in.Ver || out.Key != in.Key || out.TS != in.TS {
		t.Errorf("scalar fields diverged: %+v vs %+v", out, in)
	}
	if !bytes.Equal(out.Cursor, in.Cursor) {
		t.Errorf("cursor = %v, want %v", out.Cursor, in.Cursor)
	}
	if v, _ := out.Ad.EvaluateAttrString("Owner"); v != "alice" {
		t.Errorf("Owner = %q, want alice", v)
	}
	if v, ok := out.Ad.EvaluateAttrInt("JobStatus"); !ok || v != 4 {
		t.Errorf("JobStatus = %d (ok=%v), want 4 (typed int preserved)", v, ok)
	}

	// A synced event carries no ad.
	sev, _ := ToEvent(replicate.Change{Kind: replicate.KindSynced, Cursor: []byte{9}})
	if len(sev.Ad) != 0 {
		t.Errorf("synced event should have no ad, got %s", sev.Ad)
	}
}
