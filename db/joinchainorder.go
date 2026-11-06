package db

// Choosing which table a chain reads FROM.
//
// A chain streams its FROM table and hashes every other one, so the FROM side is where the
// pipeline starts and -- much more importantly -- it is where the index probes get their
// keys. planIndexProbes narrows each step's table to the distinct key values the FROM side
// holds, which means the FROM side's key count decides whether ANY step can be read through
// an index. A chain written FROM a large table offers more keys than maxProbeKeys and every
// step falls back to a full scan; the same chain written FROM a small one reads each large
// table through its index instead.
//
// That is a much larger effect than permuting the steps, and the two were measured against
// each other before this was written. On three tables joined on one key, two of 8192 rows
// and one of 32 (BenchmarkChainFromSide):
//
//	FROM the large table   8224 rows hashed, 8192 streamed, 8224 pairs   20.8 ms
//	FROM the small table     64 rows hashed,   32 streamed,   64 pairs    1.1 ms
//
// 19x, and it is 19x because the FROM side changes what is READ.
//
// Permuting the steps alone does not: a chain reads every table once whichever order its
// steps are in, so what moves is only the pair loop. On a shape built to maximize even that
// -- a step that doubles the row count folded before a step that drops all but 50 of them --
// the pairs examined fell from 40100 to 200, a 200x difference, and the clock went from
// 22.2 ms to 15.3 ms. 1.45x, because the scans that dominate are the same scans either way
// (BenchmarkChainStepOrder).
//
// So the step order is worth having and the FROM side is the reason to be here.
//
// WHICH CHAINS MAY BE REWRITTEN. Re-centring is sound when every table joins on ONE key,
// because that is what makes the conditions transitive: with a.K == b.K and a.K == c.K,
// b.K == c.K follows, so any of the three may sit in the middle and the other two attach
// directly to it.
//
// That is a property of the equalities, NOT of how the query hung the tables off each other.
// A chain spelled as a path -- a joined to b, then b joined to c -- is the same relation as
// the star, and is the more natural way to write it. So the equalities are read as a graph
// over (table, attribute) pairs and the pairs are unioned; see chainToStar for the three
// things that make the resulting class a usable key. A chain that joins on two genuinely
// different keys is a real shape and this is not sound for it: promoting one table would
// leave the others attached to a table that no longer supplies the probe keys. Those are
// declined and say so.
//
// WHAT MAY NOT BE REORDERED is what joinbuildside.go says: the streamed side's order is the
// join's output order, so a chain is reordered only where there is no order to preserve --
// a FROM side that promises none, or a caller that discards it. A chain that also carries a
// LIMIT is reordered under that same precondition and no extra one: a LIMIT with no promised
// order selects an arbitrary subset by definition, and the precondition is exactly that no
// order was promised.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// minChainOrderRows is the size the largest side must reach before a chain is re-planned.
// Below it every table is small enough to read and hold for nothing, so there is no benefit
// to weigh and a real cost to trying -- the same judgement, for the same reason, as
// minBuildSideRows and minProbeRows.
var minChainOrderRows = 1024

// forceNoChainReorder keeps a chain as written so a test can compare both plans and a
// benchmark both costs in one process. See forceNoBuildSideSwap for why this is a variable.
var forceNoChainReorder bool

// planJoinOrder puts a spec's sides in the order they should be read.
//
// The two spellings are planned separately and deliberately. A two-table join has one
// decision -- which of the two to hash -- and joinbuildside.go makes it against a cost model
// measured on that shape. A chain has a different one: which table to read FROM, which is
// what feeds every step's probe. Running the chain planner over a two-table spec would
// replace a measured decision with a re-derivation of it.
func (cat *Catalog) planJoinOrder(spec JoinSpec, unordered bool, stats *JoinStats) JoinSpec {
	if len(spec.Steps) > 0 {
		return cat.planChainOrder(spec, unordered, stats)
	}
	return cat.planBuildSide(spec, unordered, stats)
}

