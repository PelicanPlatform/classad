package collections

import "sync"

// closeGate accounts for readers that are inside the collection across more than one
// segment pin, so a close cannot unmap memory under them.
//
// The per-segment pin (segment.pin) already covers a reader that is WALKING a segment: a
// compaction or a Close that retires one defers the munmap to the last unpin. What it does
// not cover is the reader BETWEEN pins. A scan pins one shard at a time -- snapshot ...
// releaseWindows, shard by shard -- so after shard i it holds nothing at all, and a close
// landing in that gap unmapped shards i+1.. while the scan was still going to read them.
// The next record read then faulted on an address inside a slice whose bounds were still
// valid: a Go runtime fault, not a panic, so no recover anywhere could contain it and the
// process died with every other client's connection along with it.
//
// The gate is therefore held for the WHOLE scan rather than per shard. A close with readers
// in flight marks the collection closed (no new reader is admitted) and hands its teardown
// to the last reader on the way out, rather than blocking: the holder is an iterator the
// gate cannot see the end of -- a consumer may abandon it -- and a close that waited would
// be a daemon-wide stall in exactly the case the accounting exists to make safe.
type closeGate struct {
	mu       sync.Mutex
	readers  int
	closed   bool
	teardown func() // set only when closed with readers still in flight
}

// acquire admits a reader, or reports false when the collection is closed (or closing) and
// its memory is already spoken for. A false answer means "there is nothing here to read":
// the caller yields no rows rather than touching a mapping that may already be gone.
func (g *closeGate) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.readers++
	return true
}

// release ends a reader's use of the collection, running a teardown a close deferred when
// this is the last one out. It must balance every acquire that returned true, including on
// an early return, an error, or a consumer that stops iterating part way.
func (g *closeGate) release() {
	g.mu.Lock()
	g.readers--
	var run func()
	if g.readers == 0 && g.teardown != nil {
		run, g.teardown = g.teardown, nil
	}
	g.mu.Unlock()
	if run != nil {
		run()
	}
}

// beginClose marks the collection closed and decides who runs td. It returns false when the
// caller must run it (the common case: nothing is reading), and true when td has been handed
// to the last reader still inside, which will run it on its way out.
//
// A second close is a no-op: it returns true without replacing a teardown already queued, so
// the collection is never torn down twice.
func (g *closeGate) beginClose(td func()) (deferred bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return true
	}
	g.closed = true
	if g.readers == 0 {
		return false
	}
	g.teardown = td
	return true
}

// closeNow marks the collection closed for a teardown the caller runs itself, in flight
// readers or not. It reports false when a teardown has already run or is already queued
// behind a reader, so the caller does nothing.
//
// This is the ordinary Close, which must not defer: it is what makes the data durable, and a
// caller that gets a nil error from it has been told the write-out happened. Handing that to
// whichever reader leaves last would move the file writes to an arbitrary later moment --
// after the caller deleted the directory, in one case. A reader is left safe instead by the
// teardown emptying the shards: the windows it already pinned stay mapped until it unpins
// them, and anything it has not reached yet simply is not there any more.
func (g *closeGate) closeNow() (run bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.closed = true
	return true
}

// isClosed reports whether the collection has been closed, whether or not its teardown has
// run yet.
func (g *closeGate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// AcquireRead registers the caller as an in-flight reader of the collection and reports
// whether it may read. False means the collection is closed: a dropped table, say, whose
// segments are already unmapped or about to be. Every true answer MUST be balanced by
// ReleaseRead, or the collection's mappings are never released.
//
// It exists because a close is not serialized against reads by anything else: a reader may
// be holding an iterator, or be between the per-shard pins a scan takes, and in either case
// the mapping it is about to read has no other thing keeping it alive.
func (c *Collection) AcquireRead() bool { return c.gate.acquire() }

// ReleaseRead balances an AcquireRead that returned true. The last reader out of a closed
// collection performs the deferred teardown, so this must be called with no collection or
// shard lock held.
func (c *Collection) ReleaseRead() { c.gate.release() }

// Closed reports whether the collection has been closed.
func (c *Collection) Closed() bool { return c.gate.isClosed() }
