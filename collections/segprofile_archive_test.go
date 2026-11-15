package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// profiledArchive builds an archive whose eviction axis is "T", appending n records whose T
// values are deliberately NOT in append order -- the whole point of the profile is that it
// describes the values a segment holds, not the order they arrived in.
func profiledArchive(t *testing.T, dir string, n int) *Archive {
	t.Helper()
	a, err := CreateArchive(ArchiveOptions{
		Dir:         dir,
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		Retention:   Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// Interleave two eras so no segment is a clean value range: a backfill replaying old
		// records beside live ones is exactly the hub/spoke shape this is for.
		tv := 1_700_000_000 + i
		if i%2 == 0 {
			tv = 1_500_000_000 + i
		}
		ad, err := classad.Parse(fmt.Sprintf(`[ N=%d; T=%d; Pad="%s" ]`, i, tv, profPad(64)))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func profPad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func sealedProfiles(a *Archive) (sealed, profiled int) {
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, s := range sh.segs {
		if s != nil && s != sh.act {
			sealed++
			if s.profile != nil {
				profiled++
			}
		}
	}
	return
}

// Every segment sealed under a configured eviction axis carries a profile, and the profile
// describes the VALUES the segment holds rather than its position in the log.
func TestArchiveSealBuildsProfile(t *testing.T) {
	a := profiledArchive(t, t.TempDir(), 1200)
	defer a.Close()

	sealed, profiled := sealedProfiles(a)
	if sealed < 2 {
		t.Fatalf("want several sealed segments, got %d", sealed)
	}
	if profiled != sealed {
		t.Fatalf("%d/%d sealed segments carry a profile", profiled, sealed)
	}

	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, s := range sh.segs {
		if s == nil || s == sh.act || s.profile == nil {
			continue
		}
		p := s.profile
		if p.attr != "T" {
			t.Errorf("profile attr = %q, want T", p.attr)
		}
		if p.totalBytes <= 0 || p.unknownBytes != 0 {
			t.Errorf("total/unknown = %d/%d, want >0 / 0", p.totalBytes, p.unknownBytes)
		}
		// Both eras are interleaved into every segment, so a segment spans them -- which is
		// precisely what a min/max zone cannot express usefully and the profile can.
		if p.valueMin >= 1_700_000_000 || p.valueMax < 1_700_000_000 {
			t.Errorf("segment span = [%v,%v]; want it to straddle both eras", p.valueMin, p.valueMax)
		}
		for i := 1; i < len(p.breaks); i++ {
			if p.breaks[i] < p.breaks[i-1] {
				t.Fatalf("breaks not non-decreasing: %v", p.breaks)
			}
		}
	}
}

// The profile must survive a reopen out of the sidecar. Recomputing it would mean decoding
// every record of every segment at startup -- the cost that persisting zones exists to avoid.
func TestArchiveProfileSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	a := profiledArchive(t, dir, 1200)
	sealedBefore, profiledBefore := sealedProfiles(a)
	if profiledBefore == 0 {
		t.Fatal("nothing profiled before the reopen")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := OpenArchive(ArchiveOptions{
		Dir: dir, SegmentSize: 1 << 14,
		ZoneAttrs: []string{"T"},
		Retention: Retention{EvictByAttr: "T"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sealedAfter, profiledAfter := sealedProfiles(b)
	if sealedAfter < sealedBefore {
		t.Fatalf("sealed segments %d -> %d across the reopen", sealedBefore, sealedAfter)
	}
	if profiledAfter < profiledBefore {
		t.Errorf("profiles %d -> %d across the reopen: the section did not round-trip",
			profiledBefore, profiledAfter)
	}
}

// Changing the eviction axis must not make old profiles answer for the new attribute: their
// values are in another space. They are refused, which reads as "not orderable by value" --
// never as "holds nothing below the cutoff", which would evict them first.
func TestArchiveProfileForAnotherAttributeIsRefused(t *testing.T) {
	dir := t.TempDir()
	a := profiledArchive(t, dir, 1200)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := OpenArchive(ArchiveOptions{
		Dir: dir, SegmentSize: 1 << 14,
		ZoneAttrs: []string{"T", "N"},
		Retention: Retention{EvictByAttr: "N"}, // a different axis than the segments were sealed on
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	sealed, profiled := sealedProfiles(b)
	if sealed == 0 {
		t.Fatal("no sealed segments")
	}
	if profiled != 0 {
		t.Errorf("%d/%d segments adopted a profile built over a different attribute", profiled, sealed)
	}
}
