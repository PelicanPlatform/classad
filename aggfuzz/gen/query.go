package gen

import (
	"fmt"
	"strconv"
	"strings"
)

// The query half of the generator.
//
// A Query is engine-neutral: gen lives in the root module and must not import db, so the aggregate
// function is a small enum here that the fuzzer maps onto db.AggFunc. The mapping is written out
// explicitly rather than by sharing the integer values, so a reordering of either set is a compile
// error instead of a silently wrong aggregate.
//
// Every shape in the dispatch chain has to be reachable from these draws:
//
//	zero group columns                  -> the ungrouped index count, the constrained columnar
//	                                       count, and the ungrouped columnar aggregate
//	one raw group column                -> the categorical index path, and the columnar grouped path
//	one bucketed group column           -> the columnar grouped path only (the index declines a lone
//	                                       bucketed column: there is nothing to read counts from)
//	two columns, one raw + one bucketed -> the bucketed index path ("per owner per day")
//	a string group column               -> the dictionary / positional string readers
//	a boolean or expression column      -> a forced decline, which must reach the scan and agree
//
// and the constraint draws cover match-all, match-none, numeric conjunctions (the predicate
// analysis path), string equalities (the dictionary prune), disjunctions (the vector path),
// presence probes and member() over constant lists (set-membership probes).

// Func is an aggregate function. Mirrors db.AggFunc's set without importing it.
type Func int

const (
	FuncCount Func = iota
	FuncSum
	FuncAvg
	FuncMin
	FuncMax
	FuncCountDistinct
)

func (f Func) String() string {
	switch f {
	case FuncCount:
		return "COUNT"
	case FuncSum:
		return "SUM"
	case FuncAvg:
		return "AVG"
	case FuncMin:
		return "MIN"
	case FuncMax:
		return "MAX"
	case FuncCountDistinct:
		return "COUNT_DISTINCT"
	}
	return "?"
}

// Group is one GROUP BY column, optionally floored into fixed-width buckets.
type Group struct {
	Attr        string
	BucketWidth int64
}

// Agg is one aggregate: a function over an argument attribute, with an optional per-aggregate
// FILTER (which every fast path must decline rather than answer unfiltered).
type Agg struct {
	Func   Func
	Arg    string
	Filter string
}

// Query is one generated aggregate.
type Query struct {
	Constraint string
	Groups     []Group
	Aggs       []Agg
}

// String renders the query the way a reproducer should read it.
func (q Query) String() string {
	var sel []string
	for _, g := range q.Groups {
		if g.BucketWidth != 0 {
			sel = append(sel, fmt.Sprintf("bucket(%s,%d)", g.Attr, g.BucketWidth))
			continue
		}
		sel = append(sel, g.Attr)
	}
	for _, a := range q.Aggs {
		s := fmt.Sprintf("%s(%s)", a.Func, a.Arg)
		if a.Filter != "" {
			s += " FILTER (WHERE " + a.Filter + ")"
		}
		sel = append(sel, s)
	}
	out := "SELECT " + strings.Join(sel, ", ")
	if q.Constraint != "" {
		out += " WHERE " + q.Constraint
	}
	if len(q.Groups) > 0 {
		var gb []string
		for _, g := range q.Groups {
			if g.BucketWidth != 0 {
				gb = append(gb, fmt.Sprintf("bucket(%s,%d)", g.Attr, g.BucketWidth))
				continue
			}
			gb = append(gb, g.Attr)
		}
		out += " GROUP BY " + strings.Join(gb, ", ")
	}
	return out
}

// src is a wrap-around byte reader over the fuzz input. Wrapping (rather than returning zero past
// the end) keeps a short mutated input from collapsing onto one query, and keeps every input --
// including the empty one -- a valid query rather than a skip.
type src struct {
	b []byte
	i int
}

func (s *src) u8() byte {
	if len(s.b) == 0 {
		return 0
	}
	v := s.b[s.i%len(s.b)]
	s.i++
	return v
}

// n returns a value in [0,k).
func (s *src) n(k int) int {
	if k <= 1 {
		return 0
	}
	return int(s.u8()) % k
}

func pick[T any](s *src, xs []T) T { return xs[s.n(len(xs))] }

