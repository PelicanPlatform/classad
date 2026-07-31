package changefeed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/db/replicate"
)

// PullConfig configures a subscription to one source feed.
type PullConfig struct {
	BaseURL    string   // source root, e.g. "https://source:9200"
	Table      string   // source table/archive
	Subscriber string   // stable durable-subscription id (identifies this sink to the source)
	Src        string   // label recorded with events (defaults to Subscriber); passed as ?src=
	Constraint string   // optional server-side filter
	Project    []string // optional attribute projection

	Token      string        // bearer token
	AckEvery   time.Duration // ack + commit cadence; default 5s
	Backoff    time.Duration // reconnect backoff cap; default 30s
	HTTPClient *http.Client
}

// Pull subscribes to the source feed over SSE and applies each change to sink, resuming from
// sink.Cursor() on every (re)connect and periodically ACKing the max record timestamp it has
// durably persisted (so the source can GC). It reconnects with backoff until ctx is cancelled; it
// returns nil on cancellation.
func Pull(ctx context.Context, cfg PullConfig, sink replicate.Sink) error {
	if cfg.AckEvery <= 0 {
		cfg.AckEvery = 5 * time.Second
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{} // no timeout: SSE is long-lived
	}
	if cfg.Src == "" {
		cfg.Src = cfg.Subscriber
	}
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		err := pullOnce(ctx, cfg, sink)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > cfg.Backoff {
				backoff = cfg.Backoff
			}
			continue
		}
		backoff = 500 * time.Millisecond
	}
	return nil
}

// pullOnce runs one SSE connection until it ends (server close, error, or ctx cancel).
func pullOnce(parent context.Context, cfg PullConfig, sink replicate.Sink) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel() // stops the ack goroutine + the request when this connection ends

	resp, err := cfg.subscribe(ctx, sink.Cursor())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var maxTS atomic.Int64
	var lastCursor atomic.Pointer[[]byte]
	commitAndAck := func() {
		if cp := lastCursor.Load(); cp != nil {
			_ = sink.Commit(*cp)
		}
		postAck(ctx, cfg, maxTS.Load())
	}

	go func() {
		t := time.NewTicker(cfg.AckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				commitAndAck()
			}
		}
	}()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // comment / heartbeat / blank
		}
		var ev Event
		if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev); err != nil {
			return fmt.Errorf("changefeed: decoding event: %w", err)
		}
		ch, err := ToChange(ev)
		if err != nil {
			continue // skip a single bad ad rather than resync the whole stream
		}
		if err := sink.Apply(ch); err != nil {
			return err
		}
		if ev.TS > maxTS.Load() {
			maxTS.Store(ev.TS)
		}
		if len(ev.Cursor) > 0 {
			c := append([]byte(nil), ev.Cursor...)
			lastCursor.Store(&c)
		}
		if ev.Kind == replicate.KindSynced {
			commitAndAck()
		}
	}
	return sc.Err()
}

func (cfg PullConfig) subscribe(ctx context.Context, cursor []byte) (*http.Response, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/") + PathSubscribe)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("table", cfg.Table)
	q.Set("subscriber", cfg.Subscriber)
	q.Set("src", cfg.Src)
	if cfg.Constraint != "" {
		q.Set("constraint", cfg.Constraint)
	}
	if len(cfg.Project) > 0 {
		q.Set("project", strings.Join(cfg.Project, ","))
	}
	if len(cursor) > 0 {
		q.Set("cursor", base64.StdEncoding.EncodeToString(cursor))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("changefeed: subscribe %s: %s", cfg.Table, resp.Status)
	}
	return resp, nil
}

// postAck sends the ack watermark; failures are ignored (the next tick retries; the resume cursor,
// not the ack, is what guarantees correctness).
func postAck(ctx context.Context, cfg PullConfig, ackMillis int64) {
	body, _ := json.Marshal(ackRequest{Subscriber: cfg.Subscriber, Table: cfg.Table, AckMillis: ackMillis})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+PathAck, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
