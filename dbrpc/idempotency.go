package dbrpc

import (
	"strings"
	"sync"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// Durable idempotency (opt-in, via Tx.CommitIdempotent). A committing transaction
// that carries an idempotency key also writes a small marker record, keyed in the
// db's reserved "system" keyspace, inside the SAME transaction as the data. Because
// the commit is atomic, the marker lands iff the data lands -- so it survives a
// server restart, and a replay of the same unit of work is detected (the marker is
// already present, or the replayed marker conflicts on its key under OCC) and
// short-circuits to success instead of applying the writes twice.
//
// The marker is a system-keyed record, so it is invisible to every normal
// scan/query/iterate (see the collections reserved-prefix support) and is
// retrievable only by explicit key lookup and the TTL reaper below. A workload that
// never calls CommitIdempotent writes no markers and pays nothing.

const (
	// idemMarkerPrefix namespaces idempotency markers within the reserved system
	// keyspace, so the reaper can enumerate exactly them.
	idemMarkerPrefix = "idem:"
	// idemMarkerAttr holds a marker's creation time (unix seconds); the reaper uses
	// it to expire markers older than its configured retention.
	idemMarkerAttr = "IdemAt"
)

// idemMarkerKey is the reserved system key under which the marker for idemKey lives.
func idemMarkerKey(idemKey string) string {
	return db.SystemKey(idemMarkerPrefix + idemKey)
}

// reapIdemMarkers deletes idempotency markers older than maxAge from every table,
// returning how many were removed. Markers are system-keyed, so a normal
// DeleteWhere never touches them; the reaper enumerates them via ForEachSystemAd and
// destroys the expired ones by key.
func (s *Server) reapIdemMarkers(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge).Unix()
	reaped := 0
	for _, table := range s.cat.Tables() {
		d, ok := s.cat.Table(table)
		if !ok {
			continue
		}
		var expired []string
		d.ForEachSystemAd(func(key string, ad *classad.ClassAd) bool {
			if !strings.HasPrefix(key, idemMarkerKey("")) {
				return true // a system key that is not one of our markers
			}
			at, ok := ad.EvaluateAttrInt(idemMarkerAttr)
			if !ok || at <= cutoff {
				expired = append(expired, key)
			}
			return true
		})
		for _, k := range expired {
			tx := d.Begin()
			tx.DestroyClassAd(k)
			if err := tx.Commit(); err == nil {
				reaped++
			}
		}
	}
	return reaped
}

// StartIdemReaper runs reapIdemMarkers every interval, deleting idempotency markers
// older than maxAge, until the returned stop is called. maxAge should exceed the
// longest window over which a client might replay a unit of work (its retry budget),
// so a marker is never reaped while a late replay could still arrive. Servers that
// never see CommitIdempotent need not start it (there is nothing to reap).
func (s *Server) StartIdemReaper(interval, maxAge time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.reapIdemMarkers(maxAge)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
