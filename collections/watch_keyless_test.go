package collections

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A watcher not entitled to sealed values must learn nothing about them: not from the ads it is sent,
// not from which events a filter lets through, and not from when a chained parent fans out to its
// children. These tests hold its stream to the stream a store without the sealed values produces for
// the same writes, and hold an entitled watcher's stream to the plaintext store's.

// kwStep is one write of the keyless-watch sequence: a Put of pub (plus Secret, sealed, when secret is
// set), or a Delete.
type kwStep struct {
	key    string
	pub    string
	secret string
	del    bool
}

// kwPre is written before the watch starts, so it arrives in catch-up.
var kwPre = []kwStep{
	{key: "1.-1", pub: `DAGManJobId=42; Owner="alice"`, secret: "p1"},
	{key: "1.0", pub: `ProcId=0; Cpus=1`, secret: "c0"},
	{key: "1.1", pub: `ProcId=1; Cpus=2`, secret: "c11"},
	{key: "1.2", pub: `ProcId=2; Cpus=2`}, // inherits the parent's Secret
	{key: "2.-1", pub: `DAGManJobId=7; Owner="bob"`},
	{key: "2.0", pub: `ProcId=0; Cpus=3`, secret: "match"},
}

// kwLive is written once the watch is live. Each sealed-only write is one a watcher without the key
// must not be able to tell from a write that changed nothing.
var kwLive = []kwStep{
	{key: "1.-1", pub: `DAGManJobId=42; Owner="alice"`, secret: "p2"}, // sealed parent value changes
	{key: "1.0", pub: `ProcId=0; Cpus=1`, secret: "c1"},               // sealed child value changes
	{key: "1.-1", pub: `DAGManJobId=42; Owner="alice"`},               // parent loses its sealed attribute
	{key: "1.-1", pub: `DAGManJobId=42; Owner="alice"`, secret: "match"},
	{key: "2.-1", pub: `DAGManJobId=7; Owner="bob"`, secret: "x"},        // parent gains one
	{key: "1.-1", pub: `DAGManJobId=43; Owner="alice"`, secret: "match"}, // a public parent change
	{key: "3.0", pub: `ProcId=0; Cpus=4`, secret: "match"},
	{key: "1.1", del: true},
	{key: "2.0", pub: `ProcId=0; Cpus=5`, secret: "match"},
	{key: "2.-1", pub: `DAGManJobId=8; Owner="bob"`, secret: "x"},
}

// kwSentinel is written last; a stream is read until it arrives, which (one shard, commit order) is
// after everything written before it.
var kwSentinel = kwStep{key: "9.0", pub: `ProcId=0; Done=true`}

// kwMarker follows each live write of the differential, which waits for its event before the next
// write. A fan-out reads the children (and their parent) as they are when the watcher handles the
// parent's change, not as of that change, so without the wait a later write could land in an earlier
// fan-out and the two stores' streams would differ by timing alone.
func kwMarker(i int) kwStep {
	return kwStep{key: fmt.Sprintf("8.%d", i), pub: fmt.Sprintf("ProcId=%d; Done=true", i)}
}

// kwFilters each pass the sentinel; the rest of each reads a sealed attribute, a public one, or both.
var kwFilters = []string{
	"",
	`Secret == "match" || Done =?= true`,
	`Secret =?= undefined`,
	`Secret =!= undefined || Done =?= true`,
	`Cpus >= 2 || Done =?= true`,
	`DAGManJobId == 43 || Secret == "c1" || Done =?= true`,
}

type kwStore int

const (
	kwSealed    kwStore = iota // Secret sealed
	kwPlaintext                // the same writes, Secret stored in the clear
	kwHidden                   // the same writes with Secret removed: what a keyless watcher may see
)

func (s kwStep) ad(t *testing.T, store kwStore) *classad.ClassAd {
	t.Helper()
	src := s.pub
	if s.secret != "" && store != kwHidden {
		src += fmt.Sprintf("; Secret=%q", s.secret)
	}
	return mustAd(t, "["+src+"]")
}

