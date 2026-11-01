package changefeed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// A subscriber not entitled to sealed values must learn nothing about them from the feed: not from the
// ads it is sent and not from which events its constraint lets through. These tests hold its stream to
// the stream a store without the sealed values produces for the same writes, and a privileged
// subscriber's stream to the plaintext store's.

type feedStore int

const (
	feedSealed    feedStore = iota // Secret sealed at rest
	feedPlaintext                  // the same writes, Secret in the clear
	feedHidden                     // the same writes with Secret removed
)

// feedStep is one write: a Put of pub (plus Secret when secret is set), or a Delete.
type feedStep struct {
	key, pub, secret string
	del              bool
}

var feedSteps = []feedStep{
	{key: "a", pub: `Cpus=1`, secret: "match"},
	{key: "b", pub: `Cpus=2`, secret: "nope"},
	{key: "c", pub: `Cpus=3`},
	{key: "a", pub: `Cpus=1`, secret: "nope"},  // sealed-only change: leaves the sealed filter
	{key: "b", pub: `Cpus=2`, secret: "match"}, // sealed-only change: enters it
	{key: "c", pub: `Cpus=3`, secret: "match"}, // gains a sealed attribute
	{key: "a", pub: `Cpus=4`, secret: "nope"},
	{key: "b", del: true},
	{key: "d", pub: `Cpus=5`, secret: "match"},
}

// feedSentinel is written last; a stream is read until it arrives.
var feedSentinel = feedStep{key: "zz", pub: `Done=true`}

var feedConstraints = []string{
	"",
	`Secret == "match" || Done =?= true`,
	`Secret =?= undefined`,
	`Cpus >= 2 || Secret == "nope" || Done =?= true`,
}

func feedCatalog(t *testing.T, store feedStore) (*db.Catalog, *db.DB) {
	t.Helper()
	cfg := db.CatalogConfig{Dir: t.TempDir()}
	if store == feedSealed {
		cfg.PoolKeys = []db.KEK{{ID: "POOL", Material: []byte("pool-key-material-for-the-change-feed-test")}}
		cfg.EncryptedAttrs = []string{"Secret"}
	}
	cat, err := db.OpenCatalogConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	tbl, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	return cat, tbl
}

func feedApply(t *testing.T, tbl *db.DB, store feedStore, steps []feedStep) {
	t.Helper()
	for _, s := range steps {
		if s.del {
			if _, err := tbl.Delete(s.key); err != nil {
				t.Fatal(err)
			}
			continue
		}
		src := s.pub
		if s.secret != "" && store != feedHidden {
			src += fmt.Sprintf("; Secret = %q", s.secret)
		}
		ad, err := classad.Parse("[" + src + "]")
		if err != nil {
			t.Fatal(err)
		}
		if err := tbl.Put(s.key, ad); err != nil {
			t.Fatal(err)
		}
	}
}

// feedSubscribe opens a subscription and returns its status and, on 200, its events (closed when the
// stream ends or ctx is done).
func feedSubscribe(t *testing.T, ctx context.Context, base, table, constraint string) (int, <-chan Event) {
	t.Helper()
	q := url.Values{"table": {table}, "subscriber": {"s1"}, "src": {"src1"}, "cursor": {"@begin"}}
	if constraint != "" {
		q.Set("constraint", constraint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+PathSubscribe+"?"+q.Encode(), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	out := make(chan Event, 256)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var ev Event
			if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev); err != nil {
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return resp.StatusCode, out
}

// feedRender renders an event without the fields that differ between two stores by construction (the
// cursor carries a per-store epoch). dropUndefined leaves out undefined-valued attributes, so a sealed
// value a keyless decode turned undefined compares equal to its absence in the hidden store.
func feedRender(t *testing.T, ev Event, dropUndefined bool) string {
	t.Helper()
	ad := ""
	if len(ev.Ad) > 0 {
		ch, err := ToChange(ev)
		if err != nil {
			t.Fatal(err)
		}
		out := classad.New()
		for _, name := range ch.Ad.GetAttributes() {
			e, _ := ch.Ad.Lookup(name)
			if _, undef := e.AST().(*ast.UndefinedLiteral); undef && dropUndefined {
				continue
			}
			out.InsertExpr(name, e)
		}
		ad = out.StringWithPrivate()
	}
	return fmt.Sprintf("%s src=%s ver=%d key=%s ts=%d %s", ev.Kind, ev.Src, ev.Ver, ev.Key, ev.TS, ad)
}

// feedRun writes feedSteps and the sentinel to a fresh store, subscribes from the beginning under opts,
// and returns the stream through the sentinel's event.
func feedRun(t *testing.T, store feedStore, opts ServerOptions, constraint string, dropUndefined bool) []string {
	t.Helper()
	cat, tbl := feedCatalog(t, store)
	feedApply(t, tbl, store, feedSteps)
	opts.Heartbeat = time.Hour // keep heartbeats out of the stream
	srv := httptest.NewServer(Handler(cat, opts))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, events := feedSubscribe(t, ctx, srv.URL, "jobs", constraint)
	if status != http.StatusOK {
		t.Fatalf("subscribe %q: status %d", constraint, status)
	}
	feedApply(t, tbl, store, []feedStep{feedSentinel})
	var got []string
	for ev := range events {
		got = append(got, feedRender(t, ev, dropUndefined))
		if ev.Key == feedSentinel.key {
			cancel()
			return got
		}
	}
	t.Fatalf("stream ended before the sentinel; got %d events", len(got))
	return nil
}

func privileged(r *http.Request) (Grant, bool) {
	return Grant{Src: r.URL.Query().Get("src"), Subscriber: r.URL.Query().Get("subscriber"), Privileged: true}, true
}

func unprivileged(r *http.Request) (Grant, bool) {
	return Grant{Src: r.URL.Query().Get("src"), Subscriber: r.URL.Query().Get("subscriber")}, true
}

func legacyAuth(r *http.Request) (string, string, bool) {
	return r.URL.Query().Get("src"), r.URL.Query().Get("subscriber"), r.Header.Get("Authorization") == "Bearer test"
}

func sameStream(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: streams differ\n got (%d):\n  %s\nwant (%d):\n  %s", label,
			len(got), strings.Join(got, "\n  "), len(want), strings.Join(want, "\n  "))
	}
}

