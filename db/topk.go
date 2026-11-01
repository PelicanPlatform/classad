package db

import (
	"container/heap"
	"iter"
	"math"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/parser"
)

// topKMaxColumnar caps k for the columnar cutoff pass: beyond this "top k" is close to a full sort
// and the extra columnar scan would not pay for itself, so the plain row path is used instead.
const topKMaxColumnar = 1 << 16

// TopK returns the k rows matching constraint ordered by orderAttr -- the largest k when desc,
// the smallest k otherwise -- each projected to attrs, in sorted order. It is the server-side
// form of ORDER BY <orderAttr> {DESC|ASC} LIMIT k: only k rows are ever held or returned, not
// the whole match set, so a "newest few of a million" query does not ship a million rows to be
// sorted client-side.
//
// orderAttr must resolve to a number (ClusterId, QDate, CompletionDate, ...); a row whose order
// value is not numeric has no place in the ordering and is skipped. If orderAttr is not among
// attrs it is fetched for the comparison and trimmed from the returned rows.
func (t *ArchiveTable) TopK(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, error) {
	rows, err := t.topKRun(constraint, attrs, orderAttr, desc, k, nil)
	return rows, err
}

// TopKStats is TopK that also returns the cutoff scan's ScanStats, for EXPLAIN ANALYZE.
func (t *ArchiveTable) TopKStats(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, collections.ScanStats, error) {
	var stats collections.ScanStats
	rows, err := t.topKRun(constraint, attrs, orderAttr, desc, k, &stats)
	return rows, stats, err
}

// topKRun is the shared two-pass body: a columnar cutoff scan finds the k-th order value (filling
// stats when non-nil), then the projection runs with `orderAttr {>=|<=} cutoff` appended -- which
// the columns and zone maps narrow to ~k rows -- instead of projecting every matching row and
// heaping the lot. Falls back to the plain path (unaugmented) when the columnar cutoff is unavailable.
func (t *ArchiveTable) topKRun(constraint string, attrs []string, orderAttr string, desc bool, k int, stats *collections.ScanStats) ([][]classad.Value, error) {
	proj, orderIdx, added := withOrderAttr(attrs, orderAttr)
	c2, err := topKAugment(constraint, orderAttr, desc, k,
		func(q *vm.Query) (float64, int, bool) { return t.a.TopKOrderThreshold(q, orderAttr, desc, k, stats) })
	if err != nil {
		return nil, err
	}
	return topKResult(t.a.QueryProjectStats(c2, proj, nil), orderIdx, desc, k, added), nil
}

// TopK is ArchiveTable.TopK for a mutable table.
func (db *DB) TopK(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, error) {
	rows, err := db.topKRun(constraint, attrs, orderAttr, desc, k, nil, false)
	return rows, err
}

// TopKStats is DB.TopK that also returns the cutoff scan's ScanStats, for EXPLAIN ANALYZE.
func (db *DB) TopKStats(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, collections.ScanStats, error) {
	var stats collections.ScanStats
	rows, err := db.topKRun(constraint, attrs, orderAttr, desc, k, &stats, false)
	return rows, stats, err
}

// TopKRedacted is TopK for a caller NOT entitled to sealed values: the constraint is matched, the rows
// ordered and the projection read with no key, so a sealed value is undefined throughout -- it neither
// comes back nor decides which rows do or in what order.
func (db *DB) TopKRedacted(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, error) {
	return db.topKRun(constraint, attrs, orderAttr, desc, k, nil, true)
}

// TopKStatsRedacted is TopKStats with TopKRedacted's entitlement.
func (db *DB) TopKStatsRedacted(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, collections.ScanStats, error) {
	var stats collections.ScanStats
	rows, err := db.topKRun(constraint, attrs, orderAttr, desc, k, &stats, true)
	return rows, stats, err
}

// topKRun serves both entitlements. The columnar cutoff takes no key either way: it reads only columns,
// which never hold a sealed value, and declines on any record whose order or predicate value it cannot
// read as a literal -- a sealed one included -- so a cutoff it does return is the same for both readers.
// The projection pass decides the rows, so it is the one that has to be keyless.
func (db *DB) topKRun(constraint string, attrs []string, orderAttr string, desc bool, k int, stats *collections.ScanStats, redact bool) ([][]classad.Value, error) {
	proj, orderIdx, added := withOrderAttr(attrs, orderAttr)
	c2, err := topKAugment(constraint, orderAttr, desc, k,
		func(q *vm.Query) (float64, int, bool) { return db.c.TopKOrderThreshold(q, orderAttr, desc, k, stats) })
	if err != nil {
		return nil, err
	}
	project := db.c.QueryProject
	if redact {
		project = db.c.QueryProjectRedacted
	}
	return topKResult(project(c2, proj), orderIdx, desc, k, added), nil
}

