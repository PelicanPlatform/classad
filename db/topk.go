package db

import (
	"container/heap"
	"iter"
	"math"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
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
	proj, orderIdx, added := withOrderAttr(attrs, orderAttr)
	// Two-pass: a columnar cutoff scan finds the k-th order value, then the projection runs with
	// `orderAttr {>=|<=} cutoff` appended -- which the columns and zone maps narrow to ~k rows --
	// instead of projecting every matching row and heaping the lot. Falls back to the plain path
	// (unaugmented) when the columnar cutoff is unavailable.
	c2, err := topKAugment(constraint, orderAttr, desc, k,
		func(q *vm.Query) (float64, int, bool) { return t.a.TopKOrderThreshold(q, orderAttr, desc, k) })
	if err != nil {
		return nil, err
	}
	seq, err := t.QueryProject(c2, proj)
	if err != nil {
		return nil, err
	}
	return topKResult(seq, orderIdx, desc, k, added), nil
}

// TopK is ArchiveTable.TopK for a mutable table.
func (db *DB) TopK(constraint string, attrs []string, orderAttr string, desc bool, k int) ([][]classad.Value, error) {
	proj, orderIdx, added := withOrderAttr(attrs, orderAttr)
	c2, err := topKAugment(constraint, orderAttr, desc, k,
		func(q *vm.Query) (float64, int, bool) { return db.c.TopKOrderThreshold(q, orderAttr, desc, k) })
	if err != nil {
		return nil, err
	}
	seq, err := db.QueryProject(c2, proj)
	if err != nil {
		return nil, err
	}
	return topKResult(seq, orderIdx, desc, k, added), nil
}

// topKAugment returns the constraint the projection pass should use: the original with
// `orderAttr {>=|<=} cutoff` appended when a columnar cutoff scan (threshold) yields a usable one
// (available, and more than k matching rows so the cutoff is the true k-th value). Otherwise it
// returns the original unchanged -- the plain full-scan path -- so behavior is never worse than
// before. A blank constraint is treated as `true`.
func topKAugment(constraint, orderAttr string, desc bool, k int, threshold func(*vm.Query) (float64, int, bool)) (string, error) {
	base := constraint
	if strings.TrimSpace(base) == "" {
		base = "true"
	}
	if k <= 0 || k > topKMaxColumnar {
		return base, nil
	}
	q, err := vm.Parse(base)
	if err != nil {
		return "", err
	}
	cut, seen, ok := threshold(q)
	if !ok || seen <= k {
		return base, nil // columnar cutoff unavailable, or <=k rows: nothing to narrow
	}
	op := ">="
	if !desc {
		op = "<="
	}
	return "(" + base + ") && (" + orderAttr + " " + op + " " + fmtNum(cut) + ")", nil
}

// fmtNum renders a cutoff value as a ClassAd numeric literal: an integer when the value is integral
// (the common case -- ClusterId, QDate), else a round-tripping real. The literal is compared against
// the order column, so it must parse back to exactly this value.
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
