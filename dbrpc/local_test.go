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
	// A COLUMNAR table, not an in-memory one: an in-memory table declines the column scan, both
	// paths fall through to the record scan, and a comparison between them would be comparing
	// the framed path with itself.
	dir := t.TempDir()
	cat, err := db.OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := cat.CreateTable(DefaultTable)
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 2000; i++ {
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
		d.EnableSchemaScan(2000, 8)
	}
	s := NewServerCatalog(cat)
	opts := ServeOptions{ReadOnly: true, IncludePrivate: includePrivate}
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn, WithPeerSessionAttrs(ServerSessionAttrs()))
	return s.LocalSession(opts), c, func() { c.Close(); s.Close(); cat.Close() }
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
