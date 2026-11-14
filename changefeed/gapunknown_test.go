package changefeed

import (
	"encoding/base64"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// openArchiveAt opens (or reopens) a catalog at dir and returns its transfers archive,
// sized so a few hundred records span several segments.
func openArchiveAt(t *testing.T, dir string) (*db.Catalog, *db.ArchiveTable) {
	t.Helper()
	cat, err := db.OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := cat.ArchiveTable("transfers")
	if !ok {
		a, err = cat.CreateArchiveTable("transfers", db.ArchiveConfig{SegmentSize: 1 << 13})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SetRetention(collections.Retention{MaxSegments: 2}); err != nil {
		t.Fatal(err)
	}
	return cat, a
}

// crashTheSource simulates an unclean shutdown: the clean-shutdown watch.epoch marker is
// what a Close writes and what an Open consumes, so a source that died never left one and
// the next Open picks a fresh epoch. Removing it is the supported way to produce that state
// (collections/watchepoch.go says to do exactly this when restoring from a copy).
func crashTheSource(t *testing.T, dir string) {
	t.Helper()
	marker := filepath.Join(dir, "archives", "transfers", "watch.epoch")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no watch.epoch after a clean Close, so this is not simulating a crash: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
}

// firstUpsertTS returns the timestamp of the first record a fresh subscriber is replayed:
// where the source's retained history now starts.
func firstUpsertTS(t *testing.T, srvURL string) int64 {
	t.Helper()
	evs := readEvents(t, srvURL, url.Values{
		"table": {"transfers"}, "subscriber": {"probe"}, "src": {"s"},
	})
	for i := range evs {
		if evs[i].Kind == replicate.KindUpsert {
			return evs[i].TS
		}
	}
	t.Fatal("replay yielded no records")
	return 0
}

// TestUncleanRestartThatRotatedAnnouncesTheGap is the case a reason-gated Gap missed. An
// unclean restart rotates the epoch, so the cursor is refused as `epoch` before the append
// floor is ever consulted -- and the floor could not settle it anyway, since sequences from
// another store generation are not comparable to this one's. If the source ALSO rotated past
// that cursor while it was down, records are gone and the subscriber used to be told nothing:
// a full replay, no Gap, and a `reason` whose documented meaning was "still there".
//
// This is the fleet-upgrade shape: hundreds of sources restarted at once, some uncleanly.
func TestUncleanRestartThatRotatedAnnouncesTheGap(t *testing.T) {
	dir := t.TempDir()
	const base = 1700000000

	cat, a := openArchiveAt(t, dir)
	srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
	for i := range 5 {
		appendAt(t, a, i, base+int64(i))
	}
	evs := readEvents(t, srv.URL, url.Values{"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}})
	cursor := base64.StdEncoding.EncodeToString(evs[len(evs)-1].Cursor)
	srv.Close()
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	crashTheSource(t, dir)

	cat2, a2 := openArchiveAt(t, dir)
	defer cat2.Close()
	reg := &MemRegistry{LeaseTTL: time.Hour}
	srv2 := httptest.NewServer(Handler(cat2, ServerOptions{
		Registry: reg, AgeAttr: ageAttr, Heartbeat: time.Hour,
	}))
	defer srv2.Close()
	for i := 5; i < 400; i++ {
		appendAt(t, a2, i, base+int64(i))
	}
	dropped, err := a2.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("rotation dropped nothing, so no records were actually lost and the test proves nothing")
	}
	oldest := firstUpsertTS(t, srv2.URL)

	got := readEvents(t, srv2.URL, url.Values{
		"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "cursor": {cursor},
	})
	if got[0].Kind != replicate.KindGap {
		t.Fatalf("an unclean restart that rotated past the cursor sent no Gap: %v", kinds(got))
	}
	if got[0].ToMillis != oldest {
		t.Errorf("gap.ToMillis = %d, want %d (where the retained history now starts)", got[0].ToMillis, oldest)
	}
	// The near end is genuinely unknown here: the source died, and the default registry
	// died with it. 0 says so rather than inventing a bound.
	if got[0].FromMillis != 0 {
		t.Errorf("gap.FromMillis = %d, want 0 (the source has no surviving record of this subscriber's ack)", got[0].FromMillis)
	}
	if got[1].Kind != replicate.KindReset {
		t.Fatalf("second event = %v, want the reset the gap introduces: %v", got[1].Kind, kinds(got))
	}
	if got[1].Reason != replicate.ResetEpoch {
		t.Errorf("reset reason = %q, want %q", got[1].Reason, replicate.ResetEpoch)
	}
	if got[0].Ver >= got[1].Ver {
		t.Errorf("gap ver %d is not below the reset's %d", got[0].Ver, got[1].Ver)
	}
}

// TestAnAckCoveringTheOldestRecordRulesOutLoss is the suppression, and the reason the Gap is
// not simply emitted on every Reset. A subscriber whose ack already reaches the oldest record
// the source still holds lost nothing, whatever made its cursor unusable -- everything from
// there on is still here and is about to be replayed. A Gap on every reconnect would teach an
// operator to ignore gaps.
//
// The boundary is the point: an ack EXACTLY at the oldest retained record is still complete
// coverage, because that record is the first one the replay will deliver.
func TestAnAckCoveringTheOldestRecordRulesOutLoss(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int64 // added to the oldest retained record's timestamp
	}{
		{"ack-exactly-at-the-oldest-retained-record", 0},
		{"ack-past-the-oldest-retained-record", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const base = 1700000000

			cat, a := openArchiveAt(t, dir)
			srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
			for i := range 5 {
				appendAt(t, a, i, base+int64(i))
			}
			evs := readEvents(t, srv.URL, url.Values{"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}})
			cursor := base64.StdEncoding.EncodeToString(evs[len(evs)-1].Cursor)
			srv.Close()
			if err := cat.Close(); err != nil {
				t.Fatal(err)
			}

			crashTheSource(t, dir) // epoch reset, but nothing is rotated away

			cat2, a2 := openArchiveAt(t, dir)
			defer cat2.Close()
			reg := &MemRegistry{LeaseTTL: time.Hour}
			srv2 := httptest.NewServer(Handler(cat2, ServerOptions{
				Registry: reg, AgeAttr: ageAttr, Heartbeat: time.Hour,
			}))
			defer srv2.Close()
			appendAt(t, a2, 99, base+99)

			oldest := firstUpsertTS(t, srv2.URL)
			// A durable registry is what makes this knowable; MemRegistry stands in for one.
			reg.Ack("transfers", "c1", oldest+tc.offset, time.Now())

			got := readEvents(t, srv2.URL, url.Values{
				"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "cursor": {cursor},
			})
			for _, e := range got {
				if e.Kind == replicate.KindGap {
					t.Fatalf("ack %d covers the oldest retained record %d, but the subscriber was told it lost records: %v",
						oldest+tc.offset, oldest, kinds(got))
				}
			}
			if got[0].Kind != replicate.KindReset || got[0].Reason != replicate.ResetEpoch {
				t.Errorf("expected an epoch reset with no gap; got %v / %q", got[0].Kind, got[0].Reason)
			}
		})
	}
}

// TestAnAckBelowTheOldestRecordStillWarns is the other side of the boundary: the ack is
// known and does NOT reach the oldest retained record, so records between them are missing
// and the window has both ends.
func TestAnAckBelowTheOldestRecordStillWarns(t *testing.T) {
	dir := t.TempDir()
	const base = 1700000000

	cat, a := openArchiveAt(t, dir)
	srv := httptest.NewServer(Handler(cat, ServerOptions{AgeAttr: ageAttr, Heartbeat: time.Hour}))
	for i := range 5 {
		appendAt(t, a, i, base+int64(i))
	}
	evs := readEvents(t, srv.URL, url.Values{"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}})
	cursor := base64.StdEncoding.EncodeToString(evs[len(evs)-1].Cursor)
	srv.Close()
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	crashTheSource(t, dir)

	cat2, a2 := openArchiveAt(t, dir)
	defer cat2.Close()
	reg := &MemRegistry{LeaseTTL: time.Hour}
	srv2 := httptest.NewServer(Handler(cat2, ServerOptions{
		Registry: reg, AgeAttr: ageAttr, Heartbeat: time.Hour,
	}))
	defer srv2.Close()
	for i := 5; i < 400; i++ {
		appendAt(t, a2, i, base+int64(i))
	}
	if dropped, err := a2.Rotate(0); err != nil {
		t.Fatal(err)
	} else if dropped == 0 {
		t.Fatal("rotation dropped nothing, so there is no loss to report")
	}
	oldest := firstUpsertTS(t, srv2.URL)
	acked := oldest - 5000
	reg.Ack("transfers", "c1", acked, time.Now())

	got := readEvents(t, srv2.URL, url.Values{
		"table": {"transfers"}, "subscriber": {"c1"}, "src": {"s"}, "cursor": {cursor},
	})
	if got[0].Kind != replicate.KindGap {
		t.Fatalf("ack %d is below the oldest retained record %d but no Gap was sent: %v", acked, oldest, kinds(got))
	}
	if got[0].FromMillis != acked || got[0].ToMillis != oldest {
		t.Errorf("gap window = %d..%d, want %d..%d", got[0].FromMillis, got[0].ToMillis, acked, oldest)
	}
}
