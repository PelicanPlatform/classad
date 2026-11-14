package changefeed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/replicate"
)

// HTTP routes (versioned).
const (
	PathSubscribe = "/changefeed/v1/subscribe"
	PathAck       = "/changefeed/v1/ack"
)

// Grant is what an authenticated request may act as.
type Grant struct {
	Src        string // the source label stamped on the subscriber's events
	Subscriber string // the subscriber id its acks and lease are recorded under

	// Privileged entitles the subscriber to sealed values. Its events are decoded, and its
	// constraint evaluated, with the table's key, and its constraint may reference private
	// attributes.
	//
	// Without it the subscriber is served what an unprivileged read of the table is: each event is
	// decoded with no key, so a sealed value is undefined in the ad it receives and in the
	// constraint that chose it, and a constraint referencing a private attribute is refused. Which
	// events reach it is then as independent of sealed values as their content: a filter cannot
	// match on a value the subscriber may not see, and a chained parent whose sealed attributes
	// alone changed does not fan out to it. A write that changes only sealed attributes of a
	// record still delivers that record, identical to the upsert the subscriber last received,
	// which is what any write that changes nothing it can see delivers.
	Privileged bool
}

// GrantAuthorizer authenticates a request and returns what it may act as. ok=false rejects (401).
type GrantAuthorizer func(r *http.Request) (g Grant, ok bool)

// Authorizer authenticates a request and returns the source label and subscriber id it is allowed
// to act as. ok=false rejects (401).
//
// Deprecated: an Authorizer can only allow or deny, so a subscriber it admits is unprivileged (see
// Grant.Privileged). Use GrantAuthorizer through ServerOptions.Authorize to admit a privileged one.
type Authorizer func(r *http.Request) (src, subscriber string, ok bool)

// ServerOptions configure the feed Handler.
type ServerOptions struct {
	// Authorize authenticates each request and decides its entitlement. It takes precedence over
	// Auth. With neither set every request is allowed, unprivileged, taking src and subscriber from
	// the query.
	Authorize GrantAuthorizer
	// Auth is the allow/deny authorizer; every subscriber it admits is unprivileged.
	//
	// Deprecated: use Authorize.
	Auth      Authorizer
	Registry  Registry      // records acks + leases, computes the GC floor; MemRegistry if nil
	Heartbeat time.Duration // SSE keep-alive comment cadence; default 15s
	AgeAttr   string        // the record attribute (unix seconds) stamped as Event.TS for GC; "" => no TS
}

