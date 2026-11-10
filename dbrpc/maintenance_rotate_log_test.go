package dbrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// Rotation is the one step of the archive pass that destroys data, and db does not log, so the
// only way an operator learns an archive aged history out is this report. These cover the
// wiring: that the pass asks for the hook, that the report names the archive it fired for and
// says how much went, and that a pass which dropped nothing is silent.

// rotateLogFixture is an archive with tiny segments, enough ads to fill several of them, and a
// keep-at-most-2 bound, so one pass has something to drop.
func rotateLogFixture(t *testing.T, ret collections.Retention) (*Server, *db.ArchiveTable, func()) {
	t.Helper()
	c, srv, cleanup := catServerPairWithServer(t)
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		SegmentSize: 1 << 12,
		Retention:   ret,
	}); err != nil {
		cleanup()
		t.Fatal(err)
	}
	for i := range 1500 {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf(
			"ClusterId = %d\nOwner = \"user_%d\"\nPad = \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"",
			i, i%20)); err != nil {
			cleanup()
			t.Fatal(err)
		}
	}
	arch, ok := srv.cat.ArchiveTable("history")
	if !ok {
		cleanup()
		t.Fatal("archive missing from the catalog")
	}
	return srv, arch, cleanup
}

// TestMaintenanceReportsTheSegmentsItRotated: the pass wires OnRotate and the report names the
// archive. It goes through s.logger() rather than the package-level slog, which is the bug this
// exact spot has had before -- a report that bypasses the server's logger does not reach the
// place an operator is looking.
func TestMaintenanceReportsTheSegmentsItRotated(t *testing.T) {
	srv, arch, cleanup := rotateLogFixture(t, collections.Retention{MaxSegments: 2})
	defer cleanup()

	segsBefore := arch.Stats().Segments
	if segsBefore <= 2 {
		t.Fatalf("fixture holds %d segments, already within MaxSegments=2: nothing to rotate", segsBefore)
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	srv.maintainArchives(db.MaintainOptions{
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
	}, nil)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	segsAfter := arch.Stats().Segments
	if segsAfter >= segsBefore {
		t.Fatalf("the pass rotated nothing (%d -> %d), so there is nothing to report; logged: %q",
			segsBefore, segsAfter, got)
	}
	if !strings.Contains(got, "rotated archive") {
		t.Errorf("the pass rotated and did not report it; logged: %q", got)
	}
	if !strings.Contains(got, "archive=history") {
		t.Errorf("the report does not name the archive it fired for; logged: %q", got)
	}
	// The COUNT, not just the key: a report that always says 0 carries the key too and
	// would pass a bare substring check.
	if want := fmt.Sprintf("segments_dropped=%d", segsBefore-segsAfter); !strings.Contains(got, want) {
		t.Errorf("the report does not say how many segments went (want %q); logged: %q", want, got)
	}
}

// TestMaintenanceSaysNothingWhenItRotatedNothing: an archive with no retention configured is
// the common case, and it must not put a line in the log every pass.
func TestMaintenanceSaysNothingWhenItRotatedNothing(t *testing.T) {
	srv, arch, cleanup := rotateLogFixture(t, collections.Retention{})
	defer cleanup()

	segsBefore, countBefore := arch.Stats().Segments, arch.Count()
	if segsBefore <= 2 {
		t.Fatalf("fixture holds only %d segments; a rotation would have nothing to drop either way", segsBefore)
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	srv.maintainArchives(db.MaintainOptions{
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
	}, nil)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if arch.Stats().Segments != segsBefore || arch.Count() != countBefore {
		t.Fatalf("a pass over an archive with no retention configured changed it: segments %d -> %d, records %d -> %d",
			segsBefore, arch.Stats().Segments, countBefore, arch.Count())
	}
	if strings.Contains(got, "rotat") {
		t.Errorf("a pass that rotated nothing mentioned a rotation; logged: %q", got)
	}
}

// TestRotateFailureIsReportedAsAWarning: a rotation that could not release a dropped
// segment's file is a Warn carrying the error, and it still says how many segments went --
// the records are out of the table whether or not their files were released, so a report
// that only said "failed" would understate what happened.
func TestRotateFailureIsReportedAsAWarning(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	s := &Server{Logger: slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))}
	s.logRotate("history", db.RotateResult{Dropped: 3, Err: errors.New("unlink boom")})

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	for _, want := range []string{"level=WARN", "archive rotation failed", "archive=history",
		"unlink boom", "segments_dropped=3"} {
		if !strings.Contains(got, want) {
			t.Errorf("rotation failure report missing %q; logged: %q", want, got)
		}
	}
}