// The attribute pools, split by what the storage layer can do with each.
var (
	// numAttrs are numeric columns the columnar accelerator can aggregate and group.
	numAttrs = []string{"ProcId", "Status", "Prio", "Small", "Mem", "Disk", "Big", "Ts", "ClusterId", "Rate", "Calc"}
	// strAttrs are string columns: Owner dictionary-encodes, Host does not, Site and NumLike carry
	// the rendered-text collisions.
	strAttrs = []string{"Owner", "Site", "NumLike", "Host"}
	// oddAttrs decline somewhere: a bool column is not groupable and not aggregatable, an
	// expression-valued field is not a stored literal, Rare is below the presence floor, Err
	// evaluates to ERROR, and Missing is in no record at all.
	oddAttrs = []string{"Flag", "Expr", "Rare", "Err", "Missing"}

	groupAttrs = concat(numAttrs, strAttrs, oddAttrs)
	// aggAttrs weights the numeric columns, since those are what the aggregate fast paths serve,
	// but keeps the others so their declines are exercised too.
	aggAttrs = concat(numAttrs, numAttrs, strAttrs, oddAttrs)

	// Bucket widths. 0 (the raw value) is weighted, since it is the only width the index tier's
	// categorical column accepts; the rest span a single unit up to a day of Ts.
	bucketWidths = []int64{0, 0, 0, 0, 1, 2, 10, 100, 1000, 3600, 86400}

	// Literal domains, drawn from what the generator actually writes so a comparison is winnable.
	ownerLits  = []string{"u00", "u07", "u15", "U07", "nobody"}
	siteLits   = []string{"site-00", "site-03", "site-11", "undefined", "42", "site-99"}
	numLikeLit = []string{"0", "3", "9", "11"}
	statusLits = []int64{1, 2, 3, 5, 6}
	procLits   = []int64{0, 3, 7, 9}
	memLits    = []int64{512, 10000, 30000, 60512, 100000}
	rateLits   = []string{"-20", "0", "0.5", "3.0", "62.5"}

	// Ranges over a zone-mapped column that holds values no comparison can be true of, chosen so
	// that every value the zone DOES bound satisfies them: the bounds are +-Inf, which are min/max's
	// identities over the extended reals, so an ordinary real, an integer and a stored +-Inf all
	// pass while a NaN fails (every comparison with NaN is false) and an expression-valued Calc
	// record is not in the zone at all.
	//
	// That is the shape where a zone verdict can overcount. A zone is built from the values it can
	// bound, so these read as always-true over it; attributing a whole segment to the constraint on
	// that basis would count the records the zone left out. Only the zone's Full flag separates the
	// two. The free-form constraint draw reaches them, but the shape that makes them sharp -- the
	// GROUPED count, which is what attributes whole segments from their verdicts -- needs three
	// independent draws to line up, so countFamily draws that one directly.
	zoneWideConds = []string{
		"Rate >= -1.0e999", "Rate <= 1.0e999", "Rate >= -1.0e999 && Rate <= 1.0e999",
		"Calc >= -1.0e999", "Calc <= 1.0e999", "Rare >= -1.0e999", "Rare <= 1.0e999",
		"Ts >= 0 && Rate >= -1.0e999", "Rate <= 1.0e999 && Calc >= -1.0e999",
		"Mem <= 1000000 && Rare <= 1.0e999",
	}
)

func concat[T any](xs ...[]T) []T {
	var out []T
	for _, x := range xs {
		out = append(out, x...)
	}
	return out
}

// QueryFrom builds a query from fuzz bytes. Deterministic: the same bytes always give the same
// query, which is what makes a saved corpus entry a reproducer.
//
// The first draw is the query FAMILY, and that exists for a coverage reason rather than a
// generative one. Each fast path is gated on a narrow shape -- the index path wants exactly one
// unfiltered COUNT(*), the ungrouped columnar aggregate wants exactly one numeric aggregate and no
// grouping -- and drawing the aggregate list freely hits those shapes a few percent of the time.
// A first run of this generator without families reached the index tier TWICE in 2,146 queries,
// which is not a test of the index tier. The families make the gated shapes common and keep the
// free-form draw as the general case.
func QueryFrom(b []byte) Query {
	s := &src{b: b}
	switch s.n(10) {
	case 0, 1, 2:
		return countFamily(s)
	case 3, 4:
		return numAggFamily(s)
	default:
		return Query{
			Constraint: constraintFrom(s, 2),
			Groups:     groupsFrom(s),
			Aggs:       aggsFrom(s),
		}
	}
}

