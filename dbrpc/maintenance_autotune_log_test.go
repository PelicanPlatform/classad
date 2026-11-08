package dbrpc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
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

// TestStartMaintenanceReportsWhatTheTickTuned drives the real loop rather than one pass of
// it: StartMaintenance enumerates the catalog each tick, and the per-table hook it installs
// is what carries a mutable table's tuning out to the log. A pass called directly would not
// exercise that wiring, which is the part that can be wrong.
func TestStartMaintenanceReportsWhatTheTickTuned(t *testing.T) {
	_, srv, cleanup := catServerPairWithServer(t)
	defer cleanup()
	d, err := srv.cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("u%d", i%7))
		if err := d.Put(fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		seq, qerr := d.Query(`Owner == "u3"`)
		if qerr != nil {
			t.Fatal(qerr)
		}
		for range seq {
		}
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	srv.StartMaintenance(50*time.Millisecond, db.MaintainOptions{
		MinIndexDemand:       5,
		ArchiveMergeDisabled: true,
	})

	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		mu.Lock()
		got = buf.String()
		mu.Unlock()
		if strings.Contains(got, "maintenance added an index") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cat, _ := d.IndexedAttrs(); !slices.Contains(cat, "Owner") {
		t.Fatalf("the tick's tuner indexed %v, not Owner: there is nothing for it to report", cat)
	}
	if !strings.Contains(got, "maintenance added an index") || !strings.Contains(got, "attr=Owner") {
		t.Errorf("a maintenance tick indexed Owner and did not report it; logged: %q", got)
	}
	if !strings.Contains(got, "table=jobs") {
		t.Errorf("the report does not name the table it fired for; logged: %q", got)
	}
}

// TestLogAutoTuneNamesADrop: a dropped index says so. The budget trimmer and the unused-drop
// both emit "drop" changes, and reporting one as an add would read as the opposite of what
// happened -- an index appearing where one had just been reclaimed.
func TestLogAutoTuneNamesADrop(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	s := &Server{Logger: slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))}
	s.logAutoTune("history", "archive", db.AutoTuneResult{
		Changed: true,
		Changes: []db.AutoTuneChange{
			{Attr: "ClusterId+ProcId", Kind: "composite", Action: "drop", Reason: "over-budget"},
			{Attr: "Owner", Kind: "categorical", Action: "add", Reason: "demand"},
		},
	})
	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "maintenance dropped an index") || !strings.Contains(got, "attr=ClusterId+ProcId") {
		t.Errorf("the drop was not reported as a drop; logged: %q", got)
	}
	if !strings.Contains(got, "maintenance added an index") || !strings.Contains(got, "attr=Owner") {
		t.Errorf("the add alongside it was lost; logged: %q", got)
	}
}
