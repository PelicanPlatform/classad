package db

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// TOP-K ROWS PER GROUP: the k rows of each group with the largest (or smallest) order value.
//
// This is what a ranking window asks for -- ROW_NUMBER() OVER (PARTITION BY Owner ORDER BY QDate
// DESC) with a QUALIFY -- and the reason it is here rather than in the caller is the ROW COUNT.
// A client doing it itself takes every matching row and ranks them; over 100 owners holding a
// thousand jobs each that is 100,000 rows to keep 500.
//
// WHY NOT THE CUTOFF. AggTopKThreshold answers the same question as a per-group threshold, which
// the caller then puts in a constraint. Measured, that constraint is the problem: an OR of
// per-group windows is not columnar-eligible, so the second pass falls off the columnar scan and
// costs about a pass per term -- 5.4s at 1000 groups where fetching everything costs 27ms (see
// aggtopk_bench_test.go). Keeping the ROWS needs no second pass and no constraint, and it is
// exact where a cutoff admits every row tied with the k-th.
//
// WHAT THIS RUNG IS AND IS NOT. It is the REFERENCE: one scan, every matching row decoded and
// offered to its group's keep. It does no less storage work than the caller's own ranking --
// what it saves is the rows, which is the term that scales with the network and not with the
// scan. A columnar rung that reads only the group and order columns, and materializes only the
// records that survive, is the next step and has this to be differential-tested against.
//
// ORDER WITHIN A GROUP is the order asked for: the rows come back sorted by the order attribute,
// best first, so a caller numbering them does not sort again.

// TopKRowsSpec is one "k rows per group" request.
//
// OrderAttr must be numeric in the rows that are to be ranked. A row whose order attribute is
// absent, non-numeric or a NaN is NOT ranked and does not appear -- the same rule the numeric
// aggregates follow, and the only one under which "the newest five" means anything. Such a row
// still belongs to its group for any other purpose; this call simply has nothing to order it by.
type TopKRowsSpec struct {
	// GroupCols are the PARTITION BY columns. Empty means one partition holding every row,
	// which is the plain "top k overall" and is served here too.
	GroupCols []GroupCol
	OrderAttr string
	Desc      bool
	K         int
	// Attrs are the attributes each returned row carries, as QueryProject's are. The group
	// columns and the order attribute are NOT added for you: a caller that wants them in the
	// rows asks for them, exactly as it would of a projection.
	Attrs []string
}

// TopKRowsGroup is one group's answer: the group columns' values and its best rows, ordered.
type TopKRowsGroup struct {
	Values []classad.Value
	Rows   [][]classad.Value
}

// topKRowKeep holds one group's best k rows. It is the row-valued twin of topKThresholdAcc and
// obeys the same ordering rule, including comparing two integers as integers (their float forms
// can be equal past 2^53).
type topKRowKeep struct {
	vals []NumValue
	rows [][]classad.Value
	k    int
	desc bool
}

func (t *topKRowKeep) offer(v NumValue, row []classad.Value) {
	if v.Kind == NumReal && math.IsNaN(v.F) {
		return // never evicted, so it would hold a slot against every real row
	}
	if len(t.vals) < t.k {
		t.vals = append(t.vals, v)
		t.rows = append(t.rows, row)
		return
	}
	wi := t.worstIdx()
	if topKWorse(t.vals[wi], v, t.desc) {
		t.vals[wi], t.rows[wi] = v, row
	}
}

func (t *topKRowKeep) worstIdx() int {
	wi := 0
	for i := 1; i < len(t.vals); i++ {
		if topKWorse(t.vals[i], t.vals[wi], t.desc) {
			wi = i
		}
	}
	return wi
}

// ordered returns the kept rows best first. The keep is unordered -- it only ever has to find its
// worst -- so the sort happens once per group at the end rather than once per offer.
func (t *topKRowKeep) ordered() [][]classad.Value {
	idx := make([]int, len(t.rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return topKWorse(t.vals[idx[b]], t.vals[idx[a]], t.desc)
	})
	out := make([][]classad.Value, len(idx))
	for i, j := range idx {
		out[i] = t.rows[j]
	}
	return out
}

