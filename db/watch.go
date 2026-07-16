package db

import (
	"context"
	"iter"
	"sync"
	"syscall"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// WatchKind classifies a watch event.
type WatchKind uint8

const (
	// WatchUpsert: key was added or updated; Ad holds its new value.
	WatchUpsert WatchKind = iota
	// WatchDelete: key was removed; Ad is nil.
	WatchDelete
	// WatchReset: discard prior state and rebuild from the upserts that follow (the
	// initial full replay, or a re-sync after the cursor fell out of retention). Key
	// and Ad are empty.
	WatchReset
)

// WatchEvent is one change to the log. Cursor resumes a watch just after this event
// (survives reconnect / restart, so no change is missed).
type WatchEvent struct {
	Kind   WatchKind
	Key    string
	Ad     *classad.ClassAd // nil for a delete
	Cursor []byte
}

// Watch streams changes committed after the given cursor (nil = from now). Cancel via
// ctx. Events arrive in commit order per key; see collections.Watch for the delivery
// and coalescing guarantees.
func (db *DB) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	seq, err := db.c.Watch(ctx, cursor)
	if err != nil {
		return nil, err
	}
	return func(yield func(WatchEvent) bool) {
		for ev := range seq {
			we := WatchEvent{Key: string(ev.Key), Ad: ev.Ad, Cursor: ev.Cursor}
			switch ev.Kind {
			case collections.WatchDelete:
				we.Kind = WatchDelete
			case collections.WatchReset:
				we.Kind = WatchReset
			}
			if !yield(we) {
				return
			}
		}
	}, nil
}

// Watcher is a watch whose readiness is signalled on a file descriptor -- for the C
// (DaemonCore) side, which registers the fd in its poll loop and, on wakeup, drains
// events with Next. Go writes a single wakeup byte when the queue goes non-empty
// (coalesced: the reader drains fully per wakeup). The fd is a pipe or eventfd the
// caller created and owns; Watcher never closes it.
type Watcher struct {
	cancel context.CancelFunc
	fd     int

	mu     sync.Mutex
	queue  []WatchEvent
	ready  bool // an unconsumed wakeup byte is outstanding
	closed bool
}

// NewWatcher starts watching db, signalling notifyFD when events are queued. cursor
// nil starts from now.
func NewWatcher(db *DB, notifyFD int, cursor []byte) (*Watcher, error) {
	ctx, cancel := context.WithCancel(context.Background())
	seq, err := db.Watch(ctx, cursor)
	if err != nil {
		cancel()
		return nil, err
	}
	w := &Watcher{cancel: cancel, fd: notifyFD}
	go func() {
		for ev := range seq { // ends when Stop cancels ctx
			w.enqueue(ev)
		}
	}()
	return w, nil
}

func (w *Watcher) enqueue(ev WatchEvent) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.queue = append(w.queue, ev)
	signal := !w.ready
	w.ready = true
	w.mu.Unlock()
	if signal {
		// One byte is enough to wake a level/edge-triggered poll; the reader drains the
		// whole queue and clears ready via Next.
		_, _ = syscall.Write(w.fd, []byte{1})
	}
}

// Next dequeues one event without blocking. ok is false when the queue is empty; the
// caller (having been woken via the fd) drains until ok is false.
func (w *Watcher) Next() (WatchEvent, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		w.ready = false // consumed the wakeup; a fresh enqueue will signal again
		return WatchEvent{}, false
	}
	ev := w.queue[0]
	w.queue = w.queue[1:]
	return ev, true
}

// Stop ends the watch. The notify fd is not closed (the caller owns it).
func (w *Watcher) Stop() {
	w.mu.Lock()
	w.closed = true
	w.queue = nil
	w.mu.Unlock()
	w.cancel()
}
