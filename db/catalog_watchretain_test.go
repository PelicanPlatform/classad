package db

import (
	"context"
	"iter"
	"testing"
	"time"
)

// watchCatchup drains a watch's catch-up phase and reports the reason it Reset with (or
// WatchResetNone if it resumed incrementally) along with the cursor at WatchSynced.
func watchCatchup(t *testing.T, seq iter.Seq[WatchEvent]) (WatchResetReason, []byte) {
	t.Helper()
	reason := WatchResetNone
	for ev := range seq {
		switch ev.Kind {
		case WatchReset:
			reason = ev.Reason
		case WatchSynced:
			return reason, ev.Cursor
		}
	}
	t.Fatal("watch ended before WatchSynced")
	return reason, nil
}

// withCatalog opens the catalog with cfg, runs body, and closes it -- a CLEAN close, which
// is what the epoch marker is written by.
func withCatalog(t *testing.T, cfg CatalogConfig, body func(cat *Catalog)) {
	t.Helper()
	cat, err := OpenCatalogConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cat.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	body(cat)
}

// TestCatalogWatchRetainSurvivesRestart is the knob's whole point: a mutable table carries
// its watch epoch across a clean restart ONLY when a retention window promises its delete
// markers are still there (collections/watchepoch.go). Config.WatchRetain could say so for a
// table opened directly, but a catalog builds its tables' configs itself, so for anyone using
// OpenCatalog -- which is anyone who needs more than one table -- the window was unreachable
// and every cursor Reset on every restart.
func TestCatalogWatchRetainSurvivesRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resumeAfterRestart := func(t *testing.T, cfg func(dir string) CatalogConfig) WatchResetReason {
		t.Helper()
		dir := t.TempDir()
		var cursor []byte
		withCatalog(t, cfg(dir), func(cat *Catalog) {
			d, err := cat.CreateTable("active")
			if err != nil {
				t.Fatal(err)
			}
			putAd(t, d, "a", "N = 1")
			seq, err := d.Watch(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, cursor = watchCatchup(t, seq)
			if cursor == nil {
				t.Fatal("no cursor from the initial sync")
			}
		})
		var reason WatchResetReason
		withCatalog(t, cfg(dir), func(cat *Catalog) {
			d, ok := cat.Table("active")
			if !ok {
				t.Fatal("table did not come back after reopen")
			}
			putAd(t, d, "b", "N = 2")
			seq, err := d.Watch(ctx, cursor)
			if err != nil {
				t.Fatal(err)
			}
			reason, _ = watchCatchup(t, seq)
		})
		return reason
	}

	t.Run("without-a-window-the-cursor-resets", func(t *testing.T) {
		// The negative arm. Without it this test would pass against a build that never
		// Resets at all, which is the failure mode a resume test is most prone to.
		got := resumeAfterRestart(t, func(dir string) CatalogConfig {
			return CatalogConfig{Dir: dir}
		})
		if got != WatchResetEpoch {
			t.Errorf("no window: reason = %v, want epoch (the store generation rotated)", got)
		}
	})

	t.Run("catalog-wide-window", func(t *testing.T) {
		got := resumeAfterRestart(t, func(dir string) CatalogConfig {
			return CatalogConfig{Dir: dir, WatchRetain: time.Hour}
		})
		if got != WatchResetNone {
			t.Errorf("CatalogConfig.WatchRetain: reason = %v, want no Reset at all", got)
		}
	})

	t.Run("per-table-window", func(t *testing.T) {
		got := resumeAfterRestart(t, func(dir string) CatalogConfig {
			return CatalogConfig{Dir: dir, WatchRetainFor: map[string]time.Duration{"active": time.Hour}}
		})
		if got != WatchResetNone {
			t.Errorf("WatchRetainFor[active]: reason = %v, want no Reset at all", got)
		}
	})

	t.Run("per-table-window-names-one-table", func(t *testing.T) {
		// The override is per NAME: a window for some other table must not leak onto this
		// one, or the map would be decoration over a catalog-wide setting.
		got := resumeAfterRestart(t, func(dir string) CatalogConfig {
			return CatalogConfig{Dir: dir, WatchRetainFor: map[string]time.Duration{"transfers": time.Hour}}
		})
		if got != WatchResetEpoch {
			t.Errorf("WatchRetainFor naming a different table: reason = %v, want epoch", got)
		}
	})

	t.Run("per-table-override-beats-the-catalog-default", func(t *testing.T) {
		// A catalog that wants the window for most tables but not for one that is rebuilt
		// from scratch anyway: 0 in the map has to win over a non-zero default.
		got := resumeAfterRestart(t, func(dir string) CatalogConfig {
			return CatalogConfig{
				Dir:            dir,
				WatchRetain:    time.Hour,
				WatchRetainFor: map[string]time.Duration{"active": 0},
			}
		})
		if got != WatchResetEpoch {
			t.Errorf("WatchRetainFor[active]=0 over a catalog default: reason = %v, want epoch", got)
		}
	})
}
