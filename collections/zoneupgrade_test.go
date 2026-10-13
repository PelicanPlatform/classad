package collections

// A sealed segment whose sidecar carries no zone map is recomputed at open -- which
// decodes every record and makes startup scale with the archive rather than with its
// segment count. persistSegZonesUpgrade exists to write that map back so the cost is
// paid once. This checks that it actually is.

import (
	"fmt"
	"os"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// openArchiveWithZones opens dir as an append-only archive, optionally with zone
// attributes configured, and reports how long the open spent recomputing zone maps.
func openArchiveWithZones(t *testing.T, dir string, zoneAttrs []string) (*Collection, OpenTiming) {
	t.Helper()
	var got OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { got = d }
	defer func() { OpenIndexDiagHook = prev }()

	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 14,
		AppendOnly: true, ZoneAttrs: zoneAttrs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, got.Timing
}

// TestZoneUpgradeIsPaidOnce is the property an operator depends on: the FIRST open
// after zone attributes appear recomputes, and every open after that adopts.
func TestZoneUpgradeIsPaidOnce(t *testing.T) {
	dir := t.TempDir()

	// Seed with NO zone attributes, so the sealed sidecars carry no zone map -- the
	// state of any archive written before zone persistence, or before the attrs were
	// configured.
	c, _ := openArchiveWithZones(t, dir, nil)
	for i := 0; i < 4000; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		ad.InsertAttr("Ts", int64(1790000000+i))
		ad.InsertAttr("QDate", int64(1790000000+i))
		if err := c.Put([]byte(fmt.Sprintf("job%06d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Now configure zone attributes. This open must recompute...
	c1, t1 := openArchiveWithZones(t, dir, []string{"Ts"})
	c1.Close()
	t.Logf("open 1 (zones newly configured): zoneRecompute=%v recomputed=%d upgraded=%d failed=%d",
		t1.ZoneRecompute, t1.ZoneRecomputed, t1.ZoneUpgraded, t1.ZoneUpgradeFailed)
	if t1.ZoneRecomputed > 0 && t1.ZoneUpgraded != t1.ZoneRecomputed {
		t.Errorf("open 1 recomputed %d zone map(s) but wrote back only %d (%d failed)",
			t1.ZoneRecomputed, t1.ZoneUpgraded, t1.ZoneUpgradeFailed)
	}
	if t1.ZoneRecompute == 0 {
		t.Skip("no zone recompute happened; the fixture did not reach the path under test")
	}

	// ...and this one must not, because the first wrote the maps back.
	c2, t2 := openArchiveWithZones(t, dir, []string{"Ts"})
	c2.Close()
	t.Logf("open 2 (should adopt):           zoneRecompute=%v", t2.ZoneRecompute)

	if t2.ZoneRecompute != 0 {
		t.Errorf("zone maps were recomputed AGAIN on the second open (%v after %v); "+
			"the upgrade did not persist, so every start pays this forever",
			t2.ZoneRecompute, t1.ZoneRecompute)
	}

	// And a third, in case the second happened to write what the first did not.
	c3, t3 := openArchiveWithZones(t, dir, []string{"Ts"})
	c3.Close()
	t.Logf("open 3:                          zoneRecompute=%v", t3.ZoneRecompute)
	if t3.ZoneRecompute != 0 {
		t.Errorf("still recomputing on the third open (%v)", t3.ZoneRecompute)
	}
}

// TestZoneUpgradeRepairsAnUnreadableSection: a zone section that is PRESENT but does not
// parse used to be skipped as "already has one", so adoption left zones nil, the caller
// recomputed, and the next open did the same -- forever, silently. The repair has to
// treat unreadable as upgradeable.
func TestZoneUpgradeRepairsAnUnreadableSection(t *testing.T) {
	dir := t.TempDir()
	c, _ := openArchiveWithZones(t, dir, []string{"Ts"})
	for i := 0; i < 4000; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		ad.InsertAttr("Ts", int64(1790000000+i))
		if err := c.Put([]byte(fmt.Sprintf("job%06d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the zone section of every sidecar so it is present but unparseable.
	s := &corruptStore{t: t, dir: dir}
	var damaged int
	for _, segPath := range s.segments() {
		idx := segPath + ".idx"
		data, err := os.ReadFile(idx)
		if err != nil {
			continue
		}
		_, _, _, zone, ok := splitSegmentSidecar(data)
		if !ok || len(zone) == 0 {
			continue
		}
		// Flip the count so 4+n*20 no longer matches the length: present, unreadable.
		off := len(data) - len(zone) - sidecarTrailerLenV4
		if off < 0 || off+4 > len(data) {
			continue
		}
		if _, good := parseZoneBlob(zone); !good {
			continue
		}
		data[off] ^= 0xFF
		if err := os.WriteFile(idx, data, 0o644); err != nil {
			t.Fatal(err)
		}
		damaged++
	}
	if damaged == 0 {
		t.Skip("no parseable zone section found to damage; the fixture missed the path")
	}
	t.Logf("made the zone section unreadable in %d sidecar(s)", damaged)

	c1, t1 := openArchiveWithZones(t, dir, []string{"Ts"})
	c1.Close()
	t.Logf("open 1: recomputed=%d upgraded=%d failed=%d", t1.ZoneRecomputed, t1.ZoneUpgraded, t1.ZoneUpgradeFailed)
	if t1.ZoneRecomputed == 0 {
		t.Fatal("the damaged section was adopted anyway; the test proves nothing")
	}

	c2, t2 := openArchiveWithZones(t, dir, []string{"Ts"})
	c2.Close()
	t.Logf("open 2: recomputed=%d upgraded=%d", t2.ZoneRecomputed, t2.ZoneUpgraded)
	if t2.ZoneRecomputed != 0 {
		t.Errorf("an unreadable zone section was never repaired: still recomputing %d segment(s) "+
			"on the second open, which means every start pays it forever", t2.ZoneRecomputed)
	}
}
