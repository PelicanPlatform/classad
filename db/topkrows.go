package db

import (
	"fmt"
	"iter"
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
// WHY NOT A CUTOFF. The obvious cheaper-looking design computes a per-group THRESHOLD -- the k-th
// best order value of each group -- and lets the caller put it in a constraint. That was built
// and measured, and the constraint is what kills it: an OR of per-group windows is not
// columnar-eligible, so the pass that applies it falls off the columnar scan and costs about a
// full pass PER TERM. 5.4s at 1000 groups, where fetching every one of the 100k rows and ranking
// them costs 27ms. topkrows_bench_test.go keeps that measurement.
//
// Keeping the ROWS needs no second pass and no constraint at all, and it is exact where a cutoff
// admits every row tied with the k-th.
//
// WHAT THIS RUNG IS AND IS NOT. It is the REFERENCE: one scan, every matching row decoded and
// offered to its group's keep. It does no less storage work than the caller's own ranking --
// what it saves is the rows, which is the term that scales with the network and not with the
// scan. A columnar rung that reads only the group and order columns, and materializes only the
// records that survive, is the next step and has this to be differential-tested against.
//
// ORDER WITHIN A GROUP is the order asked for: the rows come back sorted by the order attribute,
// best first, so a caller numbering them does not sort again.

// TopKOrderTerm is one term of the ordering, most significant first.
type TopKOrderTerm struct {
	Attr string
	Desc bool
}

// TopKRowsSpec is one "k rows per group" request.
//
// EVERY ROW RANKS. The ordering attributes do not have to be numeric, or present: rows are ordered
// by collections.CompareValues, the total order a GROUP BY result is sorted by, so a string orders
// against a string and a row missing the attribute orders as undefined -- after every number and
// string, but ranked. This is the order a caller sorting the same rows itself would use, which is
// the point: a pushdown that silently dropped the rows it could not read numerically would answer
// a different question than the client code it replaces, and would look fast doing it.
//
// WHY THE ORDERING IS A LIST. "The top 5" is only a well-defined SET of rows when the ordering is
// total over them. Where it ties across the K-th place, which rows come back is a choice, and
// this makes a different choice than a client sorting the same rows does -- measured, not
// supposed: see TestTopKTieBreakingAgainstAClientRanking. A caller that needs the answer to be
// determined writes an ordering that cannot tie, which for a job table is QDate then ClusterId,
// both already in the ad. A single term remains the common spelling and is what a caller writes
// when it does not care which of several equal rows it gets.
type TopKRowsSpec struct {
	// GroupCols are the PARTITION BY columns. Empty means one partition holding every row,
	// which is the plain "top k overall" and is served here too.
	GroupCols []GroupCol
	// OrderBy is the ordering, most significant term first. It must name at least one term.
	OrderBy []TopKOrderTerm
	K       int
	// Attrs are the attributes each returned row carries, as QueryProject's are. The group
	// columns and the ordering attributes are NOT added for you: a caller that wants them in
	// the rows asks for them, exactly as it would of a projection.
	Attrs []string
}

// OrderOne is the spec for the common single-term ordering.
func OrderOne(attr string, desc bool) []TopKOrderTerm {
	return []TopKOrderTerm{{Attr: attr, Desc: desc}}
}

// TopKRowsSource is a table the top-k rungs can read: the mutable DB and an ArchiveTable both
// expose exactly these two scans with these signatures, which is why one implementation serves
// both rather than the archive growing a parallel copy of it.
//
// History is where "the top N per group" mostly points -- the five longest jobs each owner ran
// last month is a question about completed jobs, not queued ones -- so an archive that could not
// answer it was the bigger gap of the two.
type TopKRowsSource interface {
	QueryProjectStats(constraint string, attrs []string, stats *collections.ScanStats) (iter.Seq[[]classad.Value], error)
	QueryProjectStatsRedacted(constraint string, attrs []string, stats *collections.ScanStats) (iter.Seq[[]classad.Value], error)
	QueryRawWireRefsStats(constraint string, projection []string, redact bool, stats *collections.ScanStats) (iter.Seq[[]byte], error)
}

// TopKRowsGroup is one group's answer: the group columns' values and its best rows, ordered.
type TopKRowsGroup struct {
	Values []classad.Value
	Rows   [][]classad.Value
}

// validate rejects a spec no rung can serve. Both rungs call it, so a spec accepted by one is
// accepted by the other -- a request that errored on the value rung and quietly returned
// something on the wire rung would be the worst kind of divergence between them.
func (s TopKRowsSpec) validate() error {
	if s.K <= 0 {
		return fmt.Errorf("classad-db: top-k rows needs a positive K")
	}
	if len(s.OrderBy) == 0 {
		return fmt.Errorf("classad-db: top-k rows needs an attribute to order by")
	}
	for _, t := range s.OrderBy {
		if t.Attr == "" || t.Attr == "*" {
			return fmt.Errorf("classad-db: an ordering term must name an attribute")
		}
	}
	for _, g := range s.GroupCols {
		if g.Attr == "" || g.Attr == "*" {
			return fmt.Errorf("classad-db: a group column must name an attribute")
		}
		if g.BucketWidth < 0 {
			return fmt.Errorf("classad-db: a group column's bucket width cannot be negative")
		}
	}
	return nil
}

// topKRowKeep holds one group's best k rows. vals is unordered and at most k long, so offer is
// O(k) and the state is bounded by the REQUEST rather than by the group -- which is what makes
// this safe to point at a history table where a group holds millions of rows.
type topKRowKeep struct {
	// vals holds every kept row's ordering values end to end, stride terms long, so one keep
	// is two allocations however many terms the ordering has.
	vals  []classad.Value
	rows  [][]classad.Value
	k     int
	terms []TopKOrderTerm
}

func (t *topKRowKeep) stride() int { return len(t.terms) }

func (t *topKRowKeep) at(i int) []classad.Value {
	n := t.stride()
	return t.vals[i*n : (i+1)*n]
}

func (t *topKRowKeep) count() int { return len(t.vals) / t.stride() }

// topKWorse reports whether a is the value a better one would evict: the smallest when keeping the
// largest k (desc), the largest when keeping the smallest k (asc).
//
// The order is collections.CompareValues -- the one a GROUP BY result is sorted by -- so a row is
// ranked whatever its ordering attribute holds, and ranked the same way a caller sorting the same
// rows itself would rank it. Two integers compare as integers there, which matters here: past 2^53
// the float conversion calls two different job ids equal and the keep holds the wrong one.
func topKWorse(a, b classad.Value, desc bool) bool {
	c := collections.CompareValues(a, b)
	if desc {
		return c < 0
	}
	return c > 0
}

// topKWorseRow is topKWorse over a whole ordering: the first term that separates a from b
// decides, and an ordering whose every term ties reports false, so neither row evicts the other
// and the one already held stays.
func topKWorseRow(a, b []classad.Value, terms []TopKOrderTerm) bool {
	for i, t := range terms {
		if topKWorse(a[i], b[i], t.Desc) {
			return true
		}
		if topKWorse(b[i], a[i], t.Desc) {
			return false
		}
	}
	return false
}

func (t *topKRowKeep) offer(vals, row []classad.Value) {
	if t.count() < t.k {
		t.vals = append(t.vals, vals...)
		t.rows = append(t.rows, row)
		return
	}
	wi := t.worstIdx()
	if topKWorseRow(t.at(wi), vals, t.terms) {
		copy(t.at(wi), vals)
		t.rows[wi] = row
	}
}

func (t *topKRowKeep) worstIdx() int {
	wi := 0
	for i := 1; i < t.count(); i++ {
		if topKWorseRow(t.at(i), t.at(wi), t.terms) {
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
		return topKWorseRow(t.at(idx[b]), t.at(idx[a]), t.terms)
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
	return topKRowsPerGroup(db, constraint, spec, false, nil)
}

// TopKRowsPerGroupStats is TopKRowsPerGroup filling stats (may be nil) with the scan's work.
func (db *DB) TopKRowsPerGroupStats(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(db, constraint, spec, false, stats)
}

// TopKRowsPerGroupRedacted is TopKRowsPerGroup with private attributes stripped at the source,
// for serving a reader not entitled to them.
//
// This rung's exposure is NARROWER than the wire rung's: it returns one value per attribute the
// caller NAMED and nothing else, so a server can also refuse a private name up front, as the
// aggregate handler does. The pair exists anyway, because the two rungs answer the same
// question and a reviewer comparing them should not find one that can redact and one that
// cannot -- and because the caller that reaches for the value rung next will not be looking for
// a gap this one left.
func (db *DB) TopKRowsPerGroupRedacted(constraint string, spec TopKRowsSpec) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(db, constraint, spec, true, nil)
}

// TopKRowsPerGroupRedactedStats is TopKRowsPerGroupRedacted filling stats (may be nil).
func (db *DB) TopKRowsPerGroupRedactedStats(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(db, constraint, spec, true, stats)
}

// TopKRowsPerGroup is DB.TopKRowsPerGroup over a history table.
func (t *ArchiveTable) TopKRowsPerGroup(constraint string, spec TopKRowsSpec) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(t, constraint, spec, false, nil)
}

// TopKRowsPerGroupStats is ArchiveTable.TopKRowsPerGroup filling stats (may be nil).
func (t *ArchiveTable) TopKRowsPerGroupStats(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(t, constraint, spec, false, stats)
}

// TopKRowsPerGroupRedacted is DB.TopKRowsPerGroupRedacted over a history table.
func (t *ArchiveTable) TopKRowsPerGroupRedacted(constraint string, spec TopKRowsSpec) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(t, constraint, spec, true, nil)
}

