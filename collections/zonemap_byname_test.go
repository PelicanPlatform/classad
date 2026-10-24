package collections

// Zone maps persisted in segment sidecars are keyed by attribute name. Keyed by intern id, as
// they first were, a configuration change (a zone attribute added, removed or reordered, or a
// value index added) left the next process reading one attribute's [min,max] as another's,
// pruning segments that held matches -- `RequestMemory == 3` returned 8 rows of ~429 -- and age
// retention judging segments by the wrong attribute.

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

const zoneBase = 1700000000

// openZoned opens dir as an archive with the given zone and value attributes, reporting the
// open's zone timing.
func openZoned(t *testing.T, dir string, zoneAttrs, valueAttrs []string) (*Archive, OpenTiming) {
	t.Helper()
	var got OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { got = d }
	defer func() { OpenIndexDiagHook = prev }()
	a, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 16 << 10, ZoneAttrs: zoneAttrs, ValueAttrs: valueAttrs})
	if err != nil {
		t.Fatal(err)
	}
	return a, got.Timing
}

// fillZoned appends n jobs: RequestMemory cycles 0..6, CompletionDate climbs one a record,
// ClusterId one per 40 records.
func fillZoned(t *testing.T, a *Archive, n int) {
	t.Helper()
	for i := range n {
		ad, err := classad.ParseOld(fmt.Sprintf("ID = %d\nClusterId = %d\nRequestMemory = %d\nCompletionDate = %d\n"+
			"Pad = \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx%d\"", i, i/40, i%7, zoneBase+i, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

// zoneCount is how many ads the archive returns for constraint.
func zoneCount(t *testing.T, a *Archive, constraint string) int {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range a.Query(q) {
		n++
	}
	return n
}

// zoneTruth is how many retained ads satisfy constraint, by evaluating it on every one: no
// zone map, index or compiled matcher involved.
func zoneTruth(t *testing.T, a *Archive, constraint string) int {
	t.Helper()
	expr, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := vm.Parse("true")
	n := 0
	for ad := range a.Query(all) {
		if b, err := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); err == nil && b {
			n++
		}
	}
	return n
}

var zoneConstraints = []string{
	"RequestMemory == 3",
	"RequestMemory < 2",
	"CompletionDate < 1700000100",
	"CompletionDate >= 1700002900",
	"ClusterId == 10",
	"ClusterId >= 70 && RequestMemory == 6",
}

func checkZoneCounts(t *testing.T, a *Archive, stage string) {
	t.Helper()
	for _, c := range zoneConstraints {
		if got, want := zoneCount(t, a, c), zoneTruth(t, a, c); got != want {
			t.Errorf("%s: %s: %d rows, want %d", stage, c, got, want)
		}
	}
}

// Every configuration change an operator can make leaves the answers right, at the reopen
// that changes it and at every reopen after.
func TestZonesSurviveConfigChange(t *testing.T) {
	for _, tc := range []struct {
		name              string
		zones, values     []string
		reZones, reValues []string
	}{
		// The reported case: a zone attribute added ahead of the existing one.
		{name: "zone added in front", zones: []string{"CompletionDate"},
			reZones: []string{"RequestMemory", "CompletionDate"}},
		{name: "zones reordered", zones: []string{"RequestMemory", "CompletionDate"},
			reZones: []string{"CompletionDate", "RequestMemory"}},
		{name: "zone removed", zones: []string{"RequestMemory", "CompletionDate"},
			reZones: []string{"CompletionDate"}},
		// A value index is zoned automatically, after the configured zones.
		{name: "value index added", zones: []string{"CompletionDate"},
			reZones: []string{"CompletionDate"}, reValues: []string{"ClusterId"}},
		{name: "value index dropped", zones: []string{"CompletionDate"}, values: []string{"ClusterId", "RequestMemory"},
			reZones: []string{"CompletionDate"}, reValues: []string{"RequestMemory"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a, _ := openZoned(t, dir, tc.zones, tc.values)
			fillZoned(t, a, 3000)
			checkZoneCounts(t, a, "written")
			a.Close()
			for _, stage := range []string{"reopened changed", "reopened again"} {
				a, _ = openZoned(t, dir, tc.reZones, tc.reValues)
				checkZoneCounts(t, a, stage)
				a.Close()
			}
		})
	}
}

// legacyizeZones rewrites every sealed segment sidecar's zone section in the first, id-keyed
// format, with each id holding ANOTHER attribute's range under the ids the next open will
// give the attributes -- the state a config change left an old archive in. Returns how many
// sections it rewrote.
func legacyizeZones(t *testing.T, dir string, ids map[string]uint32, swap map[string]string) int {
	t.Helper()
	var idxs []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".idx") {
			idxs = append(idxs, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := func(name string) (uint32, bool) { id, ok := ids[name]; return id, ok }
	n := 0
	for _, p := range idxs {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		attr, key, col, zone, used, dictRec, ok := splitSegmentSidecarV4(data)
		if !ok || len(zone) == 0 {
			continue
		}
		zones, st := parseZoneBlob(zone, byName)
		if st != zoneBlobOK {
			t.Fatalf("%s: zone section not name-keyed", p)
		}
		wrong := map[uint32]zoneRange{}
		for name, other := range swap {
			wrong[ids[name]] = zones[ids[other]]
		}
		legacy := binary.LittleEndian.AppendUint32(nil, uint32(len(wrong)))
		for id, z := range wrong {
			legacy = binary.LittleEndian.AppendUint32(legacy, id)
			legacy = binary.LittleEndian.AppendUint64(legacy, math.Float64bits(z.Min))
			legacy = binary.LittleEndian.AppendUint64(legacy, math.Float64bits(z.Max))
		}
		out := buildSegmentSidecar(append([]byte(nil), attr...), append([]byte(nil), key...),
			append([]byte(nil), col...), legacy, used, dictRec, nil)
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

// An archive written by a build that keyed zones by id is not trusted: its zones are
// recomputed once, at the first open, and written back by name, and the next open adopts
// them. The legacy sections here are deliberately misattributed -- RequestMemory carrying
// CompletionDate's range and the reverse -- so trusting one gives wrong answers and wrong
// age retention.
func TestLegacyZoneSectionsRecomputedOnce(t *testing.T) {
	dir := t.TempDir()
	zones := []string{"RequestMemory", "CompletionDate"}
	a, _ := openZoned(t, dir, zones, nil)
	fillZoned(t, a, 3000)
	ids := map[string]uint32{}
	for _, name := range zones {
		ids[name], _ = a.c.intern.LookupID(name)
	}
	a.Close()
	// The ids a reopen with the same configuration gives the zone attributes.
	a, _ = openZoned(t, dir, zones, nil)
	for _, name := range zones {
		if id, _ := a.c.intern.LookupID(name); id != ids[name] {
			t.Fatalf("%s is %d on reopen, %d before: the test's premise is off", name, id, ids[name])
		}
	}
	a.Close()
	legacy := legacyizeZones(t, dir, ids, map[string]string{"RequestMemory": "CompletionDate", "CompletionDate": "RequestMemory"})
	if legacy == 0 {
		t.Fatal("no zone sections to rewrite")
	}

	a, t1 := openZoned(t, dir, zones, nil)
	if t1.ZoneRecomputed != legacy || t1.ZoneUpgraded != legacy {
		t.Errorf("first open: recomputed %d, upgraded %d of %d legacy sections", t1.ZoneRecomputed, t1.ZoneUpgraded, legacy)
	}
	checkZoneCounts(t, a, "legacy sections")
	// Retention by CompletionDate keeps what is younger than the cutoff -- not what a
	// misattributed RequestMemory range (0..6, "ancient") would say.
	a.SetRetention(Retention{MaxAgeAttr: "CompletionDate", MaxAge: 1000})
	if _, err := a.Rotate(zoneBase + 2999); err != nil {
		t.Fatal(err)
	}
	if got, want := zoneCount(t, a, "CompletionDate >= 1700001999"), 1001; got != want {
		t.Errorf("after age retention: %d of the newest %d records kept", got, want)
	}
	if left := a.Count(); left == 3000 || left < 1001 {
		t.Errorf("age retention kept %d of 3000", left)
	}
	checkZoneCounts(t, a, "rotated")
	a.Close()

	a, t2 := openZoned(t, dir, zones, nil)
	defer a.Close()
	if t2.ZoneRecomputed != 0 {
		t.Errorf("second open recomputed %d segments: the upgrade did not stick", t2.ZoneRecomputed)
	}
	checkZoneCounts(t, a, "upgraded")
}

// Age retention reads the zone of the age attribute by name, so a reordered configuration
// still ages segments out by CompletionDate.
func TestRetentionAfterZoneConfigChange(t *testing.T) {
	dir := t.TempDir()
	a, _ := openZoned(t, dir, []string{"CompletionDate"}, nil)
	fillZoned(t, a, 3000)
	a.Close()
	a, _ = openZoned(t, dir, []string{"RequestMemory", "CompletionDate"}, nil)
	defer a.Close()
	a.SetRetention(Retention{MaxAgeAttr: "CompletionDate", MaxAge: 1000})
	if _, err := a.Rotate(zoneBase + 2999); err != nil {
		t.Fatal(err)
	}
	if got := zoneCount(t, a, "CompletionDate >= 1700001999"); got != 1001 {
		t.Errorf("kept %d of the newest 1001 records", got)
	}
	if left := a.Count(); left == 3000 || left < 1001 {
		t.Errorf("age retention kept %d of 3000", left)
	}
	checkZoneCounts(t, a, "rotated")
}