// TestFeedKeylessDifferential: an unprivileged subscriber -- admitted by Authorize without Privileged, by
// the deprecated allow/deny Auth, or by no authorizer at all -- receives exactly the stream the store
// without the sealed values produces, constraint and all. A privileged one receives the plaintext store's.
func TestFeedKeylessDifferential(t *testing.T) {
	unpriv := map[string]ServerOptions{
		"Authorize":  {Authorize: unprivileged},
		"legacyAuth": {Auth: legacyAuth},
		"noAuth":     {},
	}
	for _, c := range feedConstraints {
		t.Run(fmt.Sprintf("%q", c), func(t *testing.T) {
			hidden := feedRun(t, feedHidden, ServerOptions{Authorize: privileged}, c, true)
			for name, opts := range unpriv {
				sameStream(t, name+": unprivileged subscriber of the sealed store vs the hidden store",
					feedRun(t, feedSealed, opts, c, true), hidden)
			}
			sameStream(t, "privileged subscriber of the sealed store vs the plaintext store",
				feedRun(t, feedSealed, ServerOptions{Authorize: privileged}, c, false),
				feedRun(t, feedPlaintext, ServerOptions{Authorize: privileged}, c, false))
		})
	}
}

// TestFeedRefusesPrivateConstraintUnprivileged: a constraint that reads a private attribute is refused
// for an unprivileged subscriber, as it is for an unprivileged read -- whether or not the store seals
// it (an archive stores private attributes in the clear). A dynamic reference is refused too, since its
// target cannot be checked. A privileged subscriber may use either.
func TestFeedRefusesPrivateConstraintUnprivileged(t *testing.T) {
	cat, arch := mustCatalogArchive(t, "history")
	if err := arch.AppendOld(`GlobalJobId = "ap40#1.0"; ClusterId = 1; ClaimId = "<secret-capability>"`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{`ClaimId == "<secret-capability>"`, `eval(strcat("Claim", "Id")) =!= undefined`} {
		for _, tc := range []struct {
			name string
			opts ServerOptions
			want int
		}{
			{"privileged", ServerOptions{Authorize: privileged}, http.StatusOK},
			{"unprivileged", ServerOptions{Authorize: unprivileged}, http.StatusForbidden},
			{"legacyAuth", ServerOptions{Auth: legacyAuth}, http.StatusForbidden},
			{"noAuth", ServerOptions{}, http.StatusForbidden},
		} {
			t.Run(tc.name+"/"+c, func(t *testing.T) {
				tc.opts.Heartbeat = time.Hour
				srv := httptest.NewServer(Handler(cat, tc.opts))
				defer srv.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				status, events := feedSubscribe(t, ctx, srv.URL, "history", c)
				if status != tc.want {
					t.Fatalf("status %d, want %d", status, tc.want)
				}
				if status != http.StatusOK {
					return
				}
				// The premise: the privileged constraint does match the record.
				for ev := range events {
					if ev.Kind == replicate.KindUpsert && len(ev.Ad) > 0 {
						return
					}
				}
				t.Fatal("privileged subscription delivered no record")
			})
		}
	}
}

// TestFeedUnprivilegedNeverSeesSealedValue: the plain leak, end to end. An unprivileged subscriber's
// events never carry a sealed value's plaintext, though its attribute is not a private name that the
// JSON rendering would drop on its own.
func TestFeedUnprivilegedNeverSeesSealedValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       ServerOptions
		wantSecret bool
	}{
		{"privileged", ServerOptions{Authorize: privileged}, true},
		{"unprivileged", ServerOptions{Authorize: unprivileged}, false},
		{"legacyAuth", ServerOptions{Auth: legacyAuth}, false},
		{"noAuth", ServerOptions{}, false},
	} {
		got := strings.Join(feedRun(t, feedSealed, tc.opts, "", false), "\n")
		if saw := strings.Contains(got, `"match"`); saw != tc.wantSecret {
			t.Errorf("%s: sealed plaintext in stream = %v, want %v:\n%s", tc.name, saw, tc.wantSecret, got)
		}
	}
}
