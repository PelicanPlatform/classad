package db

import (
	"fmt"
	"testing"
	"time"
)

// Catalog open is a loop over directories, so without per-open timing the whole thing is one number -- which
// is how a 15s startup stayed unattributed: every other phase reported 0s and catalog-open reported all of
// it, with nothing inside. OnOpenStep is what makes the next restart say WHICH table.
func TestCatalogOnOpenStepReportsEachTable(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jobs", "history_t"} {
		if _, err := cat.EnsureTable(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.CreateArchiveTable("hist", ArchiveConfig{SegmentSize: 1 << 16}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]time.Duration{}
	kinds := map[string]string{}
	reopened, err := OpenCatalogConfig(CatalogConfig{
		Dir: dir,
		OnOpenStep: func(kind, name string, d time.Duration) {
			seen[name] = d
			kinds[name] = kind
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	for _, want := range []struct{ name, kind string }{
		{"jobs", "table"}, {"history_t", "table"}, {"hist", "archive"},
	} {
		if _, ok := seen[want.name]; !ok {
			t.Errorf("no timing reported for %s %q; catalog open would still be one opaque number",
				want.kind, want.name)
			continue
		}
		if kinds[want.name] != want.kind {
			t.Errorf("%q reported as kind %q, want %q", want.name, kinds[want.name], want.kind)
		}
	}
	// A nil hook must stay the default path, since every existing caller passes no hook.
	c3, err := OpenCatalogConfig(CatalogConfig{Dir: dir})
	if err != nil {
		t.Fatalf("opening without a hook: %v", err)
	}
	c3.Close()
	_ = fmt.Sprint
}
