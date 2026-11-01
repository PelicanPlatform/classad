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

// joinProbe is the decision to narrow the hashed side, and what it was based on.
type joinProbe struct {
	attr string // the bare attribute the constraint names
	rows int    // rows the hashed side holds in total
	keys int    // distinct key values the streamed side turned out to hold
	held []*classad.ClassAd
	cons string // the membership constraint
}

// planIndexProbe decides whether to narrow the hashed side's scan, and if so materializes
// the streamed side and builds the constraint.
//
// It returns why not when it declines, because a plan that silently did not use an index is
// indistinguishable from one that had none to use.
//
// The streamed side has to be materialized to count its keys exactly, which is a cost this
// pays before it knows the answer. So the gates that need no scan come first: one equality
// on a bare attribute, an index covering it, no constraint of the hashed side's own, and a
// hashed side big enough to be worth avoiding. Only then is the other side read.
func (cat *Catalog) planIndexProbe(p *joinPlan, spec JoinSpec, scanSeq iter.Seq[*classad.ClassAd]) (*joinProbe, string) {
	if len(p.equiLeft) != 1 {
		if len(p.equiLeft) == 0 {
			return nil, "the condition has no equality to probe an index with"
		}
		return nil, fmt.Sprintf("the condition has %d equality terms and a membership "+
			"constraint can narrow on only one", len(p.equiLeft))
	}
	attr, ok := bareAttr(p.equiRight[0])
	if !ok {
		return nil, "the equality's hashed half is computed, so there is no attribute to name"
	}
	if strings.TrimSpace(spec.Right.Constraint) != "" {
		// Its own constraint already narrows it, so the table's row count overstates what
		// the scan reads and the trade cannot be priced from it.
		return nil, "the hashed side carries its own constraint, which the row count does not describe"
	}
	rows, segments, indexed, configured, ok := cat.joinSideStats(spec.Right.Table, attr)
	if !ok {
		return nil, "the hashed side's size could not be learned"
	}
	if !indexed {
		// Configured-but-not-built is reported as its own case, because it is the one a
		// reader is most likely to mistake for a working probe: the table says it indexes
		// the attribute, and a constraint naming it still parses and still filters -- over
		// a full scan, at a cost that grows with the key count.
		if configured {
			return nil, fmt.Sprintf("%s.%s is configured for an index but none is built "+
				"yet (postings appear as segments seal), so a membership constraint would "+
				"filter a full scan rather than replace it", spec.Right.Alias, attr)
		}
		return nil, fmt.Sprintf("no index covers %s.%s, so a membership constraint would "+
			"filter a full scan rather than replace it", spec.Right.Alias, attr)
	}
	if rows < minProbeRows {
		return nil, fmt.Sprintf("the hashed side holds %d rows, below the %d where "+
			"narrowing is worth deciding", rows, minProbeRows)
	}

	// Now the part that costs a scan. The streamed side is held either way from here: if the
	// probe is declined below, it is handed back to be streamed from memory, so the scan is
	// not wasted.
	//
	// Holding it is a real cost, and the one it is bounded by is MaxHashRows -- the same
	// bound the hashed side has -- so a join that takes this path can hold two bounded
	// sides rather than one. That is the price of the decision, and it is why every gate
	// that can be checked without reading a row is checked above.
	held := make([]*classad.ClassAd, 0, 256)
	seen := make(map[string]bool, 256)
	var lits []string
	for ad := range scanSeq {
		held = append(held, ad)
		if len(held) > p.limits.MaxHashRows {
			return nil, fmt.Sprintf("the streamed side holds more than %d rows, so holding "+
				"it to collect its keys would cost what hashing the other side costs",
				p.limits.MaxHashRows)
		}
		if len(lits) > maxProbeKeys {
			continue // already past the cap; keep reading so the held side stays complete
		}
		lit, ok := joinKeyLiteral(p.equiLeft[0].Eval(ad))
		if !ok {
			// No usable key value: this row matches nothing through the equality, so it
			// contributes no term -- and must not narrow the other side on its behalf.
			continue
		}
		if !seen[lit] {
			seen[lit] = true
			lits = append(lits, lit)
		}
	}
	probe := &joinProbe{attr: attr, rows: rows, keys: len(lits), held: held}
	lookups := segments * len(lits)
	switch {
	case len(lits) == 0:
		return probe, "no row of the streamed side has a usable key value"
	case len(lits) > maxProbeKeys:
		return probe, fmt.Sprintf("the streamed side has more than %d distinct keys", maxProbeKeys)
	case lookups*probeLookupCost > rows:
		return probe, fmt.Sprintf("%d keys across %d segments is %d index lookups against a "+
			"%d-row scan, which costs more than it saves", len(lits), segments, lookups, rows)
	}
	sort.Strings(lits) // so the same join builds the same constraint
	terms := make([]string, len(lits))
	for i, l := range lits {
		terms[i] = attr + " == " + l
	}
	probe.cons = "(" + strings.Join(terms, " || ") + ")"
	return probe, ""
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
