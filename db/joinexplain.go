package db

// Reporting how a join would run, without running it.
//
// Every decision the join planner makes is already recorded -- which side streams and why,
// what each orientation was priced at, whether each step's table can be read through an
// index -- and all of it has been server-side only. A client sees the rows and nothing
// about how they were produced, so a join that silently stopped using an index reads
// exactly like one that never had an index to use. That is the same gap `.explain` closes
// for a constraint query, and this closes it for a join.
//
// WHAT IT CAN AND CANNOT SAY. This reads no records, which is what makes it safe to ask
// about a join too expensive to run. The order decision is complete: it is made entirely
// from table sizes and index metadata, so what is reported here is what a real join would
// do. The PROBE decision is not complete, and the difference is worth stating rather than
// papering over: narrowing a step's table needs the distinct key values the FROM side
// actually holds, and counting those means reading the FROM side. So what is reported per
// step is the part decided before any scan -- whether an index covers the join attribute,
// whether the side is big enough to be worth narrowing, how many records its read is priced
// at. A step that passes all of that is reported as ELIGIBLE, not as narrowed: the running
// join may still decline it once it counts more distinct keys than maxProbeKeys.
//
// WHICH PLAN IT EXPLAINS. The plan a plain Join would run. A grouped aggregate discards row
// order and is planned with that freedom (see joinbuildside.go), so its plan may reorder
// where this one reports it may not.

import (
	"fmt"
	"strings"
)

// JoinExplain is how a join would be executed.
type JoinExplain struct {
	// Planner names the rules that chose the order: "build-side" for a two-table join,
	// "chain" for three or more. They ask different questions -- which of two to hash,
	// against which of several to read FROM -- so which one spoke is part of the answer.
	Planner string `json:"planner"`
	// Sides are the tables in the order the PLAN reads them, the streamed side first.
	Sides []JoinSideExplain `json:"sides"`
	// AsWritten is the aliases in the order the CALLER wrote them. It differs from Sides
	// exactly when Reordered is true.
	AsWritten []string `json:"asWritten"`
	// Reordered reports that the plan reads the tables in a different order than the query
	// wrote them. It changes which order the rows come out in, so a reader who notices
	// that needs to be able to find out it was deliberate.
	Reordered bool `json:"reordered"`
	// OrderWhy is the planner's reason, set whether or not it moved anything.
	OrderWhy string `json:"orderWhy"`
	// OrderCost and OrderAsWrittenCost are what the chosen plan and the plan as written
	// were priced at, in records touched. Both are 0 when the planner declined before
	// pricing anything -- an order it may not change costs nothing to not compare.
	OrderCost          int `json:"orderCost,omitempty"`
	OrderAsWrittenCost int `json:"orderAsWrittenCost,omitempty"`
	// Steps are the folds after the streamed side, in plan order.
	Steps []JoinStepExplain `json:"steps"`
	// Limits are what the join would actually run under: the store's own, lowered to the
	// caller's where the caller asked for less.
	Limits JoinLimits `json:"limits"`

	// EstHashRows is an UPPER BOUND on the rows this plan would hold: every side but the
	// streamed one, each at what its own read is estimated to visit. It is the quantity
	// MaxHashRows bounds, and since the planner chooses which side to hash in order to
	// stay under that bound, it is the number that says whether the plan runs at all.
	//
	// A BOUND and not a prediction, and the gap can be enormous: measured on a selective
	// side joined to an indexed one, this is 8192 where 32 rows are held. An index probe
	// narrows a hashed side to the key values the streamed side turns out to hold, and how
	// many that is cannot be known without reading the streamed side, which this does not
	// do. A step reported as probe-eligible (ProbeAttr) may therefore hold a small fraction
	// of its share of this.
	//
	// Being a bound rather than an estimate is what makes it worth publishing, because it
	// is sound in the direction that matters. At or under MaxHashRows it is a GUARANTEE:
	// the plan cannot be refused for holding too much, since what is held is at most this.
	// Over it, the plan is refused unless a probe narrows one of the sides, and HashWhy
	// names the sides that might. An estimate 256x out would be worse than no number at
	// all; a bound 256x out still answers "will this run".
	//
	// 0 means a hashed side could not be sized at all, and HashWhy names it. A partial sum
	// is not reported: it would read as a bound on the plan and is not one.
	EstHashRows int `json:"estHashRows,omitempty"`
	// HashWhy states EstHashRows against the limit in words: within it, over it, or not
	// known. A plan over the limit is one the join will REFUSE, unless a probe narrows one
	// of the sides enough -- the single case where this report cannot be definite, and it
	// says so by naming the sides that might.
	HashWhy string `json:"hashWhy,omitempty"`
}