// chainStar is a chain whose tables all join on one key: the sides, the attribute each one
// is joined on, and the conditions that are not those equalities.
type chainStar struct {
	sides []JoinSide
	// attrs[i] is side i's half of the join key, as a bare attribute name. Every side has
	// one, which is what makes the chain re-centrable.
	attrs []string
	// facts[i] is what side i's read costs, priced ONCE. Nothing in it depends on which
	// side is read FROM, and asking the store again for each candidate made the planning
	// cost more than the join it was planning: a side's size comes from its sidecars, and
	// reading those is a walk of every segment. Only the probe arithmetic below varies
	// with the candidate, and that is arithmetic over these numbers.
	facts []sideFacts
	// extra are the ON conjuncts that are not a step's hash equality, each with the set of
	// sides it names so it can be re-placed at a step where all of them are in scope.
	extra []chainCond
}

// chainCond is one ON conjunct and the sides it reads.
type chainCond struct {
	src   string
	sides []int
}

// planChainOrder returns spec with its tables in the order they should be read.
func (cat *Catalog) planChainOrder(spec JoinSpec, unordered bool, stats *JoinStats) JoinSpec {
	keep := func(format string, a ...any) JoinSpec {
		stats.ChainOrderWhy = fmt.Sprintf(format, a...)
		return spec
	}
	if forceNoChainReorder {
		return keep("the chain order is disabled")
	}
	if !unordered && !cat.orderFree(spec.Left) {
		return keep("%s is read in a fixed order, which is the order the join's rows come "+
			"out in; set the archive's Unordered to let the planner choose", spec.Left.Alias)
	}
	star, why := chainToStar(spec)
	if why != "" {
		return keep("%s", why)
	}

	if !cat.priceSides(star) {
		return keep("the chain's tables could not all be priced")
	}
	costs := make([]chainCost, len(star.sides))
	for i := range star.sides {
		costs[i] = star.chainCostFrom(i)
	}
	biggest := 0
	for _, c := range costs {
		biggest = max(biggest, c.hashRows)
	}
	if biggest < minChainOrderRows {
		return keep("every table is under %d rows, which is small enough to read in any "+
			"order", minChainOrderRows)
	}

	// The cheapest candidate is found exactly, and the margin is applied ONCE, against the
	// chain as written. Comparing every pair with the margin instead would not be an
	// ordering at all: "enough cheaper than" is not transitive, so which side came out on
	// top would depend on the order the candidates happened to be compared in.
	best := 0
	for i := 1; i < len(costs); i++ {
		if betterChain(costs[i], costs[best]) {
			best = i
		}
	}
	stats.ChainOrderAsWrittenCost, stats.ChainOrderCost = costs[0].total, costs[best].total
	if best != 0 && !worthMoving(costs[best], costs[0]) {
		stats.ChainOrderCost = costs[0].total
		best = 0
	}

	order := star.spokeOrder(best)
	if best == 0 && sort.IntsAreSorted(order) {
		runner := runnerUp(costs, 0)
		return keep("%s is already the table to read FROM (~%d records, against ~%d from "+
			"%s), and its steps are already in order", spec.Left.Alias, costs[0].total,
			costs[runner].total, star.sides[runner].Alias)
	}
	out := star.rewriteOrder(spec, best, order)
	stats.ChainFromAlias = star.sides[best].Alias
	switch {
	case best == 0:
		stats.ChainOrderWhy = fmt.Sprintf("%s is the table to read FROM, and folding its "+
			"steps smallest first carries fewer rows between them", spec.Left.Alias)
	case costs[0].fits != costs[best].fits:
		stats.ChainOrderWhy = fmt.Sprintf("the chain as written would hold ~%d rows, more "+
			"than one join may, and reading FROM %s holds ~%d", costs[0].hashRows,
			star.sides[best].Alias, costs[best].hashRows)
	case better(costs[best].total, costs[0].total):
		stats.ChainOrderWhy = fmt.Sprintf("reading FROM %s touches ~%d records against ~%d "+
			"FROM %s, because %s's %d keys narrow each other table's read",
			star.sides[best].Alias, costs[best].total, costs[0].total, spec.Left.Alias,
			star.sides[best].Alias, min(costs[best].streamRows, maxProbeKeys))
	default:
		stats.ChainOrderWhy = fmt.Sprintf("no order reads less -- every table is read once "+
			"and no index narrows one -- so the chain streams the largest: reading FROM %s "+
			"holds ~%d rows against %s's ~%d", star.sides[best].Alias, costs[best].hashRows,
			spec.Left.Alias, costs[0].hashRows)
	}
	return out
}

