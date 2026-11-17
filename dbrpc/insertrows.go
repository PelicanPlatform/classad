package dbrpc

import (
	"context"
	"errors"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// The server-side row copy: INSERT ... SELECT with no GROUP BY.
//
// It sits beside the roll-up in insertselect.go and shares its key rule -- the target's
// declared PRIMARY KEY decides each written row's storage key, so a re-run replaces rather
// than duplicates -- and almost nothing else. See opInsertRows for why the two are separate
// opcodes rather than one with a flag.

// InsertRows copies rows from source into target, all on the server, and reports how many
// were written.
//
// attrs and names are the projection: each source attribute and the target attribute it is
// written under, which is how a copy renames a column. Both empty means SELECT *, copying
// every attribute of each ad under its own name.
//
// target must declare a primary key, and a projection must include it; the server refuses
// otherwise rather than inventing keys. The copy is bounded -- a statement matching more
// rows than the server will hold is refused, not truncated -- so a caller moving a large
// table narrows it with a constraint and copies in parts, which compose because each row's
// key comes from the row.
func (c *Client) InsertRows(ctx context.Context, target, source, constraint string,
	attrs, names []string) (int, error) {

	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		b := putStr(putStr(putStr(req(id, opInsertRows), target), source), constraint)
		b = putI32(b, int32(len(attrs)))
		for i, a := range attrs {
			b = putStr(putStr(b, a), names[i])
		}
		return b
	})
	if err != nil {
		return 0, err
	}
	if status == stBadReq {
		// No fallback, for the reason InsertSelect has none: a client-side copy would write
		// through the REPL's generated keys rather than the table's declared one, so it
		// would stop being idempotent and nothing would say so.
		return 0, ErrInsertRowsUnsupported
	}
	if status != stOK {
		return 0, statusErr(status, body)
	}
	n := body.u64()
	if body.err != nil {
		return 0, errShort
	}
	return int(n), nil
}

// ErrInsertRowsUnsupported is returned against a server too old to implement the row-copy
// opcode. There is deliberately no fallback: see InsertRows.
var ErrInsertRowsUnsupported = errors.New(
	"dbrpc: the server does not implement a server-side INSERT ... SELECT row copy")

// --- server ---

// serveInsertRows answers opInsertRows: scan source, copy the rows into target.
func (s *Server) serveInsertRows(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	target := r.str()
	source := r.str()
	constraint := r.str()
	nCols := int(r.i32())
	if r.err != nil || nCols < 0 || nCols > 1024 {
		write(respBad(reqID))
		return
	}
	var attrs, names []string
	if nCols > 0 {
		attrs = make([]string, nCols)
		names = make([]string, nCols)
		for i := 0; i < nCols; i++ {
			attrs[i] = r.str()
			names[i] = r.str()
		}
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}

	// The TARGET must be a mutable table, as for the roll-up: an archive is append-only and
	// a view is derived from its base.
	targetRef := s.resolveTable(target)
	if targetRef.kind != kindTable {
		write(respErr(reqID, "INSERT ... SELECT target "+target+
			" is not a writable table (an archive and a view cannot be inserted into)"))
		return
	}
	// Refused rather than reasoned about. The scan reads committed state and the writes are
	// buffered until commit, so this would in fact run -- and it would mean "append a second
	// copy of every row under a different key", which is not what anyone writing it meant.
	if strings.EqualFold(target, source) {
		write(respErr(reqID, "INSERT ... SELECT cannot read and write the same table ("+target+")"))
		return
	}

	rows, srcSealed, err := s.scanForCopy(reqID, source, constraint, includePrivate, write)
	if err != nil {
		return // scanForCopy already answered
	}
	// Only a privileged read can move a sealed value, because an unprivileged one never
	// sees it; see db.CheckSealedCopy.
	if includePrivate {
		if sealErr := db.CheckSealedCopy(srcSealed, targetRef.db.EncryptedAttrNames(), copyColMap(attrs, names)); sealErr != nil {
			write(respErr(reqID, sealErr.Error()))
			return
		}
	}

	n, err := targetRef.db.CopyRowsInto(rows, db.CopySpec{Attrs: attrs, Names: names},
		db.CopyLimits{}, func() bool { return ctx.Err() != nil })
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	write(putU64(resp(reqID, stOK), uint64(n)))
}

// copyColMap maps each source attribute to the target attribute it lands in, lowercased on
// the source side because ClassAd attribute names are case-insensitive. It returns nil for
// SELECT *, where every attribute keeps its own name.
func copyColMap(attrs, names []string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for i, a := range attrs {
		m[strings.ToLower(a)] = names[i]
	}
	return m
}

// scanForCopy opens the source scan, whichever kind of table names it, and reports the
// source's sealed attributes alongside so the caller can check what the copy would move.
func (s *Server) scanForCopy(reqID uint64, source, constraint string, includePrivate bool,
	write func([]byte)) (iter.Seq[*classad.ClassAd], []string, error) {

	switch ref := s.resolveTable(source); ref.kind {
	case kindTable, kindView:
		var seq iter.Seq[*classad.ClassAd]
		var err error
		if includePrivate {
			seq, err = ref.db.Query(constraint)
		} else {
			seq, err = ref.db.QueryRedacted(constraint)
		}
		if err != nil {
			write(respErr(reqID, err.Error()))
			return nil, nil, errAnswered
		}
		sealed := ref.db.EncryptedAttrNames()
		// A continuous aggregate that has sealed holds half its rows in the backing table
		// and half in an archive, and the scan above reads only the first. The other
		// one-source ops DECLINE this case and send the client to a path that reads both;
		// a copy has no such path to be sent to, and declining would reach the caller as
		// "your server is too old". So the two halves are unioned here.
		//
		// This is the case where getting it wrong would be quietest: the copy would
		// succeed, report a plausible count, and leave out every row old enough to have
		// been sealed.
		if ref.seals() && ref.view != nil {
			arch, ok := ref.view.SealedArchive()
			if ok {
				var tail iter.Seq[*classad.ClassAd]
				if includePrivate {
					tail, err = arch.Query(constraint)
				} else {
					tail, err = arch.QueryLimitRedacted(constraint, 0)
				}
				if err != nil {
					write(respErr(reqID, err.Error()))
					return nil, nil, errAnswered
				}
				seq = concatAds(seq, tail)
				sealed = append(sealed, arch.EncryptedAttrNames()...)
			}
		}
		return seq, sealed, nil
	case kindArchive:
		var seq iter.Seq[*classad.ClassAd]
		var err error
		if includePrivate {
			seq, err = ref.arch.Query(constraint)
		} else {
			seq, err = ref.arch.QueryLimitRedacted(constraint, 0)
		}
		if err != nil {
			write(respErr(reqID, err.Error()))
			return nil, nil, errAnswered
		}
		return seq, ref.arch.EncryptedAttrNames(), nil
	default:
		write(respErr(reqID, "no such table: "+source))
		return nil, nil, errAnswered
	}
}

// concatAds streams a, then b. The two halves of a sealed view do not overlap -- sealing
// moves a bucket out of the backing table -- and if that ever stopped holding, the copy's
// duplicate-key check is what would catch it, loudly.
func concatAds(a, b iter.Seq[*classad.ClassAd]) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		for ad := range a {
			if !yield(ad) {
				return
			}
		}
		for ad := range b {
			if !yield(ad) {
				return
			}
		}
	}
}
