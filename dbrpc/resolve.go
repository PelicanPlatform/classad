package dbrpc

import (
	"strconv"

	"github.com/PelicanPlatform/classad/db"
)

// viewOwner is the optional Catalog capability to reach a view itself, rather than only its
// backing. It is what lets a handler ask the view for a complete answer -- a top-K or an
// aggregate over both halves -- instead of assembling one from parts it would have to know
// about. Optional for the same reason viewSealer is: a Catalog with no views need not
// provide it, and one that does not leaves reads served from the backing alone, as before.
type viewOwner interface {
	View(name string) (*db.View, bool)
}

// tableKind is which of the catalog's three namespaces a name was found in.
//
// The catalog keeps mutable tables, materialized views and append-only archives in three
// maps but ONE name space: creating any of them refuses a name another kind already holds.
// So a name has at most one kind, and the lookup order below is a search order, not a
// precedence rule -- no name can be found twice.
type tableKind uint8

const (
	kindMissing tableKind = iota // the name is not in the catalog at all
	kindTable                    // a mutable table
	kindView                     // a materialized view, read through its in-memory backing
	kindArchive                  // an append-only archive (history) table
)

// tableRef is what a name resolved to. Exactly one of db / arch is set, per kind; both are
// nil for kindMissing.
//
// A view resolves to its backing *db.DB, which is where a read of a view has always found
// its materialized rows -- but for a continuous aggregate that is only half of them. Sealing
// moves a closed bucket out of the backing and into the view's archive, so its rows live in
// TWO places, and sealed is that second one (nil unless this is a view that seals).
//
// A handler that serves kindView and consults only ref.db is not stale, it is WRONG: it
// silently returns the unsealed tail and nothing older. For a five-minute bucket the seal
// cadence is 150 seconds, so that is almost the whole series. Serve both halves, or refuse
// the name -- do not answer from one.
type tableRef struct {
	kind   tableKind
	db     *db.DB
	arch   *db.ArchiveTable
	view   *db.View         // kindView only, when the catalog exposes it
	sealed *db.ArchiveTable // kindView only, when the view has sealed history
}

// seals reports whether this name has a sealed half that a read must also consult.
func (r tableRef) seals() bool { return r.sealed != nil }

// resolveTable looks a name up in all three namespaces.
//
// Every handler that accepts a table name resolves through this, so there is one answer to
// "what is this name" rather than one per call site. Which kinds a handler then SERVES is
// the handler's own decision and differs between them -- see the switches at each call
// site, and tableOr for the ops that serve a mutable table or a view only.
func (s *Server) resolveTable(name string) tableRef {
	if d, ok := s.cat.Table(name); ok {
		return tableRef{kind: kindTable, db: d}
	}
	if d, ok := s.cat.ViewBacking(name); ok {
		ref := tableRef{kind: kindView, db: d}
		if vo, ok := s.cat.(viewOwner); ok {
			if v, has := vo.View(name); has {
				ref.view = v
				if a, sealed := v.SealedArchive(); sealed {
					ref.sealed = a
				}
			}
		}
		return ref
	}
	if a, ok := s.cat.ArchiveTable(name); ok {
		return tableRef{kind: kindArchive, arch: a}
	}
	return tableRef{}
}

// archiveTablePrefix opens the message of every refusal whose reason is that the name is an
// append-only archive. It is a machine-recognisable prefix in the same way, and for the
// same reason, as readOnlyTablePrefix: the client maps it to ErrArchiveTable so a caller
// classifies with errors.Is rather than by matching prose.
//
// It is deliberately NOT "no such table: ". That message means the name is unknown, and a
// caller that reads it correctly concludes the table has to be created or the name is
// wrong. An archive answering it sent callers chasing a table that was there all along, so
// the two now say different things and keep saying them.
const archiveTablePrefix = "archive table "

// respArchiveTable is the refusal for an op that does not serve an archive. why states the
// reason in the archive's own terms -- what an archive is, or what to call instead -- so
// the message is actionable without reading the server.
func respArchiveTable(reqID uint64, table, why string) []byte {
	return respErr(reqID, archiveTablePrefix+strconv.Quote(table)+": "+why)
}

// archiveRefusal is why o does not serve an archive.
//
// Two different reasons live here. Some ops cannot apply to an append log at all -- there
// is no key directory, no superseded version, no live ad to rank, no secondary ordering --
// and those reasons are permanent. The rest (the raw-read and snapshot ops) have no archive
// implementation yet rather than no meaning; they say what reaches an archive today instead
// of claiming a reason that does not exist.
func archiveRefusal(o op) string {
	switch o {
	case opQuery:
		return "Query is not served from an archive; use ArchiveQuery, which answers newest-first with the limit pushed down"
	case opQueryRaw:
		return "QueryRaw is not served from an archive; use QueryRawProject, which does read one"
	case opQueryRawWire, opQueryRawWireOpts:
		return "QueryRawWire is not served from an archive; use QueryRawProject, which does read one"
	case opQueryKeys:
		return "an archive has no key directory, so its rows have no storage keys to return"
	case opQueryAsOf:
		return "an archive supersedes nothing, so there is no earlier version to read as of an instant"
	case opMatchSorted:
		return "matchmaking ranks live ads; an archive holds completed records"
	case opOrdered:
		return "a secondary ordering is maintained over a mutable set; an archive has none"
	case opSnapshot:
		return "Snapshot is not available for an archive"
	}
	return o.String() + " is not served from an archive"
}

// mergeSorted merges two already-sorted, disjoint name lists into one sorted list.
func mergeSorted(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
}