// runnerUp names the cheapest side other than skip, for a message that has to say what the
// winner won against.
func runnerUp(costs []chainCost, skip int) int {
	out := -1
	for i := range costs {
		if i == skip {
			continue
		}
		if out < 0 || betterChain(costs[i], costs[out]) {
			out = i
		}
	}
	if out < 0 {
		return skip
	}
	return out
}

// betterChain orders two candidate FROM sides exactly: one that fits beats one that does
// not, then fewer records touched, then fewer rows held.
//
// The last of those is not a tiebreak for form's sake. A chain reads every table once unless
// an index narrows one, so with no index every candidate touches the SAME number of records
// and the totals tie for real -- what is left is which tables are held. Deciding on records
// alone would leave a chain holding two large tables while streaming a small one it could
// have hashed, and report the two plans as equal because by that measure they are.
func betterChain(a, b chainCost) bool {
	if a.fits != b.fits {
		return a.fits
	}
	if a.total != b.total {
		return a.total < b.total
	}
	return a.hashRows < b.hashRows
}

// worthMoving reports that re-centring the chain on `to` is enough better than the chain as
// written to be worth changing the order the rows come out in. It is betterChain with the
// estimator's margin applied, for the one comparison that actually moves anything.
func worthMoving(to, from chainCost) bool {
	if to.fits != from.fits {
		return to.fits
	}
	if better(to.total, from.total) {
		return true
	}
	if better(from.total, to.total) {
		return false
	}
	return better(to.hashRows, from.hashRows)
}

// chainToStar reads a chain as a set of tables joined on ONE key, or says why it is not.
//
// The tables need not be written as a star. A chain spelled as a path -- h joined to e, then
// e joined to m -- is the same relation as the star when every equality is on one key, and a
// path is the more natural way to write it. So the equalities are read as a GRAPH over
// (side, attribute) pairs and the pairs are unioned: what matters is that every table ends
// up in one equivalence class, not which table the query happened to hang the others off.
//
// Three things make that class a usable join key, and each is checked:
//
//   - one class. Two classes are two independent join keys, and there is no single
//     attribute every table could be re-attached by.
//   - every table in it. A table reachable from none of the others is a cross product,
//     whatever order it is folded in.
//
// A third thing would matter and cannot happen. A table appearing in one class under two of
// its OWN attributes would also be saying those two are equal to each other -- a condition
// on that table alone, which re-attaching the tables would regenerate the equalities between
// them and silently drop. It cannot happen because every step introduces exactly one new
// table and so contributes at least one node the graph has not seen: the graph is a forest,
// and a table under two attributes puts them in two components, which the one-class check
// already refuses. So there is no case to handle here, only a reason not to look for one.
func chainToStar(spec JoinSpec) (*chainStar, string) {
	sides, steps, err := spec.chain()
	if err != nil {
		return nil, "the chain could not be read"
	}
	aliases := make([]string, len(sides))
	for i, s := range sides {
		if s.Alias == "" {
			return nil, "a side of the chain has no alias"
		}
		aliases[i] = s.Alias
	}
	star := &chainStar{sides: sides, attrs: make([]string, len(sides))}
	uf := map[keyRef]keyRef{}
	// A node is identified by the FOLDED attribute name, because ClassAd attribute names
	// are case-insensitive and two spellings of one name are one node. The spelling the
	// query used is kept beside it, so the conditions this regenerates read the way the
	// caller wrote them rather than in the planner's normal form.
	spell := map[keyRef]string{}
	for i := range steps {
		st := &steps[i]
		side := i + 1
		if st.Kind != JoinInner {
			return nil, fmt.Sprintf("the step folding in %s is not an inner join, and only "+
				"an inner join may be re-ordered: an outer join's preserved side is the "+
				"one written first", aliases[side])
		}
		if len(st.Equi) != 1 {
			return nil, fmt.Sprintf("the step folding in %s has %d equalities and a chain "+
				"is re-centred on one join key", aliases[side], len(st.Equi))
		}
		// The step's own half is written against its table's ad, so it is a bare attribute.
		attr := bareAttrName(st.Equi[0].Right)
		if attr == "" {
			return nil, fmt.Sprintf("%s's half of the equality is computed, so there is no "+
				"attribute to re-attach it by", aliases[side])
		}
		table := keyRef{side: side, attr: strings.ToLower(attr)}
		row, rowName, rowWhy := rowHalfRef(st.Equi[0].Left, aliases, side)
		if rowWhy != "" {
			return nil, rowWhy
		}
		ufUnion(uf, row, table)
		spell[table], spell[row] = attr, rowName

		for _, src := range st.Cond {
			if isEquality(src, aliases, row, table, side == 1) {
				continue // the hash equality itself, which is rebuilt rather than moved
			}
			named, ok := condSides(src, aliases)
			if !ok {
				return nil, fmt.Sprintf("the ON term %q cannot be read as references to "+
					"named sides, so there is no step it can safely be moved to", src)
			}
			star.extra = append(star.extra, chainCond{src: src, sides: named})
		}
	}
	if why := star.readKeyClass(uf, spell, aliases); why != "" {
		return nil, why
	}
	return star, ""
}

