package db

import (
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The column relay: a query's matching records as typed COLUMNS rather than as rows.
//
// Every read API above this one hands back a record -- a wire row, a projected ad, rendered text
// -- and leaves the consumer to find the values inside it. For a consumer that wants the values
// and nothing else, that is two conversions it pays for and then undoes: the record is assembled
// from the columns it was stored in, and the consumer takes it apart again. Measured over the
// attributes a table's derived schema carries, the round trip is about twenty times the cost of
// reading the columns directly.
//
// The shape follows QueryRawWire's deliberately -- same gate, same empty/true-constraint fast
// path, same error for a bad constraint, same refusal BEFORE the first row for a table that
// cannot serve it. A caller should not have to know which read it is using to know what these
// return.
//
// THE BATCH IS BORROWED and valid only for the body of the loop that received it. Its slices are
// refilled for the next batch and its strings may point into the block they were read from. A
// consumer that keeps anything past its iteration must copy it. That is the price of not
// assembling a record, and it is why this is a sequence of batches rather than of rows: the
// borrow has to end somewhere a caller can see.

// ErrColumnsUnsupported reports that a table cannot serve the column relay for this query --
// it has no columnar accelerator, or the constraint is one the columns cannot evaluate.
// Callers fall back to a row scan, which every table can serve.
//
// Returned before any batch, always. A column scan that yielded nothing would be
// indistinguishable from a query that matched nothing, which is the failure this exists to
// prevent rather than one it is allowed to cause.
var ErrColumnsUnsupported = errors.New("classad-db: table cannot serve a column scan for this query")

// QueryColumns yields the values of attrs for the records matching constraint, in batches of
// typed columns (see collections.ScanColumns). An empty or "true" constraint reads every record.
// redact withholds values this reader is not entitled to, as the row reads do.
//
// attrs is what the caller wants back, in the order it wants them: every batch carries exactly
// these columns, in this order, including for a record that has none of them -- which reports
// undefined rather than a short row, so a consumer can bind its output once.
func (db *DB) QueryColumns(constraint string, attrs []string, redact bool) (iter.Seq[*collections.Batch], error) {
	return db.QueryColumnsStats(constraint, attrs, redact, nil)
}

// QueryColumnsStats is QueryColumns that also fills stats (may be nil) with the per-scan work
// breakdown -- including how many cells came from the block's columns and how many had to be read
// from the records themselves.
//
// That split is worth a caller's attention rather than a footnote. A projection naming an
// attribute the table's schema does not carry is served correctly and reads every one of its rows
// the slow way, and nothing else in the result says so.
func (db *DB) QueryColumnsStats(constraint string, attrs []string, redact bool,
	stats *collections.ColumnScanStats) (iter.Seq[*collections.Batch], error) {
	q, err := columnConstraint(constraint)
	if err != nil {
		return nil, err
	}
	if !db.c.ColumnsServable(q, attrs) {
		return nil, ErrColumnsUnsupported
	}
	return gated(db, columnSeq(db.c.ScanColumns, db.c.ScanColumnsRedacted, q, attrs, redact, stats)), nil
}

// QueryColumns yields the values of attrs for the records matching constraint, in batches of
// typed columns, newest first (see DB.QueryColumns).
func (t *ArchiveTable) QueryColumns(constraint string, attrs []string, redact bool) (iter.Seq[*collections.Batch], error) {
	return t.QueryColumnsStats(constraint, attrs, redact, nil)
}

// QueryColumnsStats is QueryColumns that also fills stats (may be nil). See DB.QueryColumnsStats.
func (t *ArchiveTable) QueryColumnsStats(constraint string, attrs []string, redact bool,
	stats *collections.ColumnScanStats) (iter.Seq[*collections.Batch], error) {
	q, err := columnConstraint(constraint)
	if err != nil {
		return nil, err
	}
	if !t.a.ColumnsServable(q, attrs) {
		return nil, ErrColumnsUnsupported
	}
	return gatedArch(t, columnSeq(t.a.ScanColumns, t.a.ScanColumnsRedacted, q, attrs, redact, stats)), nil
}

// columnConstraint parses a constraint for a column scan. An empty or "true" constraint becomes
// the match-all query rather than a nil one, because the column scan evaluates a query and has no
// separate no-WHERE path to take -- a predicate that is true for every record is already the
// cheapest thing it can run.
func columnConstraint(constraint string) (*vm.Query, error) {
	s := strings.TrimSpace(constraint)
	if s == "" {
		s = "true"
	}
	q, err := vm.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("classad-db: bad constraint %q: %w", constraint, err)
	}
	return q, nil
}

// columnSeq adapts a scan to a sequence. The scan is callback-driven and the callback's "keep
// going" is exactly a yield's, so the loop body IS the callback and the borrow ends where the
// caller can see it end.
//
// scan and redacted are passed in rather than switched on inside, so a table type that gains one
// and not the other fails to compile instead of silently serving the entitled read to everyone.
func columnSeq(scan, redacted func(*vm.Query, []string, func(*collections.Batch) bool) (collections.ColumnScanStats, bool),
	q *vm.Query, attrs []string, redact bool, stats *collections.ColumnScanStats) iter.Seq[*collections.Batch] {
	return func(yield func(*collections.Batch) bool) {
		run := scan
		if redact {
			run = redacted
		}
		got, _ := run(q, attrs, yield)
		if stats != nil {
			*stats = got
		}
	}
}
