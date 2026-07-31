package changefeed

import (
	"sync"
	"time"
)

// Registry persists per-subscriber ACK watermarks and leases and computes the GC floor. The ack
// watermark is a COMPARABLE record timestamp (unix millis, source-stamped as Event.TS), so the
// floor -- the min ack over live subscribers -- maps directly onto an archive's time-based
// retention: the source may reclaim records older than the floor. A subscriber whose lease expires
// (silent past LeaseTTL) is evicted so one dead sink cannot pin retention forever; on return it
// resumes and, if its cursor was reclaimed, receives a reset+gap.
type Registry interface {
	// Ack records that subscriber has durably persisted every record up to ackMillis on table.
	Ack(table, subscriber string, ackMillis int64, now time.Time)
	// Renew marks a subscriber live (an active subscribe stream) without moving its ack.
	Renew(table, subscriber string, now time.Time)
	// Floor returns the min ack over live subscribers of table and their count. With no live
	// subscribers, held is false (no retention hold from the feed).
	Floor(table string, now time.Time) (ackMillis int64, held bool)
	// Evict drops subscribers whose lease expired as of now; returns how many were dropped.
	Evict(now time.Time) int
	// Subscribers lists live subscribers of a table (for observability).
	Subscribers(table string, now time.Time) []SubStatus
}

// SubStatus is one subscriber's observable state.
type SubStatus struct {
	Subscriber string
	AckMillis  int64
	LastSeen   time.Time
}

// MemRegistry is an in-memory Registry: fine for a single-node source. LeaseTTL bounds how long a
// silent subscriber holds the floor (default defaultLeaseTTL). Safe for concurrent use.
type MemRegistry struct {
	LeaseTTL time.Duration
	mu       sync.Mutex
	tables   map[string]map[string]*subEntry
}

type subEntry struct {
	ack  int64
	seen time.Time
}

const defaultLeaseTTL = time.Hour

func (r *MemRegistry) ttl() time.Duration {
	if r.LeaseTTL > 0 {
		return r.LeaseTTL
	}
	return defaultLeaseTTL
}

func (r *MemRegistry) entry(table, sub string, now time.Time) *subEntry {
	if r.tables == nil {
		r.tables = map[string]map[string]*subEntry{}
	}
	subs := r.tables[table]
	if subs == nil {
		subs = map[string]*subEntry{}
		r.tables[table] = subs
	}
	e := subs[sub]
	if e == nil {
		e = &subEntry{seen: now}
		subs[sub] = e
	}
	return e
}

func (r *MemRegistry) Ack(table, sub string, ackMillis int64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(table, sub, now)
	if ackMillis > e.ack {
		e.ack = ackMillis
	}
	e.seen = now
}

func (r *MemRegistry) Renew(table, sub string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entry(table, sub, now).seen = now
}

func (r *MemRegistry) Floor(table string, now time.Time) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-r.ttl())
	var floor int64
	held := false
	for _, e := range r.tables[table] {
		if e.seen.Before(cutoff) {
			continue // expired lease: does not hold the floor
		}
		if !held || e.ack < floor {
			floor, held = e.ack, true
		}
	}
	return floor, held
}

func (r *MemRegistry) Evict(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-r.ttl())
	dropped := 0
	for table, subs := range r.tables {
		for sub, e := range subs {
			if e.seen.Before(cutoff) {
				delete(subs, sub)
				dropped++
			}
		}
		if len(subs) == 0 {
			delete(r.tables, table)
		}
	}
	return dropped
}

func (r *MemRegistry) Subscribers(table string, now time.Time) []SubStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-r.ttl())
	var out []SubStatus
	for sub, e := range r.tables[table] {
		if e.seen.Before(cutoff) {
			continue
		}
		out = append(out, SubStatus{Subscriber: sub, AckMillis: e.ack, LastSeen: e.seen})
	}
	return out
}