func kwApply(t *testing.T, c *Collection, store kwStore, steps []kwStep) {
	t.Helper()
	for _, s := range steps {
		if s.del {
			c.Delete([]byte(s.key))
			continue
		}
		if err := c.Put([]byte(s.key), s.ad(t, store)); err != nil {
			t.Fatal(err)
		}
	}
}

func kwOpen(t *testing.T, store kwStore, coalesce time.Duration) *Collection {
	t.Helper()
	opts := Options{
		Shards: 1, Dir: t.TempDir(), SegmentSize: 1 << 16, WatchHistory: 1024, WatchCoalesce: coalesce,
		ParentKeyFor: jobParentKey, IsStructural: jobStructural,
	}
	if store == kwSealed {
		_, opts.DataKey = deriveDataKey(t)
		opts.EncryptedAttrs = []string{"Secret"}
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// kwRender renders an event's ad. dropUndefined leaves out every attribute whose value is undefined, so
// a sealed value a keyless decode turned undefined compares equal to the attribute's absence in the
// kwHidden store (the two evaluate alike).
func kwRender(ad *classad.ClassAd, dropUndefined bool) string {
	if ad == nil {
		return ""
	}
	if dropUndefined {
		out := classad.New()
		for _, name := range ad.GetAttributes() {
			e, ok := ad.Lookup(name)
			if !ok {
				continue
			}
			if _, undef := e.AST().(*ast.UndefinedLiteral); undef {
				continue
			}
			out.InsertExpr(name, e)
		}
		ad = out
	}
	return ad.StringWithPrivate()
}

// kwRun writes kwPre, watches c from the beginning (redacted or not, through filter), writes kwLive and
// the sentinel, and returns the stream rendered through the sentinel's event. layout prepares the
// records written so far before the watch starts.
func kwRun(t *testing.T, c *Collection, store kwStore, redact bool, filter string, layout func(*Collection)) []string {
	t.Helper()
	kwApply(t, c, store, kwPre)
	if layout != nil {
		layout(c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watch := c.Watch
	if redact {
		watch = c.WatchRedacted
	}
	seq, err := watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filter != "" {
		q, err := vm.Parse(filter)
		if err != nil {
			t.Fatal(err)
		}
		seq = WatchFilter(seq, q.Matches)
	}
	out := make(chan string, 256)
	synced := make(chan struct{})
	go func() {
		defer close(out)
		for ev := range seq {
			if ev.Kind == WatchSynced {
				close(synced)
			}
			out <- fmt.Sprintf("%d %s %s", ev.Kind, ev.Key, kwRender(ev.Ad, redact))
			if string(ev.Key) == kwSentinel.key {
				return
			}
		}
	}()
	select {
	case <-synced:
	case <-ctx.Done():
		t.Fatal("watch never synced")
	}
	var got []string
	for i, s := range kwLive {
		kwApply(t, c, store, []kwStep{s, kwMarker(i)})
		for line := range out {
			got = append(got, line)
			if strings.HasPrefix(line, fmt.Sprintf("%d %s ", WatchUpsert, kwMarker(i).key)) {
				break
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("stream did not reach marker %d; got %d events", i, len(got))
		}
	}
	kwApply(t, c, store, []kwStep{kwSentinel})
	for line := range out {
		got = append(got, line)
	}
	if ctx.Err() != nil {
		t.Fatalf("stream did not reach the sentinel; got %d events", len(got))
	}
	return got
}

func kwSame(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}
	t.Errorf("%s: streams differ\n got (%d):\n  %s\nwant (%d):\n  %s", label,
		len(got), strings.Join(got, "\n  "), len(want), strings.Join(want, "\n  "))
}

// kwLayouts are the record layouts the watch reads from: as written (inline), and with the pre-watch
// records interned, whose fan-out re-encodes each child out of its segment dictionary.
var kwLayouts = []struct {
	name   string
	layout func(*Collection)
}{
	{"inline", nil},
	{"interned", internAll},
}

// TestWatchKeylessDifferential: a watcher not entitled to sealed values receives exactly the stream a
// store holding none of them produces for the same writes -- through catch-up, live upserts and deletes,
// a filter on the sealed attribute, and chained parents whose sealed attributes change alone. An
// entitled watcher receives exactly the plaintext store's stream.
func TestWatchKeylessDifferential(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range kwLayouts {
		for _, f := range kwFilters {
			t.Run(fmt.Sprintf("%s/%q", lay.name, f), func(t *testing.T) {
				run := func(store kwStore, redact bool) []string {
					return kwRun(t, kwOpen(t, store, 0), store, redact, f, lay.layout)
				}
				kwSame(t, "keyless watch of the sealed store vs the hidden store",
					run(kwSealed, true), run(kwHidden, false))
				kwSame(t, "entitled watch of the sealed store vs the plaintext store",
					run(kwSealed, false), run(kwPlaintext, false))
			})
		}
	}
}

// TestWatchKeylessSealedParentNoFanout pins the fan-out decision on its own: a parent whose sealed
// attribute alone changes fans out to an entitled watcher (the premise) and to no keyless one, coalesced
// or not.
func TestWatchKeylessSealedParentNoFanout(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, coalesce := range []time.Duration{0, 20 * time.Millisecond} {
		for _, redact := range []bool{false, true} {
			t.Run(fmt.Sprintf("coalesce=%v/redact=%v", coalesce, redact), func(t *testing.T) {
				c := kwOpen(t, kwSealed, coalesce)
				kwApply(t, c, kwSealed, kwPre)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				watch := c.Watch
				if redact {
					watch = c.WatchRedacted
				}
				seq, err := watch(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				keys := make(chan string, 64)
				synced := make(chan struct{})
				go func() {
					defer close(keys)
					live := false
					for ev := range seq {
						if ev.Kind == WatchSynced {
							live = true
							close(synced)
							continue
						}
						if !live {
							continue // catch-up
						}
						keys <- string(ev.Key)
						if string(ev.Key) == kwSentinel.key {
							return
						}
					}
				}()
				<-synced
				kwApply(t, c, kwSealed, []kwStep{{key: "1.-1", pub: `DAGManJobId=42; Owner="alice"`, secret: "p2"}, kwSentinel})
				var got []string
				for k := range keys {
					got = append(got, k)
				}
				fannedOut := len(got) > 1
				if fannedOut == redact {
					t.Fatalf("redact=%v: events after a sealed-only parent change = %v; want fan-out only for the entitled watcher", redact, got)
				}
			})
		}
	}
}

// TestWatchKeylessHoldsNoKey: a keyless watch never opens a sealed value -- not to decode an event, not to
// decide a parent's fan-out, not to re-encode an interned child. Withholding the key, rather than
// opening it and dropping what it revealed, is the only form of the guarantee that survives code which
// forgets to drop something. An entitled watch over the same writes does open it (the premise).
func TestWatchKeylessHoldsNoKey(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range kwLayouts {
		for _, coalesce := range []time.Duration{0, 20 * time.Millisecond} {
			for _, redact := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/coalesce=%v/redact=%v", lay.name, coalesce, redact), func(t *testing.T) {
					c := kwOpen(t, kwSealed, coalesce)
					ctr := &countingSealer{inner: c.sealer}
					c.sealer = ctr
					layout := func(c *Collection) {
						if lay.layout != nil {
							lay.layout(c)
						}
						ctr.opens.Store(0) // count from the watch on: the writes and the layout pass are not the watcher's
					}
					kwRun(t, c, kwSealed, redact, "", layout)
					n := ctr.opens.Load()
					if redact && n != 0 {
						t.Fatalf("keyless watch opened %d sealed values", n)
					}
					if !redact && n == 0 {
						t.Fatal("entitled watch opened no sealed value; the keyless check proves nothing")
					}
				})
			}
		}
	}
}