// JoinSideExplain is one table of the join and how it would be read.
type JoinSideExplain struct {
	Alias      string `json:"alias"`
	Table      string `json:"table"`
	Constraint string `json:"constraint,omitempty"`
	// Streamed marks the one side the join reads as a stream. Every other side is hashed,
	// which is what MaxHashRows bounds.
	Streamed bool `json:"streamed"`
	// Read is the access path for this side's own constraint: the same report `.explain`
	// gives for a query against that table. It is what says whether the side is read
	// through an index or scanned, and EstVisited in it is the size the join priced it at.
	Read *QueryExplain `json:"read,omitempty"`
	// ReadWhy says why Read is absent -- a table this catalog does not have, or a
	// constraint that does not plan. A side whose read cannot be described is also one the
	// order decision could not price.
	ReadWhy string `json:"readWhy,omitempty"`
}

// JoinStepExplain is one table folded into the accumulated row.
type JoinStepExplain struct {
	Alias string `json:"alias"`
	// Kind is this step's join: "inner", "left", "cross" -- or "right", which is a LEFT
	// join the planner chose to run from the other end, hashing the preserved side and
	// emitting its unmatched rows after the stream. "right" therefore always comes with
	// Reordered, and the two together are the whole statement: the caller's LEFT join,
	// read in the opposite order.
	Kind string `json:"kind"`
	// ProbeAttr is the attribute the step's table would be narrowed on, empty when it
	// would not be. ProbeWhy says why not, and is empty exactly when ProbeAttr is set.
	//
	// ProbeAttr being set is ELIGIBILITY, not a promise: see the file comment. The running
	// join still declines a step whose FROM side turns out to hold more distinct keys than
	// one constraint may carry.
	ProbeAttr string `json:"probeAttr,omitempty"`
	ProbeWhy  string `json:"probeWhy,omitempty"`
	// EstRecords is the size this table's read is estimated to visit: the whole table
	// unless the side carries a constraint of its own. It is both what narrowing this step
	// would avoid and, since every step holds its own table, this step's share of the
	// plan's EstHashRows.
	//
	// 0 only when the side could not be sized at all. It is filled in even where the probe
	// decision declined before it needed a size, because "how much does this step hold" is
	// asked of every step and not only of the ones an index could narrow.
	EstRecords int `json:"estRecords,omitempty"`
	// Cond and Equi are the step's conditions AS PLANNED. A reordered chain re-attaches
	// its conditions to different steps, so these are not always what the caller wrote --
	// which is the point of reporting them.
	Cond []string   `json:"cond,omitempty"`
	Equi []JoinEqui `json:"equi,omitempty"`
}

