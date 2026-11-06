package db

// The index-based equi join: narrowing the hashed side's scan to the keys the other side
// actually holds, instead of reading it whole.
//
// The hash join reads one side entirely to build its buckets. That is the right shape when
// both sides are comparable in size, and the wrong one when a selective left side is being
// joined against a large table: nearly every bucket built is then discarded unprobed. An
// index-based equi join reads only the rows whose key some left row actually carries.
//
// The mechanism is the same membership constraint a client can build -- fold the left side's
// distinct key values into one OR-chain of equalities, which the query planner recognizes
// and resolves as a single multi-value index probe. What is different here is the DECISION.
// Whether narrowing pays depends on three numbers:
//
//   - whether an index covers the attribute at all. Without one the constraint is a filter
//     over a scan that still reads every row, so it costs more than it saves.
//   - how many records reading the hashed side VISITS, which bounds what the narrowing
//     avoids. That is the table's size only when the side is read whole; a side carrying a
//     constraint of its own reads less, and how much less is what joincost.go works out.
//   - how many distinct keys the other side has, which bounds what it costs.
//
// A client holds none of these. It can ask for a row count, which is a round trip and
// describes the whole table rather than the scan; it cannot see the index set at all, so it
// builds the constraint and hopes. The store has all three for free, which is why this lives
// here and not there.