// keyRef is one half of an equality: a table of the chain and the attribute of it that half
// reads. The attribute is held folded, because ClassAd attribute names are case-insensitive
// and two spellings of one name are one node of the graph.
type keyRef struct {
	side int
	attr string
}

// ufFind returns the representative of r's equivalence class, adding r as its own class the
// first time it is seen.
func ufFind(uf map[keyRef]keyRef, r keyRef) keyRef {
	parent, seen := uf[r]
	if !seen {
		uf[r] = r
		return r
	}
	if parent == r {
		return r
	}
	root := ufFind(uf, parent)
	uf[r] = root // path compression: the chains here are short, but this keeps them so
	return root
}

// ufUnion merges the classes of a and b, which is what one equality asserts.
func ufUnion(uf map[keyRef]keyRef, a, b keyRef) {
	ra, rb := ufFind(uf, a), ufFind(uf, b)
	if ra != rb {
		uf[ra] = rb
	}
}

// readKeyClass fills in star.attrs from the equality graph, or says why the graph is not one
// usable join key. See chainToStar for what the two refusals mean.
func (star *chainStar) readKeyClass(uf map[keyRef]keyRef, spell map[keyRef]string, aliases []string) string {
	// Sorted, not in map order. A chain with several keys has several pairs that could be
	// named as the ones that are not equal, and a message that names a different pair on
	// every run is one a reader cannot compare against anything.
	refs := make([]keyRef, 0, len(uf))
	for ref := range uf {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(a, b int) bool {
		if refs[a].side != refs[b].side {
			return refs[a].side < refs[b].side
		}
		return refs[a].attr < refs[b].attr
	})

	var root keyRef
	for i, ref := range refs {
		if i == 0 {
			root = ref
		} else if ufFind(uf, ref) != ufFind(uf, root) {
			return fmt.Sprintf("the chain joins on more than one key (%s.%s is not equal to "+
				"%s.%s through any of the conditions), so there is no attribute every table "+
				"could be re-attached by", aliases[ref.side], ref.attr, aliases[root.side],
				root.attr)
		}
		star.attrs[ref.side] = spell[ref]
	}
	for i, attr := range star.attrs {
		if attr == "" {
			return fmt.Sprintf("%s is joined to no other table by an equality, so every "+
				"order pairs it by the condition alone", aliases[i])
		}
	}
	return ""
}