// joinKindName spells a JoinKind for a reader.
func joinKindName(k JoinKind) string {
	switch k {
	case JoinInner:
		return "inner"
	case JoinLeft:
		return "left"
	case JoinCross:
		return "cross"
	case joinRightOuter:
		return "right"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// ExplainJoin reports how the catalog would run this join, reading no records.
//
// want lowers the limits the way Join does; the limits actually applied come back in the
// report, because a plan that will stop at a limit the caller did not set is a plan the
// caller needs to know about.
func (cat *Catalog) ExplainJoin(spec JoinSpec, want JoinLimits) (JoinExplain, error) {
	limits := DefaultJoinLimits().clamp(want)
	if err := validateJoinKinds(spec); err != nil {
		return JoinExplain{}, err
	}
	asWritten, _, err := spec.chain()
	if err != nil {
		return JoinExplain{}, err
	}
	out := JoinExplain{Planner: "build-side", Limits: limits}
	if len(spec.Steps) > 0 {
		out.Planner = "chain"
	}
	for _, s := range asWritten {
		out.AsWritten = append(out.AsWritten, s.Alias)
	}

	// The same call Join makes, so what is reported is what would run rather than a second
	// derivation of it. unordered is false because this explains a plain Join; an
	// aggregate gives up row order and may be planned more freely.
	var stats JoinStats
	planned := cat.planJoinOrder(spec, limits, false, &stats)
	// Each planner records under its own names, and only the one that ran writes anything.
	if out.Planner == "chain" {
		out.OrderWhy = stats.ChainOrderWhy
		out.OrderCost, out.OrderAsWrittenCost = stats.ChainOrderCost, stats.ChainOrderAsWrittenCost
	} else {
		out.OrderWhy = stats.BuildSideWhy
		out.OrderAsWrittenCost = stats.BuildSideCost
		out.OrderCost = stats.BuildSideCost
		if stats.BuildSideSwapped {
			out.OrderCost = stats.BuildSideSwapCost
		}
	}

	p, err := compileJoin(planned, limits)
	if err != nil {
		return JoinExplain{}, err
	}
	sides, steps, err := planned.chain()
	if err != nil {
		return JoinExplain{}, err
	}
	for i, sd := range sides {
		se := JoinSideExplain{
			Alias: sd.Alias, Table: sd.Table, Constraint: sd.Constraint, Streamed: i == 0,
		}
		if ex, exErr := cat.explainSide(sd); exErr != nil {
			se.ReadWhy = exErr.Error()
		} else {
			se.Read = &ex
		}
		out.Sides = append(out.Sides, se)
	}
	// Asked of the planner rather than re-derived by comparing the two orders. The planner
	// sets exactly one of these when it rewrites the spec and neither when it declines, so
	// this is its own answer rather than a second opinion about it.
	out.Reordered = stats.BuildSideSwapped || stats.ChainFromAlias != ""

	// What the plan would HOLD, accumulated as the steps are described. Every side but the
	// streamed one is hashed and every step is one of those sides, so the steps are exactly
	// the sides this has to add up.
	held, probeable, unsized := 0, []string(nil), []string(nil)
	for i := range steps {
		sp := p.steps[i]
		attrs, all, rows, _, why := cat.probeGateAll(sp, sides[sp.side])
		// Every attribute the probe would narrow on, not one: a composite key is narrowed
		// on all of its equalities, and a report naming one of them would describe a
		// narrower plan than the join runs.
		//
		// Resolved through the same tuple selection the planner uses, so that a step whose
		// key a composite answers as a whole reports that whole key -- including any
		// component-only attribute, which carries no index of its own and so is absent
		// from the per-attribute candidates.
		names := attrNames(attrs)
		if set, ok := cat.tupleSetFor(sides[sp.side].Table, all, attrs); ok {
			names = set.attrs
		}
		attr := strings.Join(names, ", ")
		alias := p.aliases[sp.side]
		// A gate that declined before it needed a size leaves rows at 0, which is not an
		// answer to "how much does this step hold". Ask for the size directly: it is the
		// same number the planner priced the side at, and it costs index statistics rather
		// than records.
		if rows == 0 {
			if sized, ok := cat.sideReadSize(sides[sp.side]); ok {
				rows = sized
			} else {
				unsized = append(unsized, alias)
			}
		}
		held += rows
		if attr != "" {
			probeable = append(probeable, alias)
		}
		out.Steps = append(out.Steps, JoinStepExplain{
			Alias:      alias,
			Kind:       joinKindName(steps[i].Kind),
			ProbeAttr:  attr,
			ProbeWhy:   why,
			EstRecords: rows,
			Cond:       steps[i].Cond,
			Equi:       steps[i].Equi,
		})
	}
	out.EstHashRows, out.HashWhy = explainHashBound(held, limits.MaxHashRows, probeable, unsized)
	return out, nil
}

// explainHashBound puts the plan's hold against the limit in words.
//
// It reports no number at all when a side could not be sized, because the sum of the rest
// would read as a bound on the whole plan and is not one -- it bounds the part that happened
// to be measurable, which is a different and much weaker claim.
func explainHashBound(held, maxHashRows int, probeable, unsized []string) (int, string) {
	if len(unsized) > 0 {
		return 0, fmt.Sprintf("%s could not be sized, so what this plan holds is not known",
			strings.Join(unsized, ", "))
	}
	if held <= maxHashRows {
		return held, fmt.Sprintf("the plan holds at most ~%d rows, within the %d this join "+
			"may hold", held, maxHashRows)
	}
	// Over the limit. Whether that is fatal turns on the one thing this cannot see: an
	// index probe narrows a hashed side to the keys the streamed side actually holds, and
	// counting those means reading it.
	if len(probeable) > 0 {
		return held, fmt.Sprintf("the plan holds up to ~%d rows, more than the %d this join "+
			"may hold; it will be refused unless an index probe narrows %s far enough",
			held, maxHashRows, strings.Join(probeable, ", "))
	}
	return held, fmt.Sprintf("the plan holds ~%d rows, more than the %d this join may hold, "+
		"and no step can be narrowed by an index: it will be refused", held, maxHashRows)
}
