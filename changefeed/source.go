package changefeed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
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

// Authorizer authenticates a request and returns the source label and subscriber id it is allowed
// to act as. ok=false rejects (401). A nil Authorizer allows all, taking src/subscriber from query.
type Authorizer func(r *http.Request) (src, subscriber string, ok bool)

// ServerOptions configure the feed Handler.
type ServerOptions struct {
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
}

func (s *server) resolve(table string) (watchable, bool) {
	if a, ok := s.cat.ArchiveTable(table); ok {
		return a, true
	}
	if d, ok := s.cat.Table(table); ok {
		return d, true
	}
	return nil, false
}

func (s *server) authOf(r *http.Request) (src, sub string, ok bool) {
	if s.opts.Auth != nil {
		return s.opts.Auth(r)
	}
	return r.URL.Query().Get("src"), r.URL.Query().Get("subscriber"), true
}

func (s *server) subscribe(w http.ResponseWriter, r *http.Request) {
	src, sub, ok := s.authOf(r)
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
	tbl, ok := s.resolve(table)
	if !ok {
		http.Error(w, "no such table: "+table, http.StatusNotFound)
		return
	}
	cursor, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}
	var matcher *vm.Query
	if c := strings.TrimSpace(q.Get("constraint")); c != "" {
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
	seq, err := tbl.Watch(ctx, cursor)
	if err != nil {
		s.writeSSE(w, flusher, Event{Kind: replicate.KindGap}) // best-effort signal; connection then ends
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			s.opts.Registry.Renew(table, sub, time.Now())
			if _, err := w.Write([]byte(": hb\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case we, ok := <-events:
			if !ok {
				return // watch ended (ctx cancel)
			}
			ch, ok := replicate.ChangeFromWatch(we, src, ver)
			if !ok {
				continue // resync: not sink-visible
			}
			ver++
			if ch.Kind == replicate.KindUpsert && ch.Ad != nil {
				if matcher != nil && !matcher.Matches(ch.Ad) {
					continue
				}
				ch.TS = stampTS(ch.Ad, s.opts.AgeAttr)
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
				s.opts.Registry.Renew(table, sub, time.Now())
			}
		}
	}
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
	_, authSub, ok := s.authOf(r)
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
	if authSub != "" {
		sub = authSub // the authenticated identity wins over a client-claimed id
	}
	if sub == "" || req.Table == "" {
		http.Error(w, "subscriber and table required", http.StatusBadRequest)
		return
	}
	now := time.Now()
	s.opts.Registry.Ack(req.Table, sub, req.AckMillis, now)
	floor, held := s.opts.Registry.Floor(req.Table, now)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ackResponse{FloorMillis: floor, Held: held})
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
