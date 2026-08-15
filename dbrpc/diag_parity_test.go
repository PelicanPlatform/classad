package dbrpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// The two diagnostics builders drifted: each grew fields the other never got, so `.stats <table>` and
// `.stats <archive>` described the same storage engine in different terms. A mutable table reported no
// sidecar bytes and therefore no on-disk total; an archive reported no columnar accelerator, no hot
// attributes and no encryption state -- the last of which matters, because an archive really is
// unsealed, and a missing line reads as "not applicable" rather than "not protected".
//
// This test is reflective on purpose. Asserting a fixed list of fields would go stale the moment someone
// adds a field to one builder, which is exactly how the drift happened.
//
// kindSpecificFields are the fields that legitimately belong to one kind. Everything else must be
// populated for both, or listed here with a reason.
var kindSpecificFields = map[string]string{
	"Archive":         "the discriminator itself",
	"Retention":       "rotation bounds; a mutable table does not rotate",
	"ZoneAttrs":       "zone maps are an append-only construct",
	"Suggestions":     "GAP, not a design: ArchiveTable has no SuggestIndexes yet",
	"DropSuggestions": "GAP, not a design: ArchiveTable has no SuggestDrops yet",
	// This one is a real difference in the STORE, not in the reporting, and it is listed here so the
	// parity check stays meaningful rather than being satisfied by hiding the field: with pool keys
	// configured, a mutable table is encrypted at rest and an archive is NOT, because the archive open
	// path passes no data key. See TestArchiveReportsItIsNotEncrypted.
	"EncryptionEnabled": "GAP in the STORE: an archive is never sealed; reported so it is visible",
}

func TestDiagnosticsFieldsPopulatedForBothKinds(t *testing.T) {
	// Both kinds PERSISTENT, with a small segment size so segments actually SEAL, and pool keys so
	// encryption at rest is genuinely on for the mutable side. Every one of those matters: with the
	// default 8 MiB segment nothing seals, so sidecar bytes, sealed-segment counts and the columnar
	// block are all zero on both sides and the comparison passes without comparing anything -- which is
	// what the first version of this test did.
	root := t.TempDir()
	keys := []db.KEK{{ID: "POOL", Material: []byte("diag-parity-test-pool-key-material!!")}}
	// Indexes on both, too: a sidecar IS an index sealed to an mmap, so with none configured the sidecar
	// bytes and sealed-segment counts stay zero on both sides -- another way for the comparison to be
	// vacuous, and the shape a production table does not have.
	d, err := db.OpenConfig(db.Config{
		Dir: filepath.Join(root, "mutable"), SegmentSize: 1 << 16, PoolKeys: keys,
		CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"RequestMemory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: filepath.Join(root, "cat"), PoolKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	defer func() { s.Close(); cat.Close(); d.Close() }()

	// Same ads into both, so a difference cannot be blamed on the data.
	const ads = 4000
	tx := d.Begin()
	for i := 0; i < ads; i++ {
		if !tx.NewClassAdOld(fmt.Sprintf("k%d", i), parityAd(i)) {
			t.Fatalf("staging ad %d failed", i)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{
		SegmentSize: 1 << 16, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"RequestMemory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ads; i++ {
		if err := arch.AppendOld(parityAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Seal + index + columnarize both, which is what produces sidecar bytes, sealed-segment counts and
	// a columnar block. Reported as a precondition, not assumed: if either stays empty the comparisons
	// below are vacuous, and the coverage floor at the end of the test enforces that.
	d.Reindex()
	if !d.EnableSchemaScan(2000, 16) {
		t.Log("mutable: schema scan not enabled")
	}
	if !arch.BuildAndEnableSchemaScan(2000, 16) {
		t.Log("archive: schema scan not enabled")
	}

	mutable := mustDiag(t, func() ([]byte, error) { return s.diagJSON(d) })
	archive := mustDiag(t, func() ([]byte, error) { return s.archiveDiagJSON(arch) })
	if !archive.Archive {
		t.Fatal("the archive diagnostics do not identify themselves as an archive")
	}

	verified := 0
	mv, av := reflect.ValueOf(mutable), reflect.ValueOf(archive)
	for i := 0; i < mv.NumField(); i++ {
		name := mv.Type().Field(i).Name
		if why, skip := kindSpecificFields[name]; skip {
			t.Logf("%s: kind-specific (%s)", name, why)
			continue
		}
		mZero, aZero := mv.Field(i).IsZero(), av.Field(i).IsZero()
		if !mZero && !aZero {
			verified++ // both populated: this field really was compared
		} else if mZero && aZero {
			// Neither side reports it, so this fixture proves nothing about it. Logged rather than
			// passed over in silence: a field that is always zero here is a field this test does not
			// actually cover.
			t.Logf("%s: zero for BOTH kinds -- not exercised by this fixture", name)
		}
		if mZero != aZero {
			had, lacked := "mutable", "archive"
			if mZero {
				had, lacked = "archive", "mutable"
			}
			t.Errorf("%s is reported for the %s table but not the %s one: the two kinds must describe "+
				"the same engine in the same terms, or the field belongs in kindSpecificFields with a "+
				"reason", name, had, lacked)
		}
	}
	// A floor on the above: if a refactor left most fields empty on both sides, every comparison would
	// pass and this test would be decoration.
	// A RATCHET at the number this fixture reaches today. If a change legitimately removes a field,
	// lower it deliberately; if a change quietly stops populating one, this fails instead of the
	// comparison above silently having nothing to compare.
	if verified < 10 {
		t.Errorf("only %d fields were populated on both sides (want >= 10); this fixture is no longer "+
			"exercising the payload", verified)
	}
	t.Logf("%d fields verified populated for both kinds", verified)
}

// TestArchiveReportsItIsNotEncrypted pins the substance behind one of those fields. An archive's open
// path passes no data key, so history holds private attributes in the clear while a mutable table always
// seals them. Whatever is decided about that, the diagnostics must SAY so -- silence is what let it sit.
func TestArchiveReportsItIsNotEncrypted(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	defer func() { s.Close(); cat.Close() }()
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := arch.AppendOld(parityAd(1)); err != nil {
		t.Fatal(err)
	}
	got := mustDiag(t, func() ([]byte, error) { return s.archiveDiagJSON(arch) })
	if got.EncryptionEnabled {
		t.Error("an archive reports encryption at rest; if that became true, this test should be " +
			"inverted deliberately rather than deleted")
	}
}

func mustDiag(t *testing.T, build func() ([]byte, error)) Diagnostics {
	t.Helper()
	raw, err := build()
	if err != nil {
		t.Fatal(err)
	}
	var d Diagnostics
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// parityAd is one ad with the numeric attributes a columnar block covers, an indexed string, a private
// attribute -- and INCOMPRESSIBLE padding.
//
// The padding is not decoration. Without it 4000 of these ads compress into a single segment, so nothing
// ever seals, and sidecar bytes plus sealed-segment counts read as zero for both kinds: the test passes
// having compared nothing. Real ads are kilobytes each (the table this came from held 4652 ads in
// 124 MiB), so a fixture of tiny, highly-compressible ads is not a smaller version of production -- it is
// a different storage shape.
func parityAd(i int) string {
	var pad strings.Builder
	for j := 0; j < 8; j++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d", i, j)))
		pad.WriteString(hex.EncodeToString(sum[:]))
	}
	return fmt.Sprintf("ClusterId = %d\nProcId = 0\nJobStatus = %d\nRequestMemory = %d\n"+
		"Owner = \"alice%d\"\nClaimId = \"secret-%d\"\nPayload = %q",
		i, i%6, 1024+i, i%17, i, pad.String())
}