// topKAugment returns the query the projection pass should use: the original with
// `orderAttr {>=|<=} cutoff` appended when a columnar cutoff scan (threshold) yields a usable one
// (available, and more than k matching rows so the cutoff is the true k-th value). Otherwise it
// returns the original unchanged -- the plain full-scan path -- so behavior is never worse than
// before. A blank constraint is treated as `true`.
//
// The cutoff is appended to the compiled query (vm.Query.And) rather than to the text, so a
// CurrentTime in the constraint means the same instant in both passes. Recompiling the text would
// pin a later time, and a record that left the window in between would leave fewer than k rows
// above a cutoff computed with it.
func topKAugment(constraint, orderAttr string, desc bool, k int, threshold func(*vm.Query) (float64, int, bool)) (*vm.Query, error) {
	base := constraint
	if strings.TrimSpace(base) == "" {
		base = "true"
	}
	q, err := vm.Parse(base)
	if err != nil {
		return nil, err
	}
	if k <= 0 || k > topKMaxColumnar {
		return q, nil
	}
	// A constraint that filters nothing is asked as no constraint at all. The cutoff scan serves a
	// conjunction of numeric comparisons, and a bare `true` is not one -- it has no comparison to be
	// exact about -- so an unfiltered ORDER BY ... LIMIT k fell to projecting every record.
	tq := q
	if IsMatchAll(base) {
		tq = nil
	}
	cut, seen, ok := threshold(tq)
	if !ok || seen <= k {
		return q, nil // columnar cutoff unavailable, or <=k rows: nothing to narrow
	}
	op := ">="
	if !desc {
		op = "<="
	}
	bound, err := parser.ParseExpr(orderAttr + " " + op + " " + fmtNum(cut))
	if err != nil {
		return nil, err
	}
	return q.And(bound), nil
}

func fmtNum(v float64) string {
	if v == math.Trunc(v) && v >= -9.007199254740992e15 && v <= 9.007199254740992e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// withOrderAttr returns the projection to fetch (attrs, plus orderAttr if it was not already
// requested), the index of orderAttr within it, and whether it was appended (so the caller trims
// it from the results).
func withOrderAttr(attrs []string, orderAttr string) (proj []string, orderIdx int, added bool) {
	for i, a := range attrs {
		if strings.EqualFold(a, orderAttr) {
			return attrs, i, false
		}
	}
	return append(append([]string(nil), attrs...), orderAttr), len(attrs), true
}

// topKResult drains seq into a bounded top-K by column orderIdx and returns the rows in sorted
// order (best first: largest for desc, smallest for asc), trimming the appended order column when
// trimOrder is set.
func topKResult(seq iter.Seq[[]classad.Value], orderIdx int, desc bool, k int, trimOrder bool) [][]classad.Value {
	if k <= 0 {
		return nil
	}
	h := &valHeap{desc: desc}
	for row := range seq {
		v, err := row[orderIdx].NumberValue()
		if err != nil {
			continue // order value is not a number: nothing to order it by
		}
		if h.Len() >= k && !h.better(v, h.rows[0].v) {
			continue // cannot beat the worst of the kept best; skip before copying
		}
		// QueryProject reuses the yielded slice across iterations, so snapshot it before
		// retaining. classad.Value is self-contained (scalar/string payloads), so a shallow
		// copy of the slice is a full copy of the row.
		kept := krow{v, append([]classad.Value(nil), row...)}
		if h.Len() < k {
			heap.Push(h, kept)
		} else {
			h.rows[0] = kept
			heap.Fix(h, 0)
		}
	}
	// The heap's root is the worst of the kept set, so Pop yields worst-first; fill the result
	// back-to-front to get best-first (the ORDER BY order).
	out := make([][]classad.Value, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		row := heap.Pop(h).(krow).row
		if trimOrder {
			row = row[:len(row)-1]
		}
		out[i] = row
	}
	return out
}

type krow struct {
	v   float64
	row []classad.Value
}

// valHeap keeps the current best k rows with its ROOT being the worst of them, so a new row is
// compared against the root and, if better, replaces it. For desc (want the largest k) that is a
// min-heap; for asc (smallest k) a max-heap.
type valHeap struct {
	rows []krow
	desc bool
}

func (h *valHeap) Len() int { return len(h.rows) }
func (h *valHeap) Less(i, j int) bool {
	if h.desc {
		return h.rows[i].v < h.rows[j].v // min at root
	}
	return h.rows[i].v > h.rows[j].v // max at root
}
func (h *valHeap) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *valHeap) Push(x any)    { h.rows = append(h.rows, x.(krow)) }
func (h *valHeap) Pop() any      { n := len(h.rows); x := h.rows[n-1]; h.rows = h.rows[:n-1]; return x }
func (h *valHeap) better(v, worst float64) bool {
	if h.desc {
		return v > worst
	}
	return v < worst
}
