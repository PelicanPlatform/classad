// Package replicate is the transport-neutral core for replicating a db table/archive's change
// stream into another store: an in-memory Change (a db.WatchEvent enriched with replication
// metadata) and an idempotent Sink that applies Changes into a *db.DB or *db.ArchiveTable. It
// depends only on classad + db (no HTTP, no CEDAR), so every transport builds on it: the HTTP/SSE
// feed (classad/changefeed) and a dbrpc/CEDAR replicator both convert their wire form to a Change
// and feed the same Sink.
package replicate

import (
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// Kind classifies a change. It mirrors db.WatchKind 1:1, plus an advisory Gap.
type Kind string

const (
	KindUpsert Kind = "upsert" // Key was added/updated; Ad holds the new value.
	KindDelete Kind = "delete" // Key was removed; Ad is nil.
	KindReset  Kind = "reset"  // discard derived state for Src; a fresh snapshot follows.
	KindSynced Kind = "synced" // caught up to head; Cursor is a safe resume/ack point.
	KindGap    Kind = "gap"    // advisory: records were lost to source retention (see From/To).
)

// Change is one change as the sink sees it, in memory. Ad is set only on an upsert.
type Change struct {
	Kind   Kind
	Src    string           // source identity (for fan-in stamping / dedup)
	Ver    uint64           // strictly monotonic per-source version (idempotency key)
	Key    string           // source storage key
	Ad     *classad.ClassAd // upsert only
	Cursor []byte           // opaque resume token (the db.Watch cursor); client-side resume only

	// TS is the record's retention timestamp (unix millis), source-stamped from the archive's age
	// attribute. It is the COMPARABLE ack watermark: a sink acks the max TS it has durably
	// persisted, and the source's GC floor is min(ack TS) over live subscribers -- which maps onto
	// the archive's time-based retention. 0 when the source has no age attribute (GC gating off).
	TS int64

	// For KindGap only: the estimated lost time window (unix millis).
	FromMillis, ToMillis int64
}

// ChangeFromWatch maps a db.WatchEvent to a Change, stamping src and a caller-assigned monotonic
// version. ok is false for a WatchResync (the transport loop handles reconnect, not the sink).
func ChangeFromWatch(we db.WatchEvent, src string, ver uint64) (Change, bool) {
	c := Change{Src: src, Ver: ver, Key: we.Key, Ad: we.Ad, Cursor: we.Cursor}
	switch we.Kind {
	case db.WatchUpsert:
		c.Kind = KindUpsert
	case db.WatchDelete:
		c.Kind = KindDelete
	case db.WatchReset:
		c.Kind = KindReset
	case db.WatchSynced:
		c.Kind = KindSynced
	default: // db.WatchResync: not a sink-visible change
		return Change{}, false
	}
	return c, true
}