// TopKRowsPerGroupRedactedStats is ArchiveTable.TopKRowsPerGroupRedacted filling stats.
func (t *ArchiveTable) TopKRowsPerGroupRedactedStats(constraint string, spec TopKRowsSpec, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	return topKRowsPerGroup(t, constraint, spec, true, stats)
}

func topKRowsPerGroup(src TopKRowsSource, constraint string, spec TopKRowsSpec, redact bool, stats *collections.ScanStats) ([]TopKRowsGroup, error) {
	if err := spec.validate(); err != nil {
		return nil, err
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
	orderAt := make([]int, len(spec.OrderBy))
	for i, t := range spec.OrderBy {
		orderAt[i] = need(t.Attr)
	}
	keep := len(spec.Attrs)

	project := src.QueryProjectStats
	if redact {
		project = src.QueryProjectStatsRedacted
	}
	seq, err := project(constraint, attrs, stats)
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
	ovals := make([]classad.Value, len(spec.OrderBy)) // reused: the keep copies what it keeps
	for row := range seq {
		for i, at := range orderAt {
			ovals[i] = row[at]
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
				keep:   &topKRowKeep{k: spec.K, terms: spec.OrderBy},
			})
		}
		// The projected row is the scan's own slice and is reused for the next record, so a
		// row the keep retains is copied. Only the attributes the caller asked for are kept:
		// a group column appended above for the grouping is not smuggled into the answer.
		groups[i].keep.offer(ovals, append([]classad.Value(nil), row[:keep]...))
	}

	out := make([]TopKRowsGroup, len(groups))
	for i, g := range groups {
		out[i] = TopKRowsGroup{Values: g.values, Rows: g.keep.ordered()}
	}
	return out, nil
}
