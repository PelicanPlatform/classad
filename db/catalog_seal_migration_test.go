package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// A catalog builds each table's Config itself, so Config.OnSealMigration -- the hook that explains a
// longer first start after the upgrade -- was unreachable for every table a daemon actually opens. The
// migration ran; nobody could see it.
//
// The catalog-level hook carries the TABLE NAME, because "some table rewrote 400 segments" does not tell
// an operator which one to expect the cost from.
func TestCatalogReportsSealMigrationPerTable(t *testing.T) {
	root := t.TempDir()

	// Build a catalog with two tables holding unsealed private attributes. Written through a collection
	// with no data key, which is what an older binary left on disk.
	const secret = "ClaimId-legacy-capability"
	tablesDir := filepath.Join(root, "tables")
	for _, name := range []string{"jobs", "machines"} {
		dir := filepath.Join(tablesDir, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		c, err := collections.Open(collections.Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 400; i++ {
			ad, err := classad.ParseOld(fmt.Sprintf("Cpus = %d\nClaimId = %q", i, secret))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	got := map[string]int{}
	cat, err := OpenCatalogConfig(CatalogConfig{
		Dir: root,
		OnSealMigration: func(table string, segments int) {
			mu.Lock()
			got[table] += segments
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the catalog migrated tables but reported nothing; the hook is still unreachable where " +
			"a daemon opens its tables")
	}
	for _, name := range []string{"jobs", "machines"} {
		if got[name] == 0 {
			t.Errorf("no migration reported for %q (reported: %v); the count has to name its table",
				name, got)
		}
	}
	t.Logf("catalog reported migrations: %v", got)
}
