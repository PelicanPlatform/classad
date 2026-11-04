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
//   - how many rows the hashed side holds, which bounds what the narrowing avoids.
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
	step     int
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
		attr, rows, segments, why := cat.probeGate(sp, sides[sp.side])
		stats.ProbeSteps[i] = JoinProbeStat{Alias: p.aliases[sp.side], Why: why}
		if why != "" {
			continue
		}
		stats.ProbeSteps[i].Attr = attr
		cands = append(cands, &probeCandidate{
			step: i, attr: attr, rows: rows, segments: segments, seen: map[string]bool{},
		})
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
			v, ok := p.rowHalfValue(sp, rows)
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

	for _, c := range cands {
		lookups := c.segments * len(c.lits)
		st := &stats.ProbeSteps[c.step]
		switch {
		case len(c.lits) == 0:
			st.Why = "no row of the FROM side has a usable key value"
			continue
		case len(c.lits) > maxProbeKeys:
			st.Why = fmt.Sprintf("the FROM side has more than %d distinct keys", maxProbeKeys)
			continue
		case lookups*probeLookupCost > c.rows:
			st.Why = fmt.Sprintf("%d keys across %d segments is %d index lookups against a "+
				"%d-row scan, which costs more than it saves", len(c.lits), c.segments, lookups, c.rows)
			continue
		}
		sort.Strings(c.lits) // so the same join builds the same constraint
		terms := make([]string, len(c.lits))
		for i, l := range c.lits {
			terms[i] = c.attr + " == " + l
		}
		out.cons[c.step] = "(" + strings.Join(terms, " || ") + ")"
		st.Keys = len(c.lits)
	}
	stats.syncFirstProbe()
	return out
}

// probeGate applies every test that can be made without reading a row, and names the attribute
// a constraint would be built on when they all pass.
func (cat *Catalog) probeGate(sp *joinStepPlan, side JoinSide) (attr string, rows, segments int, why string) {
	if len(sp.equiSide) != 1 {
		if len(sp.equiSide) == 0 {
			return "", 0, 0, "the condition has no equality to probe an index with"
		}
		return "", 0, 0, fmt.Sprintf("the condition has %d equality terms and a membership "+
			"constraint can narrow on only one", len(sp.equiSide))
	}
	if !sp.rowReadsOnlySide0 {
		return "", 0, 0, "the equality's row half reads a side an earlier step folds in, so " +
			"its key values are not known from a scan of the FROM side"
	}
	attr, ok := bareAttr(sp.equiSide[0])
	if !ok {
		return "", 0, 0, "the equality's table half is computed, so there is no attribute to name"
	}
	if strings.TrimSpace(side.Constraint) != "" {
		// Its own constraint already narrows it, so the table's row count overstates what
		// the scan reads and the trade cannot be priced from it.
		return "", 0, 0, "the hashed side carries its own constraint, which the row count does not describe"
	}
	rows, segments, indexed, configured, ok := cat.joinSideStats(side.Table, attr)
	if !ok {
		return "", 0, 0, "the hashed side's size could not be learned"
	}
	if !indexed {
		// Configured-but-not-built is reported as its own case, because it is the one a
		// reader is most likely to mistake for a working probe: the table says it indexes
		// the attribute, and a constraint naming it still parses and still filters -- over
		// a full scan, at a cost that grows with the key count.
		if configured {
			return "", 0, 0, fmt.Sprintf("%s.%s is configured for an index but none is built "+
				"yet (postings appear as segments seal), so a membership constraint would "+
				"filter a full scan rather than replace it", side.Alias, attr)
		}
		return "", 0, 0, fmt.Sprintf("no index covers %s.%s, so a membership constraint would "+
			"filter a full scan rather than replace it", side.Alias, attr)
	}
	if rows < minProbeRows {
		return "", 0, 0, fmt.Sprintf("the hashed side holds %d rows, below the %d where "+
			"narrowing is worth deciding", rows, minProbeRows)
	}
	return attr, rows, segments, ""
}

// rowHalfValue evaluates the accumulated row's half of a step's single equality, which is
// what a probe narrows the step's table to.
func (p *joinPlan) rowHalfValue(sp *joinStepPlan, rows []*joinRow) (classad.Value, bool) {
	if len(sp.equiRow) != 1 {
		return classad.Value{}, false
	}
	// The same bare-attribute shortcut the hash key uses: the probe reads one half of every
	// scanned row, so decoding each one to read one attribute is the cost this scan exists
	// to avoid.
	if name := sp.equiRowAttr[0]; name != "" {
		if rows[0] == nil {
			return classad.Value{}, false
		}
		return rows[0].eval(name), true
	}
	if al := sp.rowAliased[0]; al != nil {
		if v, ok := al.eval(rows); ok {
			return v, true
		}
	}
	return sp.equiRow[0].Eval(p.rowHalfScope(sp, rows)), true
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
		// probe, whatever the configuration says.
		return a.Count(), a.Stats().Segments, cfg && a.SidecarSizes().Segments > 0, cfg, true
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

// adsSeq streams a materialized side.
func adsSeq(ads []*classad.ClassAd) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		for _, ad := range ads {
			if !yield(ad) {
				return
			}
		}
	}
}
