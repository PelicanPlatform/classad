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
	// Kind is this step's join: "inner", "left" or "cross".
	Kind string `json:"kind"`
	// ProbeAttr is the attribute the step's table would be narrowed on, empty when it
	// would not be. ProbeWhy says why not, and is empty exactly when ProbeAttr is set.
	//
	// ProbeAttr being set is ELIGIBILITY, not a promise: see the file comment. The running
	// join still declines a step whose FROM side turns out to hold more distinct keys than
	// one constraint may carry.
	ProbeAttr string `json:"probeAttr,omitempty"`
	ProbeWhy  string `json:"probeWhy,omitempty"`
	// EstRecords is the size the probe decision priced this table's read at, which is what
	// narrowing would avoid. 0 when the decision declined before a size was needed.
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
	planned := cat.planJoinOrder(spec, false, &stats)
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

	for i := range steps {
		sp := p.steps[i]
		attrs, rows, _, why := cat.probeGateAll(sp, sides[sp.side])
		// Every attribute the probe would narrow on, not one: a composite key is narrowed
		// on all of its equalities, and a report naming one of them would describe a
		// narrower plan than the join runs.
		names := make([]string, 0, len(attrs))
		for _, pa := range attrs {
			names = append(names, pa.attr)
		}
		attr := strings.Join(names, ", ")
		out.Steps = append(out.Steps, JoinStepExplain{
			Alias:      p.aliases[sp.side],
			Kind:       joinKindName(steps[i].Kind),
			ProbeAttr:  attr,
			ProbeWhy:   why,
			EstRecords: rows,
			Cond:       steps[i].Cond,
			Equi:       steps[i].Equi,
		})
	}
	return out, nil
}
