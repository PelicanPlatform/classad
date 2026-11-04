package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

func localPair(t *testing.T, includePrivate bool) (*LocalSession, *Client, func()) {
	t.Helper()
	// SMALL SEGMENTS, so the arena actually SEALS. At the default 8 MiB nothing seals on a
	// fixture this size, every read is a record scan whatever its backing, and a comparison
	// between the two paths is a comparison of the record path with itself. assertColumnar
	// below holds the fixture to this rather than trusting it.
	d, err := db.OpenConfig(db.Config{Dir: t.TempDir(), SegmentSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 20000; i++ {
		claim := privateSecret
		if i%2 == 1 {
			claim = fmt.Sprintf("other-%d", i)
		}
		tx.NewClassAdOld(fmt.Sprintf("j%d", i), fmt.Sprintf(
			"Cpus = %d\nJobStatus = %d\nOwner = \"u%d\"\nClaimId = %q\nCapability = \"cap-%d\"",
			i%8, i%5, i%16, claim, i))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		d.EnableSchemaScan(4096, 8)
	}
	assertColumnar(t, d)
	s := NewServer(d)
	opts := ServeOptions{ReadOnly: true, IncludePrivate: includePrivate}
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn, WithPeerSessionAttrs(ServerSessionAttrs()))
	return s.LocalSession(opts), c, func() { c.Close(); s.Close(); d.Close() }
}

// assertColumnar fails the FIXTURE, not the test, when a read of it would not take the column
// path. A fixture that record-scans answers every question here correctly and proves nothing about
// the path under test, and at the default segment size that is exactly what this one did.
//
// It asks about a SCHEMA'D attribute. A private attribute never gets a column -- ClaimId is absent
// from the derived schema at any cardinality -- so the secret below is always read from the
// record remnant, and asserting on it would assert the fixture is broken.
func assertColumnar(t *testing.T, d *db.DB) {
	t.Helper()
	info := d.SchemaScanInfo()
	if !info.Enabled || info.ColumnarSegments == 0 {
		t.Fatalf("fixture has %d of %d sealed segments in columnar form (enabled=%v): nothing "+
			"would be read from columns, so it cannot pose the question",
			info.ColumnarSegments, info.SealedSegments, info.Enabled)
	}
	var st collections.ColumnScanStats
	seq, err := d.QueryColumnsStats("true", []string{"Cpus"}, false, &st)
	if err != nil {
		t.Fatalf("fixture cannot serve a column scan: %v", err)
	}
	for range seq {
	}
	if st.ColumnCells <= st.RecordCells {
		t.Fatalf("fixture read %d cells from columns and %d from records even for a schema'd "+
			"attribute: it is record-scanning", st.ColumnCells, st.RecordCells)
	}
}

// TestLocalSessionRefusesPrivateConstraints: the in-process reader is a NEW read op, and the gate's
// own comment says what happens to a read op that forgets the gate -- it reopens the oracle. So it
// faces the same vectors every framed op faces, from the same list.
func TestLocalSessionRefusesPrivateConstraints(t *testing.T) {
	ls, _, cleanup := localPair(t, false)
	defer cleanup()
	for _, constraint := range privateConstraints {
		t.Run(constraint, func(t *testing.T) {
			rows := 0
			err := ls.ScanColumns(context.Background(), DefaultTable, constraint, []string{"Cpus"}, false,
				func(b *collections.Batch) bool { rows += b.N; return true })
			if err == nil {
				t.Fatalf("answered with %d rows; an unprivileged reader must be REFUSED, because "+
					"answering is itself the leak", rows)
			}
			if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "dynamic") {
				t.Fatalf("refused with %v, which does not look like the private-attribute gate", err)
			}
		})
	}
}

// TestLocalSessionIsNotRefusedWhenPrivileged is the other half: the gate must not refuse a session
// that IS allowed to read secrets, or the test above would pass against a reader that refuses
// everything.
func TestLocalSessionIsNotRefusedWhenPrivileged(t *testing.T) {
	ls, _, cleanup := localPair(t, true)
	defer cleanup()
	for _, constraint := range privateConstraints {
		err := ls.ScanColumns(context.Background(), DefaultTable, constraint, []string{"Cpus"}, false,
			func(b *collections.Batch) bool { return true })
		// A fall-through is not a refusal: some query shapes cannot take the column path at
		// all (a dynamic reference, for one), and the caller then reads them framed. What
		// must not happen is the GATE refusing a session that may see secrets.
		if errors.Is(err, ErrLocalUnsupported) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: privileged reader refused with %v", constraint, err)
		}
	}
}

