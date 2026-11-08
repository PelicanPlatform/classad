package dbrpc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// TestMaintenanceReportsTheArchiveIndexItAdded: the scheduled pass says what its auto-tuner
// did to an archive.
//
// Applying the decision silently was tolerable while it only meant an extra posting list.
// It stopped being so once the tuner could create a COMPOSITE: that moves the index
// generation, so every sealed segment is left to the backfill to rebuild, and an operator
// watching an archive read its whole history back over the following passes had nothing in
// the log to connect it to.
func TestMaintenanceReportsTheArchiveIndexItAdded(t *testing.T) {
	c, srv, cleanup := catServerPairWithServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize:        1 << 12,
		IndexBackfillBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if err := c.ArchiveAppend(ctx, "history",
			fmt.Sprintf("ClusterId = %d\nOwner = %q\nPad = %q", i, fmt.Sprintf("u%d", i%7),
				strings.Repeat("y", 60))); err != nil {
			t.Fatal(err)
		}
	}
	arch, ok := srv.cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive missing from the catalog")
	}
	for range 20 {
		seq, err := arch.Query(`Owner == "u3"`)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	srv.maintainArchives(db.MaintainOptions{
		ArchiveMergeDisabled:  true,
		ArchiveIndexMinDemand: 5,
	}, nil)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if cat, _ := arch.IndexedAttrs(); len(cat) == 0 {
		t.Fatalf("the tuner added no index, so there is nothing for the pass to report; logged: %q", got)
	}
	if !strings.Contains(got, "maintenance added an index") || !strings.Contains(got, "attr=Owner") {
		t.Errorf("the pass added an index on Owner and did not report it; logged: %q", got)
	}
	if !strings.Contains(got, "archive=history") {
		t.Errorf("the report does not name the archive it fired for; logged: %q", got)
	}
}

// TestMaintenanceSaysNothingWhenItTunedNothing: a pass that changed nothing is silent, which
// is almost every pass. Without this the log would carry a line per table per interval.
func TestMaintenanceSaysNothingWhenItTunedNothing(t *testing.T) {
	c, srv, cleanup := catServerPairWithServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize:        1 << 12,
		IndexBackfillBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	srv.maintainArchives(db.MaintainOptions{
		ArchiveMergeDisabled:  true,
		ArchiveIndexMinDemand: 1000, // far above anything this archive has been asked
	}, nil)
	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if strings.Contains(got, "maintenance added an index") {
		t.Errorf("a pass that tuned nothing reported an index: %q", got)
	}
}

// lockedWriter serializes writes from the maintenance goroutine against the test's read.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