// countFamily is the shape the index tier and the constrained-count fast path are gated on: a lone
// unfiltered COUNT(*). Which of them (if either) answers then depends on the grouping and on
// whether the constraint is match-all or decomposes into zone-decidable probes, so all of those are
// drawn here.
func countFamily(s *src) Query {
	q := Query{Aggs: []Agg{{Func: FuncCount, Arg: "*"}}}
	switch s.n(10) {
	case 0, 1: // ungrouped: the O(1) record count, or the columnar constrained count
	case 2, 3: // one raw CATEGORICAL column: the categorical index path
		q.Groups = []Group{{Attr: "Owner"}}
	case 4: // one raw column that is not categorically indexed
		q.Groups = []Group{{Attr: pick(s, groupAttrs)}}
	case 5, 6: // "per owner per day": the bucketed index path, in both column orders
		b := Group{Attr: pick(s, []string{"Ts", "ClusterId", "Mem"}), BucketWidth: pick(s, []int64{1, 100, 3600, 86400})}
		if s.n(2) == 0 {
			q.Groups = []Group{{Attr: "Owner"}, b}
		} else {
			q.Groups = []Group{b, {Attr: "Owner"}}
		}
	case 7, 8:
		// The zone-verdict shape: a grouped count over the categorical index, restricted by a range
		// that every value the zone bounds satisfies (see zoneWideConds). The grouped count is what
		// makes it sharp -- it attributes whole segments from their zone verdicts -- and the group
		// column must be Owner, since that is the only categorically indexed attribute. Returns
		// here rather than falling through to the constraint draw below, which would overwrite it.
		q.Groups = []Group{{Attr: "Owner"}}
		q.Constraint = pick(s, zoneWideConds)
		if s.n(4) == 0 {
			q.Groups = nil // and the ungrouped form, which takes the constrained columnar count
		}
		return q
	default:
		q.Groups = []Group{{Attr: pick(s, groupAttrs), BucketWidth: pick(s, bucketWidths)}}
	}
	// Weighted towards match-all, which is what every index path but one requires.
	switch s.n(4) {
	case 0:
		q.Constraint = ""
	case 1:
		q.Constraint = pick(s, []string{"true", "1 == 1"})
	default:
		q.Constraint = constraintFrom(s, 1)
	}
	return q
}

// numAggFamily is the shape ColumnarAggregate is gated on: exactly one aggregate, over a numeric
// attribute, with no grouping. It also draws the grouped form, which is GroupedFromColumns's.
func numAggFamily(s *src) Query {
	q := Query{
		Constraint: constraintFrom(s, 1),
		Aggs: []Agg{{
			Func: pick(s, []Func{FuncSum, FuncAvg, FuncMin, FuncMax, FuncCount}),
			Arg:  pick(s, numAttrs),
		}},
	}
	if s.n(3) == 0 {
		q.Groups = []Group{{Attr: pick(s, groupAttrs), BucketWidth: pick(s, bucketWidths)}}
	}
	return q
}

// QueryAt is QueryFrom over the deterministic byte stream Bytes(k) returns, for the fixed sweep
// the coverage test runs.
func QueryAt(k int) Query { return QueryFrom(Bytes(k)) }

func groupsFrom(s *src) []Group {
	switch s.n(10) {
	case 0, 1: // ungrouped: the ungrouped index count and the ungrouped columnar aggregate
		return nil
	case 2, 3: // the two-column "per owner per day" shape the bucketed index path serves
		return []Group{
			{Attr: "Owner"},
			{Attr: pick(s, []string{"Ts", "ClusterId", "Mem"}), BucketWidth: pick(s, []int64{1, 100, 3600, 86400})},
		}
	case 4: // two columns, neither bucketed, and two in the other order: both decline every fast
		return []Group{{Attr: pick(s, groupAttrs)}, {Attr: pick(s, groupAttrs)}} // path and must scan
	default:
		return []Group{{Attr: pick(s, groupAttrs), BucketWidth: pick(s, bucketWidths)}}
	}
}

func aggsFrom(s *src) []Agg {
	n := 1 + s.n(3)
	out := make([]Agg, n)
	for i := range out {
		a := Agg{Func: pick(s, []Func{FuncCount, FuncCount, FuncSum, FuncAvg, FuncMin, FuncMax, FuncCountDistinct})}
		if a.Func == FuncCount && s.n(2) == 0 {
			a.Arg = "*"
		} else {
			a.Arg = pick(s, aggAttrs)
		}
		// A FILTER on 1-in-16 aggregates: every fast path must decline it rather than report an
		// unfiltered aggregate as a filtered one, so this is a decline that has to be checked.
		if s.n(16) == 0 {
			a.Filter = fmt.Sprintf("Status == %d", pick(s, statusLits))
		}
		out[i] = a
	}
	return out
}

