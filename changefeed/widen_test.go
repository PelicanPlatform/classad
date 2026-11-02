package changefeed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// widenFeedRun is feedRun over a store that seals nothing explicitly when the first half of feedSteps
// is written, and Secret from then on: its policy is widened at runtime, so the records written before
// hold Secret in the clear.
func widenFeedRun(t *testing.T, opts ServerOptions, constraint string, dropUndefined bool) []string {
	t.Helper()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir(),
		PoolKeys: []db.KEK{{ID: "POOL", Material: []byte("pool-key-material-for-the-widened-feed")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	tbl, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	half := len(feedSteps) / 2
	feedApply(t, tbl, feedSealed, feedSteps[:half])
	if err := tbl.SetEncryptedAttrs([]string{"Secret"}); err != nil {
		t.Fatal(err)
	}
	feedApply(t, tbl, feedSealed, feedSteps[half:])
	opts.Heartbeat = time.Hour
	srv := httptest.NewServer(Handler(cat, opts))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, events := feedSubscribe(t, ctx, srv.URL, "jobs", constraint)
	if status != http.StatusOK {
		t.Fatalf("subscribe %q: status %d", constraint, status)
	}
	// The sentinel is written once catch-up has ended. Written as soon as the subscription is
	// accepted, it could land before the watch takes its snapshot and arrive in catch-up, wherever
	// its shard puts it, so the same writes would give two streams in two orders.
	var got []string
	for ev := range events {
		got = append(got, feedRender(t, ev, dropUndefined))
		if ev.Kind == replicate.KindSynced {
			feedApply(t, tbl, feedSealed, []feedStep{feedSentinel})
		}
		if ev.Key == feedSentinel.key {
			cancel()
			return got
		}
	}
	t.Fatalf("stream ended before the sentinel; got %d events", len(got))
	return nil
}

// TestFeedWidenedPolicyKeyless: an unprivileged subscriber of a store whose policy was widened to seal
// Secret after some of its records were written receives the hidden store's stream -- the records
// written before the widen included -- and a privileged one the plaintext store's.
func TestFeedWidenedPolicyKeyless(t *testing.T) {
	for _, c := range feedConstraints {
		t.Run(fmt.Sprintf("%q", c), func(t *testing.T) {
			sameStream(t, "unprivileged subscriber of the widened store vs the hidden store",
				widenFeedRun(t, ServerOptions{Authorize: unprivileged}, c, true),
				feedRun(t, feedHidden, ServerOptions{Authorize: privileged}, c, true))
			sameStream(t, "privileged subscriber of the widened store vs the plaintext store",
				widenFeedRun(t, ServerOptions{Authorize: privileged}, c, false),
				feedRun(t, feedPlaintext, ServerOptions{Authorize: privileged}, c, false))
		})
	}
}
