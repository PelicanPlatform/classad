package dbrpc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// A retrain is the most consequential thing the archive pass does on its own schedule -- the
// first one reseals the whole history -- and db does not log, so the only way an operator
// learns it happened is this report. These cover the wiring: that the pass asks for the hook,
// that the hook names the archive it fired for, and that a pass which does not retrain is
// silent.

// retrainLogFixture is an archive of dictionary-friendly ads, so training succeeds rather than
// declining; a declined retrain reports something different and would not prove the success
// path works.
func retrainLogFixture(t *testing.T) (*Server, *db.ArchiveTable, func()) {
	t.Helper()
	c, srv, cleanup := catServerPairWithServer(t)
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 13}); err != nil {
		cleanup()
		t.Fatal(err)
	}
	for i := range 600 {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf(
			"ClusterId = %d\nOwner = \"user_%d\"\nJobStatus = \"Completed\"\n"+
				"Cmd = \"/usr/bin/some_long_repeated_command_path\"\nArgs = \"--flag=value\"",
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

// TestMaintenanceReportsTheDictionaryItRetrained: the pass wires OnRetrain and the report
// names the archive. Without the wiring the retrain still happens and nothing says so, which
// on a history-scale archive is hours of I/O with no line to connect it to.
func TestMaintenanceReportsTheDictionaryItRetrained(t *testing.T) {
	srv, arch, cleanup := retrainLogFixture(t)
	defer cleanup()

	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	srv.maintainArchives(db.MaintainOptions{
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
		// A nanosecond: the clock starts when the archive is opened, so anything longer would
		// not have elapsed and nothing would be reported.
		ArchiveRetrainInterval: time.Nanosecond,
	}, nil)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if arch.CodecStats(16).LastRetrain.IsZero() {
		t.Fatalf("no retrain happened, so there is nothing for the pass to report; logged: %q", got)
	}
	if !strings.Contains(got, "retrained archive dictionary") {
		t.Errorf("the pass retrained and did not report it; logged: %q", got)
	}
	if !strings.Contains(got, "archive=history") {
		t.Errorf("the report does not name the archive it fired for; logged: %q", got)
	}
	if !strings.Contains(got, "dict_bytes=") {
		t.Errorf("the report does not say how big a dictionary was trained; logged: %q", got)
	}
}

// TestMaintenanceSaysNothingWhenItDidNotRetrain: the interval is the common case -- a pass
// that does not retrain must be silent, or the log carries a line per archive per pass.
func TestMaintenanceSaysNothingWhenItDidNotRetrain(t *testing.T) {
	srv, arch, cleanup := retrainLogFixture(t)
	defer cleanup()

	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	// Unset, which is also the default: retraining is off unless asked for.
	srv.maintainArchives(db.MaintainOptions{
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
	}, nil)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !arch.CodecStats(16).LastRetrain.IsZero() {
		t.Fatal("the pass retrained with the interval unset")
	}
	if strings.Contains(got, "retrain") {
		t.Errorf("a pass that did not retrain mentioned one; logged: %q", got)
	}
}