// Handler serves the change feed: SSE GET PathSubscribe and POST PathAck. It reads from cat's
// tables/archives via db.Watch; it never writes. Mount it on any HTTP listener (token-gate it in
// front). Transport-neutral: no CEDAR.
func Handler(cat *db.Catalog, opts ServerOptions) http.Handler {
	if opts.Registry == nil {
		opts.Registry = &MemRegistry{}
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 15 * time.Second
	}
	s := &server{cat: cat, opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc(PathSubscribe, s.subscribe)
	mux.HandleFunc(PathAck, s.ack)
	return mux
}

type server struct {
	cat  *db.Catalog
	opts ServerOptions
}

// watchable is satisfied by both *db.DB (mutable) and *db.ArchiveTable.
type watchable interface {
	Watch(ctx context.Context, cursor []byte) (iter.Seq[db.WatchEvent], error)
	WatchRedacted(ctx context.Context, cursor []byte) (iter.Seq[db.WatchEvent], error)
}

// resolve finds the table and reports whether it is append-only (an archive). The two are
// watched identically but replay very differently: an archive's replay is an event log a
// consumer appends, a mutable table's is a state snapshot it rebuilds from. Only the first
// can have its replay bounded without leaving the consumer with a half-built view.
func (s *server) resolve(table string) (w watchable, appendOnly, ok bool) {
	if a, ok := s.cat.ArchiveTable(table); ok {
		return a, true, true
	}
	if d, ok := s.cat.Table(table); ok {
		return d, false, true
	}
	return nil, false, false
}

func (s *server) authOf(r *http.Request) (Grant, bool) {
	if s.opts.Authorize != nil {
		return s.opts.Authorize(r)
	}
	if s.opts.Auth != nil {
		src, sub, ok := s.opts.Auth(r)
		return Grant{Src: src, Subscriber: sub}, ok
	}
	return Grant{Src: r.URL.Query().Get("src"), Subscriber: r.URL.Query().Get("subscriber")}, true
}

func (s *server) subscribe(w http.ResponseWriter, r *http.Request) {
	g, ok := s.authOf(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	table := q.Get("table")
	if table == "" {
		http.Error(w, "table required", http.StatusBadRequest)
		return
	}
	tbl, appendOnly, ok := s.resolve(table)
	if !ok {
		http.Error(w, "no such table: "+table, http.StatusNotFound)
		return
	}
	cursor, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}
	since, err := parseSince(q.Get("since"))
	if err != nil {
		http.Error(w, "bad since: "+err.Error(), http.StatusBadRequest)
		return
	}
	if since > 0 {
		// Refused rather than ignored, both times: a subscriber that asked to bound its
		// replay and was quietly given the whole thing is the failure this parameter exists
		// to prevent.
		if !appendOnly {
			http.Error(w, "since is only supported on an append-only (archive) table: "+
				"dropping records from a mutable table's replay would leave a half-built state view", http.StatusBadRequest)
			return
		}
		if s.opts.AgeAttr == "" {
			http.Error(w, "since requires the feed to be configured with an age attribute", http.StatusBadRequest)
			return
		}
	}
	var matcher *vm.Query
	if c := strings.TrimSpace(q.Get("constraint")); c != "" {
		// As for an unprivileged read: evaluating a constraint has no notion of privacy, so one
		// that reads a private attribute would answer for its value through which events
		// arrive. Refuse it, and one naming attributes dynamically, whose targets cannot be
		// checked.
		if !g.Privileged {
			attr, dynamic := db.PrivateConstraintRef(c)
			if attr != "" {
				http.Error(w, "cannot reference private attribute "+attr+" in a constraint", http.StatusForbidden)
				return
			}
			if dynamic {
				http.Error(w, "cannot use a dynamic attribute reference in a constraint", http.StatusForbidden)
				return
			}
		}
		matcher, err = vm.Parse(c)
		if err != nil {
			http.Error(w, "bad constraint: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	var project []string
	if p := strings.TrimSpace(q.Get("project")); p != "" {
		project = splitCSV(p)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	// The watch, not the stream, carries the entitlement: an unprivileged subscriber's events are
	// decoded with no key, so the constraint below and every ad it is sent see a sealed value as
	// undefined, and the key is never opened on its behalf.
	watch := tbl.WatchRedacted
	if g.Privileged {
		watch = tbl.Watch
	}
	seq, err := watch(ctx, cursor)
	if err != nil {
		_ = s.writeSSE(w, flusher, Event{Kind: replicate.KindGap}) //nolint:errcheck // best-effort signal; the connection ends either way
		return
	}

	// Range the (blocking) Watch in a goroutine so the handler can interleave heartbeats.
	events := make(chan db.WatchEvent, 64)
	go func() {
		defer close(events)
		for we := range seq {
			select {
			case events <- we:
			case <-ctx.Done():
				return
			}
		}
	}()

	hb := time.NewTicker(s.opts.Heartbeat)
	defer hb.Stop()
	var ver uint64
	// A retention Reset is held until the first replayed record reveals where the retained
	// history now starts, so the Gap announcing the loss can name both ends of the window.
	// held is empty once they have been written.
	var held []Event
	catchingUp, lostHistory := true, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			s.opts.Registry.Renew(table, g.Subscriber, time.Now())
			if _, err := w.Write([]byte(": hb\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case we, ok := <-events:
			if !ok {
				return // watch ended (ctx cancel)
			}
			ch, ok := replicate.ChangeFromWatch(we, g.Src, ver)
			if !ok {
				continue // resync: not sink-visible
			}
			ver++
			if ch.Kind == replicate.KindReset && ch.Reason == replicate.ResetRetention {
				// Records between this subscriber's cursor and the replay's start are GONE,
				// not merely re-sent. Hold the Reset: the Gap that says so goes out in front
				// of it, and its far end is not known until the next record arrives.
				lostHistory = true
				// The Gap goes out first, so it takes the version the Reset was assigned and
				// the Reset takes the next one. Versions stay strictly monotonic in the order
				// the subscriber receives them, which is what a sink dedupes on.
				gap := Event{Kind: replicate.KindGap, Src: g.Src, Ver: ch.Ver, FromMillis: s.ackOf(table, g.Subscriber)}
				ch.Ver = ver
				ver++
				resetEv, err := ToEvent(ch)
				if err != nil {
					return
				}
				held = []Event{gap, resetEv}
				continue
			}
			var ts int64
			if ch.Kind == replicate.KindUpsert && ch.Ad != nil {
				// Before the constraint: the window's far end is where the SOURCE's history
				// now starts, which a subscriber's filter has no say in.
				ts = stampTS(ch.Ad, s.opts.AgeAttr)
			}
			if len(held) > 0 {
				// On the FIRST event after the Reset, whatever it is: the Reset has to
				// precede the replay it introduces, so nothing may overtake it while the
				// window's far end is still unknown. ts is 0 when that event is not a
				// timestamped record, which is the honest answer for an end we cannot see.
				held[0].ToMillis = ts
				for _, ev := range held {
					if err := s.writeSSE(w, flusher, ev); err != nil {
						return
					}
				}
				held = nil
			}
			if ch.Kind == replicate.KindUpsert && ch.Ad != nil {
				// since is the subscriber's own floor on a replay it did not ask to receive
				// in full. It applies only to a catch-up that LOST history: a cursor that
				// resumed cleanly is delivering records the subscriber has not seen and
				// expects, and silently dropping those would skip them for good.
				if since > 0 && catchingUp && lostHistory && ts > 0 && ts < since {
					continue
				}
				if matcher != nil && !matcher.Matches(ch.Ad) {
					continue
				}
				ch.TS = ts
				if len(project) > 0 {
					ch.Ad = projectAd(ch.Ad, project)
				}
			}
			ev, err := ToEvent(ch)
			if err != nil {
				continue // skip an unmarshalable ad rather than kill the stream
			}
			if err := s.writeSSE(w, flusher, ev); err != nil {
				return
			}
			if ch.Kind == replicate.KindSynced {
				catchingUp = false
				s.opts.Registry.Renew(table, g.Subscriber, time.Now())
			}
		}
	}
}

// ackOf returns what the source last recorded this subscriber as having durably persisted,
// in unix millis, or 0 if it has no record of it. 0 is the honest answer rather than a
// guess: the default Registry is in memory, so a source that restarted has forgotten every
// ack, and a subscriber that tracks its own applied timestamp knows this end of the window
// better than the source does.
func (s *server) ackOf(table, subscriber string) int64 {
	if subscriber == "" {
		return 0
	}
	for _, st := range s.opts.Registry.Subscribers(table, time.Now()) {
		if st.Subscriber == subscriber {
			return st.AckMillis
		}
	}
	return 0
}

// parseSince parses the subscribe handler's since= parameter: unix milliseconds, or empty
// for none.
func parseSince(v string) (int64, error) {
	if v = strings.TrimSpace(v); v == "" {
		return 0, nil
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not unix milliseconds: %q", v)
	}
	if ms < 0 {
		return 0, fmt.Errorf("negative: %d", ms)
	}
	return ms, nil
}

func (s *server) writeSSE(w http.ResponseWriter, f http.Flusher, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// ackRequest is the POST /ack body.
type ackRequest struct {
	Subscriber string `json:"subscriber"`
	Table      string `json:"table"`
	AckMillis  int64  `json:"ackMillis"`
}

type ackResponse struct {
	FloorMillis int64 `json:"floorMillis"`
	Held        bool  `json:"held"`
}

func (s *server) ack(w http.ResponseWriter, r *http.Request) {
	g, ok := s.authOf(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sub := req.Subscriber
	if g.Subscriber != "" {
		sub = g.Subscriber // the authenticated identity wins over a client-claimed id
	}
	if sub == "" || req.Table == "" {
		http.Error(w, "subscriber and table required", http.StatusBadRequest)
		return
	}
	now := time.Now()
	s.opts.Registry.Ack(req.Table, sub, req.AckMillis, now)
	floor, held := s.opts.Registry.Floor(req.Table, now)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ackResponse{FloorMillis: floor, Held: held}) //nolint:errcheck // ack already recorded; a client that drops the reply sees the floor on its next one
}

// stampTS reads the record's age attribute (unix seconds) and returns it as unix millis, or 0.
func stampTS(ad *classad.ClassAd, ageAttr string) int64 {
	if ageAttr == "" {
		return 0
	}
	if v, ok := ad.EvaluateAttrInt(ageAttr); ok && v > 0 {
		return v * 1000
	}
	return 0
}

// projectAd returns a copy of ad holding only the named attributes (case-insensitive) that exist.
func projectAd(ad *classad.ClassAd, attrs []string) *classad.ClassAd {
	out := classad.New()
	for _, name := range attrs {
		if e, ok := ad.Lookup(name); ok {
			out.InsertExpr(name, e)
		}
	}
	return out
}

func decodeCursor(s string) ([]byte, error) {
	switch s {
	case "", "@now":
		return nil, nil
	case "@begin":
		return []byte{}, nil // empty non-nil: some watchers treat "" as full replay
	}
	return base64.StdEncoding.DecodeString(s)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
