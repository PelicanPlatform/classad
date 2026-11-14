package changefeed

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// upsertTS returns the timestamps of the upserts in a replay, in delivery order.
func upsertTS(evs []Event) []int64 {
	var out []int64
	for _, e := range evs {
		if e.Kind == replicate.KindUpsert {
			out = append(out, e.TS)
		}
	}
	return out
}

// subscribeStatus starts a subscription and returns the HTTP status, for the cases the
// handler should refuse outright.
func subscribeStatus(t *testing.T, base string, q url.Values) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+PathSubscribe+"?"+q.Encode(), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf) //nolint:errcheck // the status is what is under test; a short body is fine
	return resp.StatusCode, string(buf[:n])
}

// TestSinceBoundsAReplayThatLostHistory is ask 2(a): a collector that has been away does not
// want "discard and rebuild from everything you still hold" -- for an append-only event log
// that can be the whole byte cap, from every source at once, after a fleet restart. since is
// the subscriber's own floor on a replay it did not ask to receive in full.
func TestSinceBoundsAReplayThatLostHistory(t *testing.T) {
	cat, a := rotatingArchive(t, "transfers")
	srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
	defer srv.Close()

	cursor, _ := staleCursor(t, srv, a, "transfers")

	// What the source would send unbounded.
	full := upsertTS(readEvents(t, srv.URL, url.Values{
		"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "cursor": {cursor},
	}))
	if len(full) < 10 {
		t.Fatalf("unbounded replay delivered %d records; too few to bound meaningfully", len(full))
	}
	// A floor in the middle of what is retained.
	since := full[len(full)/2]

	bounded := upsertTS(readEvents(t, srv.URL, url.Values{
		"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"},
		"cursor": {cursor}, "since": {int64str(since)},
	}))
	if len(bounded) == 0 {
		t.Fatal("bounded replay delivered nothing")
	}
	if len(bounded) >= len(full) {
		t.Errorf("since did not bound anything: %d records with a floor, %d without", len(bounded), len(full))
	}
	for _, ts := range bounded {
		if ts < since {
			t.Errorf("record at %d is below the requested floor %d", ts, since)
		}
	}
	// The gap still names where the SOURCE's history starts, not where this subscriber
	// chose to begin: the two are different facts and only the first is a loss.
	evs := readEvents(t, srv.URL, url.Values{
		"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"},
		"cursor": {cursor}, "since": {int64str(since)},
	})
	if evs[0].Kind != replicate.KindGap {
		t.Fatalf("no gap in front of a bounded replay: %v", kinds(evs))
	}
	if evs[0].ToMillis != full[0] {
		t.Errorf("gap.ToMillis = %d, want %d (where the source's history starts, not the subscriber's floor %d)",
			evs[0].ToMillis, full[0], since)
	}
}

// TestSinceIsIgnoredOnAResumableCursor pins the half of the semantics that is easy to get
// wrong. since is the fallback for a cursor that could NOT be honoured. Applying it to one
// that resumed cleanly would silently drop records this sink has not seen, and the cursor it
// then commits would skip them for good.
func TestSinceIsIgnoredOnAResumableCursor(t *testing.T) {
	cat, a := rotatingArchive(t, "transfers")
	srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
	defer srv.Close()

	const base = 1700000000
	for i := range 5 {
		appendAt(t, a, i, base+int64(i))
	}
	q := url.Values{"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}}
	cur := readEvents(t, srv.URL, q)
	cursor := cur[len(cur)-1].Cursor

	// One more record, older than the floor the subscriber asks for.
	appendAt(t, a, 99, base+6)

	q.Set("cursor", base64.StdEncoding.EncodeToString(cursor))
	q.Set("since", int64str((base+1000)*1000)) // far in the future of every record
	got := upsertTS(readEvents(t, srv.URL, q))
	if len(got) != 1 {
		t.Fatalf("a resumable cursor delivered %d records with a future since; want the 1 it had not seen", len(got))
	}
}

// TestSinceIsRefusedWhereItCannotBeHonoured: refused, not ignored. A subscriber that asked
// to bound its replay and was quietly handed everything is the outcome the parameter exists
// to prevent.
func TestSinceIsRefusedWhereItCannotBeHonoured(t *testing.T) {
	t.Run("mutable-table", func(t *testing.T) {
		cat, err := db.OpenCatalog(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer cat.Close()
		if _, err := cat.CreateTable("active"); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
		defer srv.Close()
		code, body := subscribeStatus(t, srv.URL, url.Values{
			"table": {"active"}, "subscriber": {"c1"}, "src": {"s"}, "since": {"1700000000000"},
		})
		if code != http.StatusBadRequest {
			t.Errorf("since on a mutable table: status %d, want 400 (body %q)", code, body)
		}
	})

	t.Run("no-age-attribute", func(t *testing.T) {
		cat, _ := rotatingArchive(t, "transfers")
		srv := httptest.NewServer(Handler(cat, ServerOptions{Heartbeat: time.Hour}))
		defer srv.Close()
		code, body := subscribeStatus(t, srv.URL, url.Values{
			"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "since": {"1700000000000"},
		})
		if code != http.StatusBadRequest {
			t.Errorf("since with no age attribute: status %d, want 400 (body %q)", code, body)
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		cat, _ := rotatingArchive(t, "transfers")
		srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
		defer srv.Close()
		for _, bad := range []string{"yesterday", "-5", "1.5"} {
			code, body := subscribeStatus(t, srv.URL, url.Values{
				"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "since": {bad},
			})
			if code != http.StatusBadRequest {
				t.Errorf("since=%q: status %d, want 400 (body %q)", bad, code, body)
			}
		}
	})
}

func int64str(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
