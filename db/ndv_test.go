package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// ndvCatalog builds a mutable table and an archive table over the same shape, so the
// catalog-level lookup can be exercised on both kinds without either standing in for the
// other.
func ndvCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })

	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	if !jobs.AddIndex([]string{"Owner"}, []string{"ClusterId"}) {
		t.Fatal("AddIndex on jobs")
	}
	owners := []string{"alice", "bob", "carol"}
	for i := 0; i < 300; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ ClusterId = %d; Owner = %q; Cpus = %d ]`,
			i, owners[i%len(owners)], 1+i%4))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := jobs.Put(fmt.Sprintf("%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
	}
	jobs.Reindex()

	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		CategoricalAttrs: []string{"Owner"},
		ValueAttrs:       []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ ClusterId = %d; Owner = %q; ExitCode = %d ]`,
			i, owners[i%len(owners)], i%2))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := hist.Append(ad); werr != nil {
			t.Fatal(werr)
		}
	}
	if err := hist.Flush(); err != nil {
		t.Fatal(err)
	}
	hist.Reindex()
	return cat
}

// TestCatalogAttrNDV covers the lookup a join planner makes: one call per side, over
// either kind of table, with "no statistic" an answer it can act on.
func TestCatalogAttrNDV(t *testing.T) {
	t.Parallel()
	cat := ndvCatalog(t)

	for _, table := range []string{"jobs", "hist"} {
		owner, ok := cat.AttrNDV(table, "Owner")
		if !ok {
			t.Fatalf("%s.Owner is indexed; want an estimate", table)
		}
		if owner.Distinct != 3 {
			t.Errorf("%s.Owner distinct = %d, want 3", table, owner.Distinct)
		}
		cluster, ok := cat.AttrNDV(table, "ClusterId")
		if !ok {
			t.Fatalf("%s.ClusterId is indexed; want an estimate", table)
		}
		// A key with a distinct value per row: the figure a join-cardinality estimate
		// divides by, and the one a sampled count would understate.
		if cluster.Distinct < 280 || cluster.Distinct > 320 {
			t.Errorf("%s.ClusterId distinct = %d, want ~300", table, cluster.Distinct)
		}
		if cluster.Distinct <= owner.Distinct {
			t.Errorf("%s: a per-row key (%d) must outrank a three-value one (%d)",
				table, cluster.Distinct, owner.Distinct)
		}
		// Cpus/ExitCode are present in every row and indexed by nothing.
		if est, ok := cat.AttrNDV(table, "Cpus"); ok && table == "jobs" {
			t.Errorf("jobs.Cpus is unindexed; got %+v", est)
		}
	}

	if est, ok := cat.AttrNDV("nosuchtable", "Owner"); ok {
		t.Errorf("an unknown table yielded an estimate %+v", est)
	}
}

// TestCatalogAttrNDVAccuracyIsCarried: the class must reach the catalog surface, because a
// planner that reads only the number cannot tell a count from a bound.
func TestCatalogAttrNDVAccuracyIsCarried(t *testing.T) {
	t.Parallel()
	cat := ndvCatalog(t)
	est, ok := cat.AttrNDV("hist", "Owner")
	if !ok {
		t.Fatal("no estimate")
	}
	if est.Accuracy == "" || est.Err == "" {
		t.Fatalf("estimate %+v carries no accuracy class", est)
	}
	switch est.Accuracy {
	case NDVExact:
		if est.Err != NDVErrNone {
			t.Errorf("exact with err %q", est.Err)
		}
	case NDVBound:
		if est.Err != NDVErrLow && est.Err != NDVErrHigh {
			t.Errorf("bound with err %q", est.Err)
		}
	case NDVEstimated:
		if est.Err != NDVErrEither {
			t.Errorf("estimate with err %q", est.Err)
		}
	default:
		t.Errorf("unknown accuracy %q", est.Accuracy)
	}
	if est.Source == "" {
		t.Error("an estimate must name where it came from")
	}
}