// rowHalfRef reads a step's row half as exactly one attribute of one side.
func rowHalfRef(src string, aliases []string, side int) (keyRef, string, string) {
	if side == 1 {
		// At the first step the row holds the FROM side alone, so a bare half reads it --
		// which is what the two-table spelling writes.
		if bare := bareAttrName(src); bare != "" {
			return keyRef{side: 0, attr: strings.ToLower(bare)}, bare, ""
		}
	}
	q, err := vm.ParseAliases(src, aliases)
	if err != nil || !q.AliasNative() {
		return keyRef{}, "", fmt.Sprintf("the row half %q is not a plain reference to a "+
			"named side, so the chain cannot be re-centred on it", src)
	}
	refs := q.AliasRefs()
	if len(refs) != 1 || refs[0].Name == "" {
		return keyRef{}, "", fmt.Sprintf("the row half %q does not read exactly one "+
			"attribute of one table, so it is not a key the other tables could be joined "+
			"to", src)
	}
	return keyRef{side: refs[0].Alias, attr: strings.ToLower(refs[0].Name)}, refs[0].Name, ""
}

// condSides lists the sides an ON term reads.
//
// A term the alias compiler cannot take whole is refused rather than guessed at: AliasRefs
// only describes an AliasNative program, and a delegated subtree can reach a side in ways no
// list of names shows. Placing such a term by an incomplete list would put it at a step
// where something it reads is not folded in yet, which reads undefined and silently drops
// rows -- a wrong answer with no symptom, which is the one outcome worth refusing a valid
// plan to avoid.
func condSides(src string, aliases []string) ([]int, bool) {
	if strings.TrimSpace(src) == "" {
		return nil, true
	}
	q, err := vm.ParseAliases(src, aliases)
	if err != nil || !q.AliasNative() {
		return nil, false
	}
	seen := map[int]bool{}
	var out []int
	for _, r := range q.AliasRefs() {
		if !seen[r.Alias] {
			seen[r.Alias] = true
			out = append(out, r.Alias)
		}
	}
	sort.Ints(out)
	return out, true
}

// isEquality reports that src is exactly the equality the step's Equi already describes, in
// either order -- the term the rewrite regenerates rather than moves.
func isEquality(src string, aliases []string, row, table keyRef, firstStep bool) bool {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), "")) }
	lhs := norm(aliases[row.side] + "." + row.attr)
	rhs := norm(aliases[table.side] + "." + table.attr)
	got := norm(src)
	if got == lhs+"=="+rhs || got == rhs+"=="+lhs {
		return true
	}
	// At the first step the row half may be written bare, which is the two-table spelling.
	bare := norm(row.attr)
	return firstStep && row.side == 0 && (got == bare+"=="+rhs || got == rhs+"=="+bare)
}

// sideFacts is what one table's read costs, independent of the chain's shape.
type sideFacts struct {
	rows     int  // records reading it touches, whole table or constrained
	segments int  // segments a probe would do its lookups across
	indexed  bool // an index covers the attribute this side is joined on
}

// priceSides fills in star.facts, one store query per side.
func (cat *Catalog) priceSides(star *chainStar) bool {
	star.facts = make([]sideFacts, len(star.sides))
	for i, side := range star.sides {
		total, segments, indexed, _, ok := cat.joinSideStats(side.Table, star.attrs[i])
		if !ok {
			return false
		}
		rows, why := cat.sideRows(side, total)
		if why != "" {
			return false
		}
		star.facts[i] = sideFacts{rows: rows, segments: segments, indexed: indexed}
	}
	return true
}

// chainCost is what reading a chain FROM one of its tables costs.
type chainCost struct {
	total      int // records touched: the FROM side, plus every other table's read
	streamRows int // the FROM side's rows, which bound the keys its probes offer
	hashRows   int // rows held across every step, which is what MaxHashRows bounds
	fits       bool
}