// TopKRowsPerGroup answers spec over the records matching constraint.
//
// The groups come back in no defined order -- a GROUP BY defines none -- but each group's rows
// are ordered by the order attribute, best first. A group with fewer than K ranked rows returns
// all of them.
func (db *DB) TopKRowsPerGroup(constraint string, spec TopKRowsSpec) ([]TopKRowsGroup, error) {
	return db.topKRowsPerGroup(constraint, spec, nil)
}

// TopKRowsPerGroupStats is TopKRowsPerGroup filling stats (may be nil) with the scan's work.
func (db *DB) TopKRowsPerGroupStats(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	return db.topKRowsPerGroup(constraint, spec, stats)
}

func (db *DB) topKRowsPerGroup(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	if spec.K <= 0 {
		return nil, fmt.Errorf("classad-db: top-k rows needs a positive K")
	}
	if spec.OrderAttr == "" || spec.OrderAttr == "*" {
		return nil, fmt.Errorf("classad-db: top-k rows needs an attribute to order by")
	}
	for _, g := range spec.GroupCols {
		if g.Attr == "" || g.Attr == "*" {
			return nil, fmt.Errorf("classad-db: a group column must name an attribute")
		}
		if g.BucketWidth < 0 {
			return nil, fmt.Errorf("classad-db: a group column's bucket width cannot be negative")
		}
	}

	// One projection carrying what the caller asked for, then the group columns and the order
	// attribute appended where they are not already in it. The returned rows are cut back to
	// the caller's own attributes, so asking for a group column does not silently add one.
	attrs := append([]string(nil), spec.Attrs...)
	pos := map[string]int{}
	for i, a := range attrs {
		pos[strings.ToLower(a)] = i
	}
	need := func(a string) int {
		if i, ok := pos[strings.ToLower(a)]; ok {
			return i
		}
		attrs = append(attrs, a)
		pos[strings.ToLower(a)] = len(attrs) - 1
		return len(attrs) - 1
	}
	groupAt := make([]int, len(spec.GroupCols))
	for i, g := range spec.GroupCols {
		groupAt[i] = need(g.Attr)
	}
	orderAt := need(spec.OrderAttr)
	keep := len(spec.Attrs)

	seq, err := db.QueryProjectStats(constraint, attrs, stats)
	if err != nil {
		return nil, err
	}

	type groupEntry struct {
		values []classad.Value
		keep   *topKRowKeep
	}
	at := map[string]int{}
	var groups []*groupEntry
	var label strings.Builder
	for row := range seq {
		ov, ok := numValueOf(row[orderAt])
		if !ok {
			continue // nothing to rank it by; see TopKRowsSpec
		}
		// Keyed on the same rendering a GROUP BY groups by, so two rows fall together here
		// exactly when the aggregate engine would have put them in one group.
		label.Reset()
		values := make([]classad.Value, len(spec.GroupCols))
		dropped := false
		for i, g := range spec.GroupCols {
			v := row[groupAt[i]]
			if g.BucketWidth > 0 {
				text, bok := bucketKeyText(v, g.BucketWidth)
				if !bok {
					dropped = true // not a finite number, so it leaves the series
					break
				}
				v = ParseGroupValue(text)
			}
			values[i] = v
			if i > 0 {
				label.WriteByte(0)
			}
			label.WriteString(GroupValueText(v))
		}
		if dropped {
			continue
		}
		i, seen := at[label.String()]
		if !seen {
			i = len(groups)
			at[label.String()] = i
			groups = append(groups, &groupEntry{
				values: values,
				keep:   &topKRowKeep{k: spec.K, desc: spec.Desc},
			})
		}
		// The projected row is the scan's own slice and is reused for the next record, so a
		// row the keep retains is copied. Only the attributes the caller asked for are kept:
		// a group column appended above for the grouping is not smuggled into the answer.
		groups[i].keep.offer(ov, append([]classad.Value(nil), row[:keep]...))
	}

	out := make([]TopKRowsGroup, len(groups))
	for i, g := range groups {
		out[i] = TopKRowsGroup{Values: g.values, Rows: g.keep.ordered()}
	}
	return out, nil
}
