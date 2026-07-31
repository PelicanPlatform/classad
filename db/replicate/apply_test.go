package replicate

import (
	"context"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

func mustArchive(t *testing.T) *db.ArchiveTable {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	a, err := cat.CreateArchiveTable("history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// drainReplay watches src from cursor, feeds each replayed change into sink via ChangeFromWatch,
// and stops at WatchSynced (committing the synced cursor). Returns the synced cursor.
func drainReplay(t *testing.T, src *db.ArchiveTable, cursor []byte, srcName string, sink Sink) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seq, err := src.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	var ver uint64
	var synced []byte
	for we := range seq {
		ch, ok := ChangeFromWatch(we, srcName, ver)
		if !ok {
			continue
		}
		ver++
		if err := sink.Apply(ch); err != nil {
			t.Fatal(err)
		}
		if we.Kind == db.WatchSynced {
			synced = we.Cursor
			break
		}
	}
	if err := sink.Commit(synced); err != nil {
		t.Fatal(err)
	}
	return synced
}

func count(t *testing.T, a *db.ArchiveTable, constraint string) int {
	t.Helper()
	seq, err := a.Query(constraint)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	return n
}

// TestArchiveSinkApplyResume: the archive sink imports a source's changes (stamping Src), commits a
// resume cursor, and a resume imports only the new records (no re-import of already-synced ones).
func TestArchiveSinkApplyResume(t *testing.T) {
	src, dst := mustArchive(t), mustArchive(t)
	app := func(text string) {
		if err := src.AppendOld(text); err != nil {
			t.Fatal(err)
		}
	}
	app(`GlobalJobId = "ap40#12.0"; Owner = "alice"; ClusterId = 12`)
	app(`GlobalJobId = "ap40#13.0"; Owner = "bob";   ClusterId = 13`)

	sink, err := NewArchiveSink(dst, "ap40", &MemCursorStore{})
	if err != nil {
		t.Fatal(err)
	}
	cur := drainReplay(t, src, nil, "ap40", sink)
	if len(cur) == 0 {
		t.Fatal("no synced cursor")
	}
	if dst.Count() != 2 {
		t.Fatalf("dst has %d, want 2", dst.Count())
	}
	if n := count(t, dst, `Owner == "alice" && Src == "ap40"`); n != 1 {
		t.Errorf("alice+Src match = %d, want 1 (Src stamped, ad carried across)", n)
	}

	app(`GlobalJobId = "ap40#14.0"; Owner = "carol"; ClusterId = 14`)
	drainReplay(t, src, sink.Cursor(), "ap40", sink)
	if dst.Count() != 3 {
		t.Fatalf("after resume dst has %d, want 3 (no re-import)", dst.Count())
	}
}
