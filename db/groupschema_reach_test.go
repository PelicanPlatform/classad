package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestGroupSchemaConfigIsReachable: the group-schema knobs must be settable from a db caller.
//
// They were not, at first: GroupSchemaCount existed only on collections.Options, so no catalog and
// therefore no daemon could enable the feature. That is the same shape as an earlier gap where an
// index backfill horizon was implemented and unreachable -- a knob that cannot be set is the same
// as a knob that does not exist, and nothing else in the test suite notices, because the storage
// layer's own tests construct collections.Options directly.
func TestGroupSchemaConfigIsReachable(t *testing.T) {
	t.Run("mutable table", func(t *testing.T) {
		db, err := OpenConfig(Config{Dir: t.TempDir(), GroupSchemaCount: 4, GroupStabilityRuns: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for i := range 800 {
			text := fmt.Sprintf(`[ ClusterId=%d; Owner="u%d" ]`, i, i%5)
			if i%4 == 0 {
				text = fmt.Sprintf(`[ ClusterId=%d; Owner="u%d"; GA=%d; GB=%d; GC=%d ]`, i, i%5, i, i*2, i*3)
			}
			ad, err := classad.Parse(text)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
				t.Fatal(err)
			}
		}
		if len(db.GroupSchemas(4096, 4).Groups) == 0 {
			t.Fatal("no candidate groups derived through db.Config")
		}
		// Reporting works whatever the config says, so it is not the reachability check. What
		// proves the knob is wired is that blocks are BUILT for the groups.
		if !db.EnableSchemaScan(4096, 8) {
			t.Skip("schema scan did not enable")
		}
		if n := db.SchemaScanInfo().GroupSchemas; n == 0 {
			t.Error("GroupSchemaCount set through db.Config but no group schemas were built")
		}
	})

	t.Run("archive table", func(t *testing.T) {
		cat, err := OpenCatalog(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer cat.Close()
		a, err := cat.CreateArchiveTable("history", ArchiveConfig{
			GroupSchemaCount: 4, GroupStabilityRuns: 1, SegmentSize: 1 << 14,
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 800 {
			ad := classad.New()
			ad.Set("ClusterId", int64(i))
			ad.Set("Owner", fmt.Sprintf("u%d", i%5))
			if i%4 == 0 {
				ad.Set("GA", int64(i))
				ad.Set("GB", int64(i*2))
				ad.Set("GC", int64(i*3))
			}
			if err := a.Append(ad); err != nil {
				t.Fatal(err)
			}
		}
		if len(a.GroupSchemas(4096, 4).Groups) == 0 {
			t.Fatal("no candidate groups derived through ArchiveConfig")
		}
		if !a.BuildAndEnableSchemaScan(4096, 8) {
			t.Skip("schema scan did not enable")
		}
		if n := a.SchemaScanInfo().GroupSchemas; n == 0 {
			t.Error("GroupSchemaCount set through ArchiveConfig but no group schemas were built")
		}
	})
}
