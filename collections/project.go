package collections

import (
	"iter"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// QueryProject is Query specialized for aggregation and projection: for each ad
// matching q it yields just the named attributes' values, read wire-native
// (straight from the encoded ad, no *classad.ClassAd built) whenever they are
// scalar literals -- the common case for the numeric/string attributes an
// aggregate groups or sums by. Only an ad that stores one of the requested
// attributes as a non-literal expression falls back to a full decode, and only
// that ad.
//
// This avoids the dominant cost of a GROUP BY / aggregate over Query, which
// fully decodes every matching ad (tens of allocations each) just to read one or
// two attributes.
//
// The yielded slice is reused across iterations and aligned with attrs; the
// consumer must copy any value it needs to retain past the next step (the
// aggregate reads each value into its group state immediately).
func (c *Collection) QueryProject(q *vm.Query, attrs []string) iter.Seq[[]classad.Value] {
	return c.QueryProjectStats(q, attrs, nil)
}

// QueryProjectStats is QueryProject that also fills stats (may be nil) with the scan's work
// breakdown -- how many segments were pruned by zone maps vs scanned, and how many records were
// answered from columns vs reassembled from the arena. It exists so an EXPLAIN ANALYZE of an
// aggregate that falls to this projected scan (an attribute the columnar accelerator does not cover
// numerically) can show WHERE the time went -- specifically a large RecordsReassembled, the slow
// per-record path. Stats accumulate as the returned sequence is consumed.
func (c *Collection) QueryProjectStats(q *vm.Query, attrs []string, stats *ScanStats) iter.Seq[[]classad.Value] {
	return c.queryProjectAs(q, attrs, stats, false)
}

// QueryProjectRedacted is QueryProject for a reader NOT entitled to sealed values: q is matched and attrs
// are read with no key, so a sealed value is undefined in both. Every aggregate and top-K over this scan
// is therefore an answer about what the reader may see, not about the plaintext.
func (c *Collection) QueryProjectRedacted(q *vm.Query, attrs []string) iter.Seq[[]classad.Value] {
	return c.queryProjectAs(q, attrs, nil, true)
}

// QueryProjectStatsRedacted is QueryProjectStats with QueryProjectRedacted's entitlement.
func (c *Collection) QueryProjectStatsRedacted(q *vm.Query, attrs []string, stats *ScanStats) iter.Seq[[]classad.Value] {
	return c.queryProjectAs(q, attrs, stats, true)
}

func (c *Collection) queryProjectAs(q *vm.Query, attrs []string, stats *ScanStats, redact bool) iter.Seq[[]classad.Value] {
	return func(yield func([]classad.Value) bool) {
		// Projection demand: every record this scan yields has each of attrs read out of
		// it. Recorded before the chained fallback, which returns through Query -- and
		// Query knows only the constraint, not these. The constraint's own demand comes
		// from newQueryPlan, on either branch.
		c.demand.recordReads(attrs)
		// Chained collections need parent attributes merged in; fall back to the
		// full-decode path (correctness over speed for the rarer chained case).
		if c.parentKeyFor != nil {
			for ad := range c.queryAs(q, redact) {
				vals := make([]classad.Value, len(attrs))
				for i, n := range attrs {
					vals[i] = ad.EvaluateAttr(n)
				}
				if !yield(vals) {
					return
				}
			}
			return
		}

		scratch := make([]classad.Value, len(attrs))
		rs := &wireScope{ctx: c.evalCtx(redact)} // projection resolver, distinct from the match ws
		// A CLOSURE PLAN, not a plain one, and the difference is the whole correctness story here.
		//
		// A projection plan lets the scan hand this emit an ad NARROWED to attrs, built from a
		// columnarized segment's columns instead of reassembling the record. That is sufficient for
		// a consumer that SHIPS the entries it is given. This one EVALUATES them: projectInto
		// decodes the ad whenever a requested attribute is stored as an expression rather than a
		// literal, and a PLAIN plan narrows away exactly the references such an expression needs --
		// `RequestMemory = ProcId*512+7` projected as just RequestMemory loses ProcId and evaluates
		// to UNDEFINED. Measured on a columnarized fixture: 14 of 3000 records turned UNDEFINED,
		// with no error anywhere. This scan backs COUNT and GROUP BY, where UNDEFINED is not an
		// error the caller sees but a different answer, so that is not a trade to make.
		//
		// Marking the plan a closure plan removes the hazard rather than avoiding it: the scan
		// keeps the narrowed ad only for a record where every reference its projected expressions
		// reach is in it (adClosed), and reassembles the rest. So the fast path runs exactly where
		// it reads the same as the whole record, and the 14 records above reassemble.
		//
		// Measured on that fixture (BenchmarkQueryProjectPlan): 1.58x and half the allocations,
		// not the 3x a plan without the closure check would promise -- the difference is the
		// records that fall back plus the projected attributes no schema covers, which are in the
		// record either way. The speedup is a property of the projection, not a constant.
		//
		// Pinned by TestQueryProjectExpressionValues, which compares every value against the
		// full-decode path, and by TestProjectedAdIsNotEvaluable, which holds the plain plan to
		// still losing values -- that is what makes this a closure test rather than a plan test.
		proj := c.newProjPlan(attrs)
		if proj != nil {
			proj.closure = true
			// This consumer never sees the entries, only the values they evaluate to, which is
			// what lets the scan serve an expression-valued cell from its precomputed literal.
			proj.evaluates = true
		}
		// A redacted read withholds private attributes BY NAME, resolved once here rather than
		// per record. Leaving this to the sealed-value path is not enough, and the gap is in the
		// STORE rather than in this scan: where a sealer exists, a private attribute is sealed as
		// it is written (shouldEncrypt) and the keyless context finds UNDEFINED -- but a store
		// that seals nothing has no keyless context at all (evalCtx returns the collection), so
		// the plaintext it wrote is simply read back. Measured before this was added: the same
		// projection of ClaimId over one such archive returned the plaintext in every row under
		// redact, while the RAW projection of the same records over the same scan dropped it,
		// because that path filters names (rawProjector) instead of relying on sealing.
		//
		// It is also a different set from the one keylessHides covers. That set is the sealing
		// POLICY's, withheld because a value written before the policy named it can still sit in
		// the clear; this one is the private set, which is private whether or not anything is
		// sealed here.
		//
		// So the entitlement a caller asks for here is now the entitlement it gets on either table
		// kind, which is the whole point of the Redacted spelling. The constraint is a separate
		// question with a separate answer: evaluating a private attribute is an oracle even when
		// its value is never returned, and the RPC layer refuses such a constraint by name before
		// the scan starts.
		var private []bool
		if redact {
			for i, n := range attrs {
				if classad.IsPrivateAttribute(n) {
					if private == nil {
						private = make([]bool, len(attrs))
					}
					private[i] = true
				}
			}
		}
		c.scanQuery(c.newQueryPlan(q, proj, stats, redact), func(w []byte, dict *segDictHandle) bool {
			rs.dict = dict
			c.projectInto(rs, w, attrs, scratch)
			for i := range private {
				if private[i] {
					scratch[i] = classad.NewUndefinedValue()
				}
			}
			return yield(scratch)
		})
	}
}

// projectInto fills out (len == len(attrs)) with each attribute's value read from
// the matching ad's wire bytes w, reading wire-native and falling back to a full
// decode of this one ad only if any requested attribute is a non-literal
// expression.
func (c *Collection) projectInto(rs *wireScope, w []byte, attrs []string, out []classad.Value) {
	ad := wire.Ad(w)
	rs.ad = ad
	rs.fellBack = false
	needDecode := false
	for i, name := range attrs {
		v, found := rs.tryResolve(ad, rs.dict, name)
		if rs.fellBack {
			needDecode = true
			break
		}
		if !found {
			out[i] = classad.NewUndefinedValue()
		} else {
			out[i] = v
		}
	}
	if needDecode {
		// Through the record's segment dictionary, in the scan's context: an interned record is keyed by
		// that segment's local ids, which the collection-wide decode cannot read -- it failed, and every
		// attribute of the row came back ERROR -- and a sealed value is opened with the key for an
		// entitled read, as it is in an inline segment, and is undefined for a redacted one.
		if a, err := rs.ctx.decodeWireDict(rs.dict, w); err == nil {
			full := classad.FromAST(a)
			for i, name := range attrs {
				out[i] = full.EvaluateAttr(name)
			}
		} else {
			for i := range out {
				out[i] = classad.NewErrorValue()
			}
		}
	}
}