// TestLocalSessionRedactsLikeTheWire: the same session options must produce the same VALUES through
// both paths. Redaction is the other half of what IncludePrivate decides, and a reader that skipped
// it would return the secret while the framed path returned undefined.
//
// The secret arrives through the RECORD remnant, not a column: a private attribute is absent from
// the derived schema whatever its cardinality, so a columnar read of one always falls back. That is
// the path worth covering here, because it is the only path a private attribute ever takes -- but
// the fixture is columnar (assertColumnar) so the rest of the read is the path under test.
func TestLocalSessionRedactsLikeTheWire(t *testing.T) {
	for _, includePrivate := range []bool{false, true} {
		t.Run(fmt.Sprintf("includePrivate=%v", includePrivate), func(t *testing.T) {
			ls, c, cleanup := localPair(t, includePrivate)
			defer cleanup()
			attrs := []string{"Cpus", "ClaimId"}

			local := map[string]string{}
			if err := ls.ScanColumns(context.Background(), DefaultTable, "true", attrs, false,
				func(b *collections.Batch) bool {
					for i := 0; i < b.N; i++ {
						local[fmt.Sprint(b.Cols[0].Num[i])] = cellText(&b.Cols[1], i)
					}
					return true
				}); err != nil {
				t.Fatal(err)
			}

			wire := map[string]string{}
			if err := c.QueryValuesStream(context.Background(), DefaultTable, "true", attrs, 0, false,
				func(vb *ValueBatch) bool {
					cpus := vb.Column(0, nil)
					claim := vb.Column(1, nil)
					for i := range cpus {
						n, _ := cpus[i].IntValue()
						wire[fmt.Sprint(n)] = claim[i].String()
					}
					return true
				}); err != nil {
				t.Fatal(err)
			}

			if len(local) == 0 || len(local) != len(wire) {
				t.Fatalf("local read %d rows, wire %d", len(local), len(wire))
			}
			for k, lv := range local {
				wv := wire[k]
				// The wire renders a string with quotes and undefined as "undefined"; compare
				// on whether the SECRET is present, which is the property that matters.
				lHas := strings.Contains(lv, privateSecret)
				wHas := strings.Contains(wv, privateSecret)
				if lHas != wHas {
					t.Fatalf("Cpus=%s: local %q, wire %q -- the two paths disagree about "+
						"whether this session may see the secret", k, lv, wv)
				}
				if !includePrivate && lHas {
					t.Fatalf("Cpus=%s: unprivileged local read returned the secret (%q)", k, lv)
				}
			}
			// Premise: a privileged session must actually SEE the secret here, or the
			// comparison above is between two redacted reads and proves nothing.
			if includePrivate {
				found := false
				for _, v := range local {
					if strings.Contains(v, privateSecret) {
						found = true
					}
				}
				if !found {
					t.Fatal("the privileged read saw no secret; the fixture does not pose the question")
				}
			}
		})
	}
}

func cellText(c *collections.Column, i int) string {
	switch c.Kind[i] {
	case collections.ValueString:
		return c.Str[i]
	case collections.ValueUndefined:
		return "undefined"
	default:
		return fmt.Sprint(c.Num[i])
	}
}

// TestLocalSessionJoinsTheShutdownBarrier: an in-process read is not a framed request, so nothing
// else makes Close wait for it. Without the barrier, Close returns while a read is still walking
// the catalog, and the catalog closes under it.
func TestLocalSessionJoinsTheShutdownBarrier(t *testing.T) {
	ls, _, cleanup := localPair(t, true)
	defer cleanup()

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		first := true
		done <- ls.ScanColumns(context.Background(), DefaultTable, "true", []string{"Cpus"}, false,
			func(b *collections.Batch) bool {
				if first {
					first = false
					close(started)
					<-release // hold the read open across the Close below
				}
				return true
			})
	}()
	<-started

	closed := make(chan struct{})
	go func() { ls.s.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a read was still in flight; the barrier is not held")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the read failed: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the read finished")
	}
}