// constraintFrom draws a constraint. depth bounds the recursion into conjunctions/disjunctions.
func constraintFrom(s *src, depth int) string {
	if depth > 0 && s.n(4) == 0 {
		l, r := constraintFrom(s, depth-1), constraintFrom(s, depth-1)
		switch s.n(3) {
		case 0:
			return "(" + l + ") && (" + r + ")"
		case 1:
			// A disjunction is not decomposable into a conjunction of zone-decidable probes, so it
			// is what pushes the columnar layer onto its vector path.
			return "(" + l + ") || (" + r + ")"
		default:
			return "!(" + l + ")"
		}
	}
	switch s.n(13) {
	case 0: // match-all, as the empty constraint...
		return ""
	case 1: // ...and as a literal, which is a different code path (IsMatchAll folds both)
		return pick(s, []string{"true", "1 == 1", "true && true"})
	case 2: // match-none: folded away without reading anything
		return pick(s, []string{"false", "1 == 2", "undefined", "error", "!true"})
	case 3, 4: // a numeric conjunction over zone-mapped columns: the predicate-analysis path
		lo := TsBase + int64(s.n(40))*40*TsStep
		hi := lo + int64(1+s.n(30))*40*TsStep
		out := fmt.Sprintf("Ts >= %d && Ts < %d", lo, hi)
		switch s.n(3) {
		case 0:
			return out
		case 1:
			return out + fmt.Sprintf(" && Status == %d", pick(s, statusLits))
		default:
			return out + fmt.Sprintf(" && ClusterId >= %d", 1000+int64(s.n(140)))
		}
	case 5, 6: // a string equality: the dictionary prune
		switch s.n(3) {
		case 0:
			return "Owner " + pick(s, []string{"==", "!=", "=?="}) + " " + strconv.Quote(pick(s, ownerLits))
		case 1:
			return "Site " + pick(s, []string{"==", "!=", "=?="}) + " " + strconv.Quote(pick(s, siteLits))
		default:
			return "NumLike == " + strconv.Quote(pick(s, numLikeLit))
		}
	case 7: // a disjunction of two cheap numeric probes: the vector path
		return fmt.Sprintf("(Status == %d) || (Mem > %d)", pick(s, statusLits), pick(s, memLits))
	case 8: // presence, including on attributes the columns do not carry
		return pick(s, groupAttrs) + pick(s, []string{" is undefined", " isnt undefined", " =?= undefined", " =!= undefined"})
	case 9: // a plain numeric comparison on a column of each width, or set membership
		switch s.n(6) {
		case 0:
			return "ProcId " + pick(s, []string{"==", "<", ">="}) + " " + strconv.FormatInt(pick(s, procLits), 10)
		case 1:
			return "Mem " + pick(s, []string{"==", "<", ">="}) + " " + strconv.FormatInt(pick(s, memLits), 10)
		case 2:
			return "Prio " + pick(s, []string{"==", "<", ">="}) + " " + strconv.FormatInt(int64(-20+s.n(41)), 10)
		case 3:
			return "Big " + pick(s, []string{"<", ">="}) + " " + strconv.FormatInt(int64(s.n(200)), 10)
		case 4:
			return "Rate " + pick(s, []string{"==", "<", ">="}) + " " + pick(s, rateLits)
		default:
			// member over a constant list: an "in" probe for the value index, the zones and the
			// dictionary prune, and a compiled set for the row and vector evaluators.
			switch s.n(4) {
			case 0:
				return fmt.Sprintf("member(ClusterId, {%d, %d, %d})", 1000+s.n(200), 1000+s.n(200), 1000+s.n(200))
			case 1:
				return fmt.Sprintf("member(ClusterId, {%d, %d.0, true}) && Status == %d",
					1000+s.n(200), 1000+s.n(200), pick(s, statusLits))
			case 2:
				return fmt.Sprintf("member(Owner, {%s, %s})", strconv.Quote(pick(s, ownerLits)), strconv.Quote(pick(s, ownerLits)))
			default:
				return fmt.Sprintf("identicalMember(Owner, {%s}) || member(Status, {%d, %d})",
					strconv.Quote(pick(s, ownerLits)), pick(s, statusLits), pick(s, statusLits))
			}
		}
	case 10: // a range every value a zone can bound satisfies; see zoneWideConds
		return pick(s, zoneWideConds)
	case 11: // elvis and arithmetic: not decomposable, so the scan or the vector path answers
		return pick(s, []string{
			"(Rate ?: 0) > 0", "(Rare ?: -1) == -1", "Mem + ProcId > 30000",
			"(Expr ?: 0) >= 0", "Flag", "!Flag", "Err =?= error",
		})
	default: // a comparison against a value that is never present: matches nothing, the hard way
		return pick(s, []string{"Owner == \"nobody\"", "Ts > 2000000000", "Missing == 1"})
	}
}

// Bytes is the deterministic fuzz input for sweep step k: a 16-byte splitmix64 stream, long enough
// that QueryFrom never wraps on a single query. The sweep and the fuzzer's seed corpus are built
// from the same bytes, so the two explore one space rather than two that can drift apart.
func Bytes(k int) []byte {
	b := make([]byte, 16)
	x := uint64(k)*0x9E3779B97F4A7C15 + 0x1234567
	for i := range b {
		x ^= x >> 30
		x *= 0xBF58476D1CE4E5B9
		x ^= x >> 27
		b[i] = byte(x >> 31)
	}
	return b
}

// IsStringAttr reports whether attr's columnar field is a STRING. Reading that off the attribute
// name would be guessing, so the pool itself answers.
func IsStringAttr(attr string) bool {
	for _, a := range strAttrs {
		if a == attr {
			return true
		}
	}
	return false
}