// chainCostFrom prices reading the chain FROM side from.
//
// Every other table is priced at what an index probe would make its read, when one applies.
// That is the whole reason the candidates differ -- without it every FROM side reads the
// same records and the choice is a coin toss between equal numbers.
func (star *chainStar) chainCostFrom(from int) chainCost {
	streamRows := star.facts[from].rows
	out := chainCost{streamRows: streamRows, total: streamRows}
	for i := range star.sides {
		if i == from {
			continue
		}
		read := star.sideRead(i, streamRows)
		out.total += read
		out.hashRows += read
	}
	out.fits = out.hashRows <= DefaultJoinLimits().MaxHashRows
	return out
}

// sideRead is how many records one step's table would be read at, given a FROM side of
// streamRows rows to take probe keys from.
func (star *chainStar) sideRead(i, streamRows int) int {
	f := star.facts[i]
	if !f.indexed {
		return f.rows
	}
	// The FROM side's ROWS bound its distinct keys, so this overstates the probe and
	// understates its benefit -- the safe direction, since the gate that actually decides
	// errs towards reading.
	keys := min(streamRows, maxProbeKeys)
	if lookups := f.segments * keys * probeLookupCost; lookups < f.rows {
		return lookups
	}
	return f.rows
}

// spokeOrder returns the sides, other than from, in the order their steps should fold them
// in: smallest read first.
//
// A smaller table in a star join both matches fewer of the rows reaching it and drops more
// of them, so folding it first keeps the row count between the steps down. That is the
// smaller of the two effects this file is about -- the tables are all read either way -- and
// it is why the FROM side is decided first and this only orders what is left.
func (star *chainStar) spokeOrder(from int) []int {
	streamRows := star.facts[from].rows
	type spoke struct {
		side, read int
	}
	spokes := make([]spoke, 0, len(star.sides)-1)
	for i := range star.sides {
		if i == from {
			continue
		}
		spokes = append(spokes, spoke{side: i, read: star.sideRead(i, streamRows)})
	}
	sort.SliceStable(spokes, func(a, b int) bool { return spokes[a].read < spokes[b].read })
	out := make([]int, 0, len(spokes))
	for _, s := range spokes {
		out = append(out, s.side)
	}
	return out
}

// rewriteOrder rebuilds the spec with side from read FROM and the rest folded in the given
// order.
func (star *chainStar) rewriteOrder(spec JoinSpec, from int, order []int) JoinSpec {
	fromAlias, fromAttr := star.sides[from].Alias, star.attrs[from]
	// pos maps a side to its position in the new chain, which is what says at which step a
	// condition's references are all in scope.
	pos := map[int]int{from: 0}
	for p, side := range order {
		pos[side] = p + 1
	}
	steps := make([]JoinStep, len(order))
	for p, side := range order {
		steps[p] = JoinStep{
			Kind: JoinInner,
			Side: star.sides[side],
			Cond: []string{fmt.Sprintf("%s.%s == %s.%s", fromAlias, fromAttr,
				star.sides[side].Alias, star.attrs[side])},
			Equi: []JoinEqui{{
				Left:  fmt.Sprintf("%s.%s", fromAlias, fromAttr),
				Right: star.attrs[side],
			}},
		}
	}
	// Every other ON term goes to the earliest step by which every side it reads is folded
	// in. For an all-inner chain that is exact -- ON and WHERE are the same conjunctive
	// filter -- and it keeps the term filtering as early as it legally can, which is the
	// whole reason not to move them all to WHERE. A term reading only the FROM side has no
	// step of its own and goes on the first, where it filters each pair the same way.
	for _, c := range star.extra {
		at := 0
		for _, side := range c.sides {
			at = max(at, pos[side])
		}
		at = max(at, 1)
		steps[at-1].Cond = append(steps[at-1].Cond, c.src)
	}
	out := spec
	out.Kind = JoinInner
	out.Left = star.sides[from]
	out.Right = JoinSide{}
	out.Cond, out.Equi = nil, nil
	out.Steps = steps
	return out
}
