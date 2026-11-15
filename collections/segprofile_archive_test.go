package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
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
		ad, err := classad.Parse(fmt.Sprintf(`[ N=%d; T=%d; Pad=%q ]`, i, tv, profPad(64)))
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

// A merge whose sources are not all profiled -- segments sealed before the eviction axis
// was configured sit beside ones sealed after -- must still produce a profiled segment. The
// merge cannot assemble one from the sources (one of them has nothing to offer, and a
// profile over only some of the records would misreport what sits above a cutoff), so it
// is computed from the output's own records. Left nil, the merged segment would be
// reclaimable only by position for the rest of its life: nothing later rebuilds a profile.
func TestMergeRecomputesProfileForUnprofiledSource(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		ZoneAttrs:   []string{"T"},
		// No eviction axis yet: the first segments seal unprofiled.
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	appendTwoEras := func(from, n int) {
		for i := from; i < from+n; i++ {
			tv := 1_700_000_000 + i
			if i%2 == 0 {
				tv = 1_500_000_000 + i
			}
			ad, perr := classad.Parse(fmt.Sprintf(`[ N=%d; T=%d; Pad=%q ]`, i, tv, profPad(64)))
			if perr != nil {
				t.Fatal(perr)
			}
			if err := a.Append(ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendTwoEras(0, 600)
	a.SetRetention(Retention{EvictByAttr: "T"}) // from here on, seals are profiled
	appendTwoEras(600, 600)

	sh := a.c.shards[0]
	sh.mu.RLock()
	var run []*segment
	unprofiled, profiled := 0, 0
	for _, s := range sh.segs {
		if s == nil || s == sh.act || s.used == 0 {
			continue
		}
		run = append(run, s)
		if s.profile == nil {
			unprofiled++
		} else {
			profiled++
		}
	}
	sh.mu.RUnlock()
	if unprofiled == 0 || profiled == 0 {
		t.Fatalf("fixture: %d unprofiled and %d profiled sealed segments; need both", unprofiled, profiled)
	}
	before := countMatching(t, a, "T > 0")

	a.c.maintMu.Lock()
	ok := a.c.mergeSegments(sh, run)
	a.c.maintMu.Unlock()
	if !ok {
		t.Fatal("mergeSegments reported failure")
	}
	if after := countMatching(t, a, "T > 0"); after != before {
		t.Fatalf("records %d -> %d across the merge", before, after)
	}

	sh.mu.RLock()
	merged := sh.segs[run[0].id]
	sh.mu.RUnlock()
	p := merged.profile
	if p == nil {
		t.Fatal("merged segment has no profile: a merge that absorbed an unprofiled source left the output unprofiled")
	}
	if p.attr != "T" || !p.haveValues || p.unknownBytes != 0 || p.totalBytes <= 0 {
		t.Errorf("profile = %+v, want a complete profile over T", p)
	}
	if p.valueMin >= 1_700_000_000 || p.valueMax < 1_700_000_000 {
		t.Errorf("span = [%v,%v]; want it to cover both eras the sources held", p.valueMin, p.valueMax)
	}
	if len(p.breaks) < 2 {
		t.Errorf("%d breakpoints over a %d-byte segment: not computed from the records", len(p.breaks), merged.used)
	}
	for i := 1; i < len(p.breaks); i++ {
		if p.breaks[i] < p.breaks[i-1] {
			t.Fatalf("breaks not non-decreasing: %v", p.breaks)
		}
	}
	// The lower-bound invariant, checked against the records themselves: what the profile
	// says lies below a cutoff must not exceed what actually does.
	var below int64
	cutoff := 1_600_000_000.0
	tid, _ := a.c.intern.LookupID("T")
	za := zoneAttr{name: "T", id: tid}
	for off := 0; off < merged.used; {
		o := uint32(off)
		total := recTotalLen(merged.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(merged.data, o) {
			w, werr := a.c.recordWireIn(merged, merged.data, o, nil)
			if werr != nil {
				t.Fatal(werr)
			}
			// The same lookup the trim and the profile use, so the check reads the value
			// exactly as they do.
			if node, has := lookupZoneNode(wire.Ad(w), za, merged.dict.Load(), sh.zoneInline); has {
				if tv, cell := zoneValue(node); cell == zoneNumber && tv < cutoff {
					below += int64(total)
				}
			}
		}
		off += int(total)
	}
	if got, _ := p.bytesBelow(cutoff); got > below {
		t.Errorf("bytesBelow(%v) = %d, but only %d bytes of records are below it", cutoff, got, below)
	}
}

// Configuring the eviction axis is enough: the attribute is zone-mapped for you, because the
// profile is read off the same per-segment walk as the zone map. Without that, an archive
// configured with EvictByAttr and no matching ZoneAttr built no profiles at all and evicted
// by position -- the behaviour the setting exists to replace -- and said nothing.
func TestEvictAttrIsZoneMappedWithoutBeingListed(t *testing.T) {
	a, err := CreateArchive(ArchiveOptions{
		Dir:         t.TempDir(),
		SegmentSize: 1 << 14,
		Retention:   Retention{EvictByAttr: "T"}, // deliberately no ZoneAttrs
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for i := 0; i < 1200; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ T=%d; Pad=%q ]`, 1_700_000_000+i, profPad(96)))
		if perr != nil {
			t.Fatal(perr)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}

	profiled, sealed := a.ProfiledSegments()
	if sealed < 2 {
		t.Fatalf("want several sealed segments, got %d", sealed)
	}
	if profiled != sealed {
		t.Errorf("%d/%d sealed segments profiled: the axis was not zone-mapped, so eviction "+
			"silently falls back to dropping the oldest segment by position", profiled, sealed)
	}
	if !containsFoldStr(a.ZoneAttrs(), "T") {
		t.Errorf("ZoneAttrs = %v, want the eviction axis among them", a.ZoneAttrs())
	}
}

func containsFoldStr(xs []string, want string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}