import (
	"fmt"
	"iter"
	"sort"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// Probe tuning. All vars so a test can move them, and so a site whose data disagrees with
// the shapes these were measured on has somewhere to change them.
var (
	// probeLookupCost is how many scanned records one index lookup is worth.
	//
	// The naive model -- fewer rows read is faster -- gets the SIGN wrong, because the
	// probe's cost is not a function of the key count alone. A sealed segment carries its
	// own index, so an `in` probe of k values across s segments costs s*k lookups while the
	// scan it replaces costs r records.
	//
	// Swept at r=8192 over s=211 sealed segments, with the gate forced off so every key
	// count actually probed:
	//
	//	keys  lookups  probe     full scan
	//	  16    3,376   0.41 ms    9.62 ms   23.3x faster
	//	  32    6,752   0.86 ms    9.33 ms   10.9x faster
	//	  64   13,504  31.3  ms   10.42 ms    3.0x SLOWER
	//	 128   27,008  118.5 ms    9.50 ms   12.5x slower
	//	 256   54,016  466.1 ms   15.97 ms   29.2x slower
	//
	// So the crossover sits between 6,752 and 13,504 lookups against 8,192 records: one
	// lookup is worth about one scanned record, which is what this being 1 says. Comparing
	// s*k with r declines from 39 keys up, inside the measured interval.
	//
	// Two things that table is worth reading twice. The cost past the crossover grows about
	// 4x per doubling, so being wrong on this side is expensive and the gate should err
	// towards scanning. And an earlier draft used rows-per-key with a guessed threshold of
	// 8, which would have accepted all five rows of that table -- the 29x case included.
	probeLookupCost = 1

	// maxProbeKeys caps the distinct key values folded into one constraint. Past it the
	// expression costs more to build and evaluate than any scan it could save, whatever
	// the cost model says.
	maxProbeKeys = 1024

	// minProbeRows is the hashed side's size below which narrowing is not considered at
	// all. Reading a small table whole is cheap enough that the decision is not worth its
	// own cost, let alone the risk of getting it wrong.
	minProbeRows = 1024
)

// joinProbes is what the probe decision came to for a whole chain: the FROM side if it had to
// be held to reach the decision, and the membership constraint each step's table is narrowed
// by.
type joinProbes struct {
	// held is the FROM side's rows, materialized to count their keys. Non-nil means the
	// caller must stream the FROM side from here rather than from the source, so the scan
	// is not paid twice.
	held []*joinRow
	// rescan says the decision consumed part of the FROM side and then gave up, so the
	// source has to be resolved again.
	rescan bool
	// cons maps a step's index to the constraint its table is read with.
	cons map[int]string
}

// probeCandidate is one step that passed every gate a scan is not needed for.
type probeCandidate struct {
	step int
	// equi is which of the step's equalities this candidate would narrow on, and attr the
	// attribute of the step's own table that equality names.
	equi     int
	attr     string
	rows     int
	segments int
	lits     []string
	seen     map[string]bool
}

// planIndexProbes decides, for every step at once, whether to narrow that step's table scan
// to the key values the FROM side actually holds.
//
// WHY EVERY STEP AND NOT JUST THE FIRST. The keys a step would be narrowed to come from the
// accumulated row, and for a chain joined on one key -- which is the shape these tables are
// in -- every step's row half reads the FROM side alone. So ONE scan of the FROM side yields
// the key set for every step, and the second and third tables get an index-driven access path
// on the same terms the second always had. A client cannot do this for the third table
// without first materializing the join of the first two.
//
// A step whose row half reads a side an earlier step folds in is declined, and says so: its
// key set is not known until that step has run, and guessing it from the FROM side would
// narrow a scan on the strength of values that may not survive.
//
// It returns why it declined, per step, because a plan that silently did not use an index is
// indistinguishable from one that had none to use.
//
// The gates that need no scan come first -- one equality on a bare attribute, an index
// covering it, no constraint of the step's own, and a table big enough to be worth avoiding --
// because the scan that follows is a cost paid before the answer is known.
func (cat *Catalog) planIndexProbes(p *joinPlan, sides []JoinSide, scanSeq iter.Seq[*joinRow],
	stats *JoinStats) *joinProbes {
	out := &joinProbes{cons: map[int]string{}}
	stats.ProbeSteps = make([]JoinProbeStat, len(p.steps))
	var cands []*probeCandidate
	for i, sp := range p.steps {
		attrs, rows, segments, why := cat.probeGateAll(sp, sides[sp.side])
		// EstRecords is recorded even when the gate declines, because the decline that
		// reads on the size is the one a reader most needs the size for.
		stats.ProbeSteps[i] = JoinProbeStat{Alias: p.aliases[sp.side], Why: why, EstRecords: rows}
		if why != "" {
			continue
		}
		// One candidate per probeable equality. Which of them is worth narrowing on is
		// not knowable yet -- it turns on how many distinct keys the FROM side holds for
		// each, which the scan below counts -- so all of them collect keys and the choice
		// is made once the counts are in.
		stats.ProbeSteps[i].Attr = attrs[0].attr
		for _, pa := range attrs {
			cands = append(cands, &probeCandidate{
				step: i, equi: pa.equi, attr: pa.attr,
				rows: rows, segments: segments, seen: map[string]bool{},
			})
		}
	}
	if len(cands) == 0 {
		stats.syncFirstProbe()
		return out
	}

	// Now the part that costs a scan. The FROM side is held either way from here: if every
	// probe is declined below, it is handed back to be streamed from memory, so the scan is
	// not wasted.
	//
	// Holding it is a real cost, bounded by MaxHashRows -- the same bound the hashed sides
	// have -- so a join that takes this path holds one more bounded side than it otherwise
	// would. That is the price of the decision, and it is why every gate that can be
	// checked without reading a row is checked above.
	held := make([]*joinRow, 0, 256)
	rows := make([]*joinRow, p.nSides())
	for ad := range scanSeq {
		held = append(held, ad)
		if len(held) > p.limits.MaxHashRows {
			why := fmt.Sprintf("the FROM side holds more than %d rows, so holding it to "+
				"collect its keys would cost what hashing the other side costs",
				p.limits.MaxHashRows)
			for _, c := range cands {
				stats.ProbeSteps[c.step].Why = why
			}
			stats.syncFirstProbe()
			out.rescan = true
			return out
		}
		rows[0] = ad
		for _, c := range cands {
			if len(c.lits) > maxProbeKeys {
				continue // already past the cap; keep reading so the held side stays complete
			}
			sp := p.steps[c.step]
			v, ok := p.rowHalfValueAt(sp, rows, c.equi)
			if !ok {
				continue
			}
			lit, ok := joinKeyLiteral(v)
			if !ok {
				// No usable key value: this row matches nothing through the equality, so
				// it contributes no term -- and must not narrow the table on its behalf.
				continue
			}
			if !c.seen[lit] {
				c.seen[lit] = true
				c.lits = append(c.lits, lit)
			}
		}
	}
	out.held = held

	// One constraint per STEP, conjoining every equality that collected keys.
	//
	// All of them rather than the best one, because which is "best" is not knowable here.
	// A constraint's selectivity is its key count over the attribute's distinct values IN
	// THE HASHED TABLE, and that second number lives in the segment statistics, which this
	// layer cannot see. Choosing on key count alone gets the common case backwards: a join
	// on (ClusterId, ProcId) may offer fifty ClusterIds and four ProcIds, and the four
	// would win while matching every row in the table.
	//
	// Conjoining them hands the choice to the query planner, which does have those
	// statistics and already orders conjuncts by estimated selectivity before intersecting
	// their postings. A row matching all the equalities matches each of them, so the
	// conjunction is as sound a superset filter as any one of them, and strictly narrower.
	for _, g := range groupByStep(cands) {
		st := &stats.ProbeSteps[g[0].step]
		keys, lookups := 0, 0
		for _, c := range g {
			keys += len(c.lits)
			lookups += c.segments * len(c.lits)
		}
		rows := g[0].rows
		switch {
		case keys == 0:
			st.Why = "no row of the FROM side has a usable key value"
			continue
		case tooManyKeys(g):
			st.Why = fmt.Sprintf("the FROM side has more than %d distinct keys", maxProbeKeys)
			continue
		case lookups*probeLookupCost > rows:
			st.Why = fmt.Sprintf("%d keys across %d segments is %d index lookups against a "+
				"read of %d records, which costs more than it saves", keys, g[0].segments,
				lookups, rows)
			continue
		}
		parts := make([]string, 0, len(g))
		attrs := make([]string, 0, len(g))
		for _, c := range g {
			sort.Strings(c.lits) // so the same join builds the same constraint
			terms := make([]string, len(c.lits))
			for i, l := range c.lits {
				terms[i] = c.attr + " == " + l
			}
			parts = append(parts, "("+strings.Join(terms, " || ")+")")
			attrs = append(attrs, c.attr)
		}
		out.cons[g[0].step] = strings.Join(parts, " && ")
		st.Attr = strings.Join(attrs, ", ")
		st.Keys = keys
	}
	stats.syncFirstProbe()
	return out
}

// probeGate applies every test that can be made without reading a row, and names the attribute
// a constraint would be built on when they all pass.
// probeGateAll applies every test that can be made without reading a row and returns EVERY
// equality of the step a membership constraint could be built on.
//
// More than one is useful because a constraint narrows on ONE equality and the choice
// matters. Narrowing on any of them is sound -- a row matching all the equalities matches
// each of them, so a constraint built from one is a superset filter, and the hash key is
// still built from all of them while the ON condition re-checks every pair. But which one
// is worth narrowing on differs enormously: a join on a composite key like
// (ClusterId, ProcId) may see fifty distinct ClusterIds and five thousand ProcIds from the
// same FROM side, and narrowing on the second would replace a scan with more index lookups
// than the scan reads records. So the gate names the candidates and the scan, which counts
// each one's distinct keys anyway, picks between them.
//
// This is also why a step is no longer refused outright for having several equalities. That
// refusal cost the probe on exactly the shape it is most wanted for: a two-attribute key is
// how jobs are identified, so the join that most wants an index was the one declining it.
func (cat *Catalog) probeGateAll(sp *joinStepPlan, side JoinSide) (out []probeAttr, rows, segments int, why string) {
	if len(sp.equiSide) == 0 {
		return nil, 0, 0, "the condition has no equality to probe an index with"
	}
	var names []string
	for i, ex := range sp.equiSide {
		if i < len(sp.rowOnSide0) && !sp.rowOnSide0[i] {
			continue // its key values are not known from a scan of the FROM side
		}
		if a, ok := bareAttr(ex); ok {
			out = append(out, probeAttr{equi: i, attr: a})
			names = append(names, a)
		}
	}
	if len(out) == 0 {
		if !sp.rowReadsOnlySide0 {
			return nil, 0, 0, "the equality's row half reads a side an earlier step folds " +
				"in, so its key values are not known from a scan of the FROM side"
		}
		return nil, 0, 0, "the equality's table half is computed, so there is no attribute to name"
	}
	// Every candidate reads the same table, so its size and segment count are asked once.
	// Which attributes are INDEXED is per attribute, and is what the filter below is for.
	rows, segments, _, _, ok := cat.joinSideStats(side.Table, "")
	if !ok {
		return nil, 0, 0, "the hashed side's size could not be learned"
	}
	kept := out[:0]
	var configuredOnly []string
	for _, pa := range out {
		_, _, indexed, configured, statsOK := cat.joinSideStats(side.Table, pa.attr)
		switch {
		case statsOK && indexed:
			kept = append(kept, pa)
		case configured:
			configuredOnly = append(configuredOnly, pa.attr)
		}
	}
	out = kept
	if len(out) == 0 {
		return nil, 0, 0, noIndexWhy(side.Alias, names, configuredOnly)
	}
	// A side with a constraint of its own does not read the whole table, so the table's size
	// is not what a membership constraint would save. Price the read it actually does; the
	// alternative, and what this did until the store could be asked, is to decline every
	// constrained side -- which is most of them, since a history join narrows by owner or
	// date far more often than not.
	rows, why = cat.sideRows(side, rows)
	if why != "" {
		return nil, 0, 0, why
	}
	if rows < minProbeRows {
		// rows is returned with the decline: it is the whole reason for it.
		return nil, rows, segments, fmt.Sprintf("the hashed side's read visits %d records, "+
			"below the %d where narrowing is worth deciding", rows, minProbeRows)
	}
	return out, rows, segments, ""
}

// probeAttr is one equality of a step that a membership constraint could narrow on: which
// equality it is, and the attribute of the step's own table it names.
type probeAttr struct {
	equi int
	attr string
}

// noIndexWhy explains that no candidate attribute is indexed.
//
// Configured-but-not-built is reported as its own case, because it is the one a reader is
// most likely to mistake for a working probe: the table says it indexes the attribute, and
// a constraint naming it still parses and still filters -- over a full scan, at a cost that
// grows with the key count.
func noIndexWhy(alias string, names, configured []string) string {
	if len(configured) > 0 {
		return fmt.Sprintf("%s.%s is configured for an index but none is built yet (postings "+
			"appear as segments seal), so a membership constraint would filter a full scan "+
			"rather than replace it", alias, configured[0])
	}
	if len(names) == 1 {
		return fmt.Sprintf("no index covers %s.%s, so a membership constraint would filter a "+
			"full scan rather than replace it", alias, names[0])
	}
	return fmt.Sprintf("no index covers any of %s (%s), so a membership constraint would "+
		"filter a full scan rather than replace it", alias, strings.Join(names, ", "))
}

// rowHalfValueAt evaluates the accumulated row's half of the step's k'th equality, which is
// what a probe narrowing on that equality would restrict the step's table to.
func (p *joinPlan) rowHalfValueAt(sp *joinStepPlan, rows []*joinRow, k int) (classad.Value, bool) {
	if k < 0 || k >= len(sp.equiRow) {
		return classad.Value{}, false
	}
	// The same bare-attribute shortcut the hash key uses: the probe reads one half of every
	// scanned row, so decoding each one to read one attribute is the cost this scan exists
	// to avoid.
	if name := sp.equiRowAttr[k]; name != "" {
		if rows[0] == nil {
			return classad.Value{}, false
		}
		return rows[0].eval(name), true
	}
	if al := sp.rowAliased[k]; al != nil {
		if v, ok := al.eval(rows); ok {
			return v, true
		}
	}
	return sp.equiRow[k].Eval(p.rowHalfScope(sp, rows)), true
}

// joinSideStats reports what the decision needs about the side about to be hashed: how many
// rows it holds, how many segments it is spread across, and whether an index covers attr.
//
// The segment count is there because the probe's cost depends on it and nothing else
// reports it. A sealed segment carries its own index, so an `in` probe of k values across s
// segments is s*k lookups, while the scan it replaces is r records.
//
// "Indexed" means configured AND built. Postings for a segment appear when that segment
// seals, so a table can carry the configuration with nothing indexed yet -- in which case
// the membership constraint is a filter over a full scan, which costs more than the scan it
// was meant to replace.
func (cat *Catalog) joinSideStats(table, attr string) (rows, segments int, indexed, configured, ok bool) {
	covers := func(names ...[]string) bool {
		for _, set := range names {
			for _, name := range set {
				if strings.EqualFold(name, attr) {
					return true
				}
			}
		}
		return false
	}
	if d, found := cat.Table(table); found {
		c, v := d.IndexedAttrs()
		cfg := covers(c, v)
		return d.Len(), d.Stats().Segments, cfg, cfg, true
	}
	if a, found := cat.ArchiveTable(table); found {
		c, v := a.IndexedAttrs()
		cfg := covers(c, v)
		// A sealed segment's index is in its sidecar; with none sealed there is nothing to
		// probe, whatever the configuration says. Asked through HasSealedSidecar rather
		// than SidecarSizes, which parses every sidecar to break its bytes out and is a
		// diagnostic rather than something a query path may call.
		return a.Count(), a.Stats().Segments, cfg && a.HasSealedSidecar(), cfg, true
	}
	// A view's two halves have separate indexes and separate sizes; narrowing one of them
	// is a decision this does not try to make.
	return 0, 0, false, false, false
}

// bareAttr reports the attribute name of an expression that is nothing but an unscoped
// attribute reference. Anything else -- a computed half, a scoped one, a nested select --
// has no name a constraint on one table could use.
func bareAttr(ex *classad.Expr) (string, bool) {
	if ex == nil {
		return "", false
	}
	ar, isRef := ex.AST().(*ast.AttributeReference)
	if !isRef || ar.Scope != ast.NoScope || ar.Name == "" {
		return "", false
	}
	return ar.Name, true
}

// joinKeyLiteral renders a key value as a ClassAd literal, or reports that it cannot be one.
// Only the scalar kinds the hash key already normalizes can appear in an equality term.
func joinKeyLiteral(v classad.Value) (string, bool) {
	switch {
	case v.IsBool():
		b, err := v.BoolValue()
		if err != nil {
			return "", false
		}
		return strconv.FormatBool(b), true
	case v.IsInteger():
		n, err := v.IntValue()
		if err != nil {
			return "", false
		}
		return strconv.FormatInt(n, 10), true
	case v.IsReal():
		f, err := v.RealValue()
		if err != nil {
			return "", false
		}
		return strconv.FormatFloat(f, 'g', -1, 64), true
	case v.IsString():
		s, err := v.StringValue()
		if err != nil {
			return "", false
		}
		return quoteClassAdString(s), true
	}
	return "", false
}

// quoteClassAdString renders a Go string as a ClassAd string literal.
func quoteClassAdString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// groupByStep gathers the per-equality candidates by the step they narrow, keeping both the
// steps and the equalities within a step in the order they were written, so the same join
// builds the same constraint twice.
func groupByStep(cands []*probeCandidate) [][]*probeCandidate {
	byStep := map[int][]*probeCandidate{}
	order := make([]int, 0, len(cands))
	for _, c := range cands {
		if _, seen := byStep[c.step]; !seen {
			order = append(order, c.step)
		}
		byStep[c.step] = append(byStep[c.step], c)
	}
	out := make([][]*probeCandidate, 0, len(order))
	for _, step := range order {
		out = append(out, byStep[step])
	}
	return out
}

// tooManyKeys reports that some equality of the step blew the per-constraint key cap. Per
// equality rather than on the total: each is its own OR-chain, and the cap is on how long
// one of those may get before building and evaluating it costs more than any scan it saves.
func tooManyKeys(g []*probeCandidate) bool {
	for _, c := range g {
		if len(c.lits) > maxProbeKeys {
			return true
		}
	}
	return false
}
