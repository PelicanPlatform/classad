package collections

import (
	"context"
	"encoding/binary"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
)

// Archive Watch is a log tail. The archive is append-only (no updates, no deletes),
// so a watcher's cursor is a durable log position (epoch, segment id, offset) rather
// than an MVCC sequence: replay every record after the position, then stream new
// appends. Segment ids are stable across restart (the catalog persists them), so a
// cursor resumes incrementally even across a reopen. A cursor older than what
// rotation still retains gets a WatchReset (a history gap: resume from the floor).
// Events are WatchUpsert (an appended ad; Key is nil), plus WatchSynced / WatchResync
// (see WatchEvent). See docs/WATCH.md.

// --- opaque positional cursor: {epoch, seg, off} ---

func encodeArchiveCursor(epoch uint64, seg, off uint32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], epoch)
	binary.LittleEndian.PutUint32(b[8:], seg)
	binary.LittleEndian.PutUint32(b[12:], off)
	return b
}

func decodeArchiveCursor(b []byte) (epoch uint64, seg, off uint32, ok bool) {
	if len(b) != 16 {
		return 0, 0, 0, false
	}
	return binary.LittleEndian.Uint64(b[0:]),
		binary.LittleEndian.Uint32(b[8:]),
		binary.LittleEndian.Uint32(b[12:]), true
}

// ensureWatchEpoch lazily loads (or creates) a stable per-archive identity persisted
// at <dir>/watch.epoch, so a cursor minted against a different archive is rejected.
func (a *Archive) ensureWatchEpoch() uint64 {
	a.watchEpochOnce.Do(func() {
		p := filepath.Join(a.dir, "watch.epoch")
		if b, err := os.ReadFile(p); err == nil && len(b) == 8 {
			a.watchEpoch = binary.LittleEndian.Uint64(b)
			return
		}
		e := randomEpoch()
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], e)
		_ = os.WriteFile(p, b[:], 0o644)
		a.watchEpoch = e
	})
	return a.watchEpoch
}

// --- live subscription hub (append-only variant) ---

// archiveWatchBuf is the per-watcher live buffer capacity.
const archiveWatchBuf = 1024

type archiveRawEvent struct {
	seg, off uint32
	ad       []byte // compressed stored bytes
	codec    Codec
}

type archiveWatcher struct {
	ch     chan archiveRawEvent
	lagged atomic.Bool
}

type archiveWatchHub struct {
	active   atomic.Bool
	mu       sync.Mutex
	watchers map[*archiveWatcher]struct{}
}

func newArchiveWatchHub() *archiveWatchHub {
	return &archiveWatchHub{watchers: map[*archiveWatcher]struct{}{}}
}

func (h *archiveWatchHub) register(buf int) *archiveWatcher {
	w := &archiveWatcher{ch: make(chan archiveRawEvent, buf)}
	h.mu.Lock()
	h.watchers[w] = struct{}{}
	h.active.Store(true)
	h.mu.Unlock()
	return w
}

func (h *archiveWatchHub) deregister(w *archiveWatcher) {
	h.mu.Lock()
	delete(h.watchers, w)
	if len(h.watchers) == 0 {
		h.active.Store(false)
	}
	h.mu.Unlock()
}

// publish fans a newly appended record out to every active watcher, non-blocking; a
// watcher whose buffer is full is marked lagged (told to resync) rather than stalling
// Append.
func (h *archiveWatchHub) publish(seg, off uint32, ad []byte, codec Codec) {
	if !h.active.Load() {
		return
	}
	h.mu.Lock()
	if len(h.watchers) == 0 {
		h.mu.Unlock()
		return
	}
	ev := archiveRawEvent{seg, off, ad, codec}
	for w := range h.watchers {
		select {
		case w.ch <- ev:
		default:
			w.lagged.Store(true)
		}
	}
	h.mu.Unlock()
}

// --- snapshot for catch-up ---

type archiveWin struct {
	id   uint32
	data []byte
	used int
	seg  *segment
}

