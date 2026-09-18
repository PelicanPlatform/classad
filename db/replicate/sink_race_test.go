package replicate

import (
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestSinkCursorConcurrentCommit reproduces the race CI found in the changefeed module: a puller
// commits from its main loop AND from an ack ticker beside it, so two goroutines call Commit (and
// Cursor) on one sink at once. The cursor is the resume point -- a torn write there means
// replaying or skipping changes after a restart -- so it has to be safe, not merely usually right.
//
// The race is timing-dependent through the changefeed client (its ticker has to fire at the moment
// the loop commits, which a fast machine rarely arranges); driven directly it is deterministic
// under -race.
func TestSinkCursorConcurrentCommit(t *testing.T) {
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	tbl, err := cat.CreateTable("mirror")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := cat.CreateArchiveTable("hist", db.ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		make func() (Sink, error)
	}{
		{"table", func() (Sink, error) { return NewTableSink(tbl, "ap40", &MemCursorStore{}) }},
		{"archive", func() (Sink, error) { return NewArchiveSink(arch, "ap40", &MemCursorStore{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.make()
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < 200; i++ {
						if err := s.Commit([]byte{byte(g), byte(i)}); err != nil {
							t.Errorf("commit: %v", err)
							return
						}
						_ = s.Cursor()
					}
				}(g)
			}
			wg.Wait()
			if c := s.Cursor(); len(c) != 2 {
				t.Fatalf("cursor is %d bytes after concurrent commits, want a whole one", len(c))
			}
		})
	}
}