// TestLocalSessionRefusedAfterClose: once Close has begun, a read is not served at all, rather
// than being served against a catalog that is going away.
func TestLocalSessionRefusedAfterClose(t *testing.T) {
	ls, _, cleanup := localPair(t, true)
	defer cleanup()
	ls.s.Close()
	err := ls.ScanColumns(context.Background(), DefaultTable, "true", []string{"Cpus"}, false,
		func(b *collections.Batch) bool { return true })
	if !errors.Is(err, ErrServerClosed) {
		t.Fatalf("got %v, want ErrServerClosed", err)
	}
}

// TestLocalSessionContainsAPanic: a panic while serving one read must fail that read and leave the
// server able to serve the next. Without containment it unwinds into the caller's goroutine, which
// for a Flight read is a gRPC handler -- and takes the daemon with it.
func TestLocalSessionContainsAPanic(t *testing.T) {
	ls, _, cleanup := localPair(t, true)
	defer cleanup()
	before := RequestPanics()

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped the read: %v", r)
			}
		}()
		return ls.ScanColumns(context.Background(), DefaultTable, "true", []string{"Cpus"}, false,
			func(b *collections.Batch) bool { panic("injected: consumer panicked") })
	}()
	if err == nil {
		t.Fatal("the read reported success after panicking")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("failed with %v, which does not name the panic", err)
	}
	if RequestPanics() == before {
		t.Error("the panic was not counted, so an operator sees no sign of it")
	}
	// The server still works.
	rows := 0
	if err := ls.ScanColumns(context.Background(), DefaultTable, "true", []string{"Cpus"}, false,
		func(b *collections.Batch) bool { rows += b.N; return true }); err != nil {
		t.Fatalf("the next read failed after a contained panic: %v", err)
	}
	if rows == 0 {
		t.Fatal("the next read returned nothing")
	}
}

// TestLocalSessionFallsThroughWhenColumnsAreUnavailable: a table that cannot serve a column scan
// must say so BEFORE yielding anything, so the caller can take the framed path and get the same
// answer. A reader that instead returned an empty result, or a partial one, would turn "this
// table is not columnar" into "this table has no rows".
func TestLocalSessionFallsThroughWhenColumnsAreUnavailable(t *testing.T) {
	// No EnableSchemaScan: the accelerator is off, so there are no columns to read. Same for a
	// table whose rows are all still in the open segment.
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	tx.NewClassAdOld("j1", "Cpus = 4\nJobStatus = 2")
	if cerr := tx.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	s := NewServer(d)
	defer s.Close()
	ls := s.LocalSession(ServeOptions{ReadOnly: true, IncludePrivate: true})

	yielded := 0
	err = ls.ScanColumns(context.Background(), DefaultTable, "true", []string{"Cpus"}, false,
		func(b *collections.Batch) bool { yielded++; return true })
	if !errors.Is(err, ErrLocalUnsupported) {
		t.Fatalf("got %v, want ErrLocalUnsupported so the caller falls back", err)
	}
	if yielded != 0 {
		t.Fatalf("yielded %d batches before declining: a fall-through must happen before any "+
			"rows reach the caller, or the caller cannot retry on the framed path", yielded)
	}
}

// TestLocalSessionRejectsAnEmptyProjection pins the one argument check: a column scan with no
// attributes has nothing to read, and the framed op refuses it too.
func TestLocalSessionRejectsAnEmptyProjection(t *testing.T) {
	ls, _, cleanup := localPair(t, true)
	defer cleanup()
	if err := ls.ScanColumns(context.Background(), DefaultTable, "true", nil, false,
		func(b *collections.Batch) bool { return true }); err == nil {
		t.Fatal("an empty projection was accepted")
	}
}

// TestLocalSessionUnknownTable: a name that resolves to nothing is an error, not an empty read.
func TestLocalSessionUnknownTable(t *testing.T) {
	ls, _, cleanup := localPair(t, true)
	defer cleanup()
	err := ls.ScanColumns(context.Background(), "no-such-table", "true", []string{"Cpus"}, false,
		func(b *collections.Batch) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("got %v, want a no-such-table error", err)
	}
}