// watchSnapshot pins every retained segment (oldest first) and reports the retention
// floor (oldest segment id) and the tail position (just past the last record). Pins
// keep segments alive against rotation for the duration of catch-up.
func (a *Archive) watchSnapshot() (wins []archiveWin, floor, tailSeg, tailOff uint32) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, as := range a.segs { // sealed, oldest first
		if as.seg.used == 0 {
			continue
		}
		as.seg.pin()
		wins = append(wins, archiveWin{as.seg.id, as.seg.data, as.seg.used, as.seg})
	}
	if a.active != nil && a.active.seg.used > 0 {
		a.active.seg.pin()
		wins = append(wins, archiveWin{a.active.seg.id, a.active.seg.data, a.active.seg.used, a.active.seg})
	}
	if len(wins) > 0 {
		floor = wins[0].id
		last := wins[len(wins)-1]
		tailSeg, tailOff = last.id, uint32(last.used)
	} else {
		floor, tailSeg, tailOff = a.nextID, a.nextID, 0
	}
	return wins, floor, tailSeg, tailOff
}

func releaseArchiveWins(wins []archiveWin) {
	for i := range wins {
		wins[i].seg.unpin()
	}
}

func (a *Archive) decodeStored(stored []byte, codec Codec) (*classad.ClassAd, error) {
	w, err := codec.Decompress(nil, stored)
	if err != nil {
		return nil, err
	}
	node, err := a.decodeWire(w)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(node), nil
}

// --- the Watch verb ---

// Watch replays every archived ad after cursor (nil ⇒ from the oldest retained), then
// streams new appends until ctx is cancelled or the consumer stops. A cursor from a
// different archive, or older than what rotation still retains, yields a WatchReset
// and resumes from the current floor. See WatchEvent and docs/WATCH.md.
func (a *Archive) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error) {
	return func(yield func(WatchEvent) bool) {
		epoch := a.ensureWatchEpoch()
		w := a.hub.register(archiveWatchBuf)
		defer a.hub.deregister(w)

		wins, floor, tailSeg, tailOff := a.watchSnapshot()
		defer releaseArchiveWins(wins)

		cEpoch, cSeg, cOff, ok := decodeArchiveCursor(cursor)
		full := !ok || cEpoch != epoch || cSeg < floor
		startSeg, startOff := cSeg, cOff
		if full {
			startSeg, startOff = floor, 0
			if !yield(WatchEvent{Kind: WatchReset}) {
				return
			}
		}

		// Catch-up: every record at position >= (startSeg, startOff), oldest first.
		for _, wn := range wins {
			if wn.id < startSeg {
				continue
			}
			off := 0
			if wn.id == startSeg {
				off = int(startOff)
			}
			for off < wn.used {
				o := uint32(off)
				total := recTotalLen(wn.data, o)
				if total == 0 {
					break
				}
				if ad, err := a.decodeStored(recAd(wn.data, o), a.codec); err == nil {
					if !yield(WatchEvent{Kind: WatchUpsert, Ad: ad}) {
						return
					}
				}
				off += int(total)
			}
		}
		if !yield(WatchEvent{Kind: WatchSynced, Cursor: encodeArchiveCursor(epoch, tailSeg, tailOff)}) {
			return
		}

		// Live: stream appends past the catch-up tail.
		for {
			if w.lagged.Load() {
				yield(WatchEvent{Kind: WatchResync})
				return
			}
			select {
			case <-ctx.Done():
				return
			case raw := <-w.ch:
				if raw.seg < tailSeg || (raw.seg == tailSeg && raw.off < tailOff) {
					continue // already covered by catch-up
				}
				ad, err := a.decodeStored(raw.ad, raw.codec)
				if err != nil {
					continue
				}
				next := raw.off + uint32(recordLen(0, len(raw.ad)))
				ev := WatchEvent{Kind: WatchUpsert, Ad: ad, Cursor: encodeArchiveCursor(epoch, raw.seg, next)}
				if !yield(ev) {
					return
				}
			}
		}
	}, nil
}
