package changefeed

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// assertContains asserts an archive holds exactly want records matching constraint.
func assertContains(t *testing.T, a *db.ArchiveTable, constraint string, want int) {
	t.Helper()
	seq, err := a.Query(constraint)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if n != want {
		t.Errorf("query %q matched %d, want %d", constraint, n, want)
	}
}

func mustCatalogArchive(t *testing.T, name string) (*db.Catalog, *db.ArchiveTable) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	a, err := cat.CreateArchiveTable(name, db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	return cat, a
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestHTTPFeed: end-to-end over a real HTTP server -- initial replay, live tail, SrcAttr stamping,
// and the ACK updating the source's GC floor.
func TestHTTPFeed(t *testing.T) {
	srcCat, srcArch := mustCatalogArchive(t, "history")
	reg := &MemRegistry{LeaseTTL: time.Hour}
	srv := httptest.NewServer(Handler(srcCat, ServerOptions{
		Registry: reg, AgeAttr: "EnteredHistoryTime", Heartbeat: 50 * time.Millisecond,
	}))
	defer srv.Close()

	app := func(a *db.ArchiveTable, text string) {
		if err := a.AppendOld(text); err != nil {
			t.Fatal(err)
		}
	}
	app(srcArch, `GlobalJobId = "ap40#12.0"; Owner = "alice"; ClusterId = 12; EnteredHistoryTime = 1700000100`)
	app(srcArch, `GlobalJobId = "ap40#13.0"; Owner = "bob";   ClusterId = 13; EnteredHistoryTime = 1700000200`)

	_, dst := mustCatalogArchive(t, "history")
	sink, err := replicate.NewArchiveSink(dst, "ap40", &replicate.MemCursorStore{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Pull(ctx, PullConfig{BaseURL: srv.URL, Table: "history", Subscriber: "sink1", Src: "ap40", AckEvery: 40 * time.Millisecond}, sink)
	}()

	waitFor(t, 5*time.Second, func() bool { return dst.Count() == 2 })
	assertContains(t, dst, `Owner == "alice" && Src == "ap40"`, 1)

	// Live tail: a new append on the source flows through the open stream.
	app(srcArch, `GlobalJobId = "ap40#14.0"; Owner = "carol"; ClusterId = 14; EnteredHistoryTime = 1700000300`)
	waitFor(t, 5*time.Second, func() bool { return dst.Count() == 3 })

	// The ACK advanced the source GC floor to the newest persisted record time (millis).
	waitFor(t, 5*time.Second, func() bool {
		floor, held := reg.Floor("history", time.Now())
		return held && floor == 1700000300*1000
	})
}

// TestHTTPFeedSelectiveFanIn: two sources fan into one sink archive, each with a server-side
// constraint (only alice's jobs), stamped by Src.
func TestHTTPFeedSelectiveFanIn(t *testing.T) {
	newSource := func(name string, rows []string) *httptest.Server {
		cat, arch := mustCatalogArchive(t, "history")
		for _, r := range rows {
			if err := arch.AppendOld(r); err != nil {
				t.Fatal(err)
			}
		}
		return httptest.NewServer(Handler(cat, ServerOptions{Heartbeat: 50 * time.Millisecond}))
	}
	srvA := newSource("history", []string{
		`GlobalJobId = "ap40#1.0"; Owner = "alice"; ClusterId = 1`,
		`GlobalJobId = "ap40#2.0"; Owner = "eve";   ClusterId = 2`,
	})
	defer srvA.Close()
	srvB := newSource("history", []string{
		`GlobalJobId = "ap55#1.0"; Owner = "alice"; ClusterId = 1`,
		`GlobalJobId = "ap55#9.0"; Owner = "mallory"; ClusterId = 9`,
	})
	defer srvB.Close()

	_, dst := mustCatalogArchive(t, "central")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, s := range []struct{ url, src string }{{srvA.URL, "ap40"}, {srvB.URL, "ap55"}} {
		sink, err := replicate.NewArchiveSink(dst, s.src, &replicate.MemCursorStore{})
		if err != nil {
			t.Fatal(err)
		}
		cfg := PullConfig{BaseURL: s.url, Table: "history", Subscriber: "central-" + s.src, Src: s.src,
			Constraint: `Owner == "alice"`, AckEvery: 40 * time.Millisecond}
		go func() { _ = Pull(ctx, cfg, sink) }()
	}

	// Only the two alice rows (one per source) land; eve/mallory are filtered server-side.
	waitFor(t, 5*time.Second, func() bool { return dst.Count() == 2 })
	assertContains(t, dst, `Owner == "alice"`, 2)
	assertContains(t, dst, `Src == "ap40"`, 1)
	assertContains(t, dst, `Src == "ap55"`, 1)
	assertContains(t, dst, `Owner == "eve" || Owner == "mallory"`, 0)
}
