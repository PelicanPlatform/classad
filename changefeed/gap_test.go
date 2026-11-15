package changefeed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// rotatingArchive is an archive sized so a few hundred records span several segments, with
// retention tight enough that Rotate drops the oldest of them.
func rotatingArchive(t *testing.T, name string) (*db.Catalog, *db.ArchiveTable) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	a, err := cat.CreateArchiveTable(name, db.ArchiveConfig{
		SegmentSize: 1 << 13,
		Retention:   collections.Retention{MaxSegments: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cat, a
}

const ageAttr = "EnteredHistoryTime"

// appendAt appends one record stamped at unix second ts.
func appendAt(t *testing.T, a *db.ArchiveTable, id int, ts int64) {
	t.Helper()
	if err := a.AppendOld(fmt.Sprintf(
		`GlobalJobId = "ap#%d.0"; ClusterId = %d; %s = %d`, id, id, ageAttr, ts)); err != nil {
		t.Fatal(err)
	}
}

// readEvents opens an SSE subscription and returns every event up to and including the first
// Synced, or fails on timeout.
func readEvents(t *testing.T, base string, q url.Values) []Event {
	t.Helper()
	u := base + PathSubscribe + "?" + q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribe: %s", resp.Status)
	}
	var out []Event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var ev Event
		if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
		if ev.Kind == replicate.KindSynced {
			return out
		}
	}
	t.Fatalf("stream ended after %d events without a Synced", len(out))
	return nil
}

func kinds(evs []Event) []replicate.Kind {
	out := make([]replicate.Kind, 0, len(evs))
	for i := range evs {
		out = append(out, evs[i].Kind)
	}
	return out
}

// staleCursor appends records, takes a cursor over them, appends many more, and rotates so
// the cursor now points below the retained floor. It returns that cursor and the age of the
// oldest record still retained.
func staleCursor(t *testing.T, srv *httptest.Server, a *db.ArchiveTable, table string) (cursor string, oldestMillis int64) {
	t.Helper()
	const base = 1700000000
	for i := range 5 {
		appendAt(t, a, i, base+int64(i))
	}
	evs := readEvents(t, srv.URL, url.Values{"table": {table}, "subscriber": {"c1"}, "src": {"s"}})
	cur := evs[len(evs)-1].Cursor
	if len(cur) == 0 {
		t.Fatal("no cursor at Synced")
	}
	for i := 5; i < 400; i++ {
		appendAt(t, a, i, base+int64(i))
	}
	dropped, err := a.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("rotation dropped nothing, so the cursor is still resumable and the test proves nothing")
	}
	// What the replay will now start from: the oldest record still retained.
	after := readEvents(t, srv.URL, url.Values{"table": {table}, "subscriber": {"probe"}, "src": {"s"}})
	for i := range after {
		if after[i].Kind == replicate.KindUpsert {
			return base64.StdEncoding.EncodeToString(cur), after[i].TS
		}
	}
	t.Fatal("replay yielded no records")
	return "", 0
}

// TestOutOfRetentionResetAnnouncesTheGap is ask 3: a cursor the source cannot honour because
// the records are GONE used to be indistinguishable from one it is merely re-sending. The
// subscriber re-ingested the whole archive and learned nothing; an operator learned nothing
// at all. Now it is told, with the window.
func TestOutOfRetentionResetAnnouncesTheGap(t *testing.T) {
	cat, a := rotatingArchive(t, "transfers")
	reg := &MemRegistry{LeaseTTL: time.Hour}
	srv := httptest.NewServer(Handler(cat, ServerOptions{
		Registry: reg, AgeAttr: ageAttr, Heartbeat: time.Hour,
	}))
	defer srv.Close()

	cursor, oldestRetained := staleCursor(t, srv, a, "transfers")
	if oldestRetained == 0 {
		t.Fatal("the retained records carry no timestamp, so the window has no far end to check")
	}
	// What the source recorded this subscriber as having persisted: the near end of the window.
	const acked = 1700000004000
	reg.Ack("transfers", "c1", acked, time.Now())

	evs := readEvents(t, srv.URL, url.Values{
		"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "cursor": {cursor},
	})
	if len(evs) < 2 {
		t.Fatalf("expected at least a Gap and a Reset; got %v", kinds(evs))
	}
	gap, reset := evs[0], evs[1]
	if gap.Kind != replicate.KindGap {
		t.Fatalf("first event = %v, want gap (it has to precede the replay it describes); got %v", gap.Kind, kinds(evs))
	}
	if reset.Kind != replicate.KindReset {
		t.Fatalf("second event = %v, want reset; got %v", reset.Kind, kinds(evs))
	}
	if reset.Reason != replicate.ResetRetention {
		t.Errorf("reset reason = %q, want %q", reset.Reason, replicate.ResetRetention)
	}
	if gap.FromMillis != acked {
		t.Errorf("gap.FromMillis = %d, want %d (the subscriber's recorded ack)", gap.FromMillis, acked)
	}
	if gap.ToMillis != oldestRetained {
		t.Errorf("gap.ToMillis = %d, want %d (the oldest record still retained)", gap.ToMillis, oldestRetained)
	}
	if gap.ToMillis <= gap.FromMillis {
		t.Errorf("gap window %d..%d is empty or inverted; it should name records that went missing", gap.FromMillis, gap.ToMillis)
	}
	if gap.Ver >= reset.Ver {
		t.Errorf("gap ver %d is not below the reset's %d; versions must be monotonic in delivery order", gap.Ver, reset.Ver)
	}
}

// TestResumableCursorGetsNoGap is the negative arm. A Gap on every reconnect would be worse
// than none: an operator would learn to ignore it.
func TestResumableCursorGetsNoGap(t *testing.T) {
	cat, a := rotatingArchive(t, "transfers")
	srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
	defer srv.Close()

	for i := range 5 {
		appendAt(t, a, i, 1700000000+int64(i))
	}
	q := url.Values{"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}}
	evs := readEvents(t, srv.URL, q)
	cur := evs[len(evs)-1].Cursor
	if evs[0].Kind != replicate.KindReset || evs[0].Reason != replicate.ResetInitial {
		t.Errorf("first subscribe: got %v/%q, want a reset with reason initial", evs[0].Kind, evs[0].Reason)
	}
	appendAt(t, a, 99, 1700000099)

	q.Set("cursor", base64.StdEncoding.EncodeToString(cur))
	resumed := readEvents(t, srv.URL, q)
	for _, e := range resumed {
		if e.Kind == replicate.KindGap {
			t.Fatalf("a resumable cursor was told it lost records: %v", kinds(resumed))
		}
		if e.Kind == replicate.KindReset {
			t.Fatalf("a resumable cursor was Reset (reason %q): %v", e.Reason, kinds(resumed))
		}
	}
}
