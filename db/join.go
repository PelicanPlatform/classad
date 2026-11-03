package db

// Joining two tables inside the store.
//
// A joined row is a ClassAd whose members are the two sides under their aliases, so `ON`,
// the residual `WHERE` and everything a caller writes afterwards are ordinary ClassAd
// expressions over one ad. Nothing here teaches the evaluator a new rule: `h.ClusterId` is
// the select operator over a nested ad, and an unmatched LEFT row simply has no member for
// the right side, which is ClassAd's own spelling of SQL's NULL.
//
// WHEN IT IS WORTH DOING HERE rather than above the store, which is NOT always, and the
// caller is the one that decides. A joined row carries both sides, so a join that EXPANDS
// ships more from here than its inputs were: the left ad is repeated once per output row.
// Measured over a real connection, a two-table chain that expands puts 1.86x the bytes on
// the wire that a client pairing the same rows does, and a three-table chain 2.30x; a chain
// that filters puts far fewer. The direction is set by the expansion ratio -- output rows
// over input rows -- and nothing here estimates it, so a caller that cannot estimate it
// either should not assume this is the cheaper place. The measurement is in dbrpc, under
// TestNwayWireVolumeGrowsWithTheChain.
//
// Nor does a client have to fetch both sides whole: it can narrow the second side's read to
// the key values the first side turned out to hold, which is the same trick planIndexProbe
// plays here. What a client cannot do is decide well whether that narrowing pays, because
// the statistics it needs are on this side.
//
// What is unconditional is the materialization. The hash is built on one side and the other
// is streamed against it, so one side is held at a time; a client join holds the rows it has
// accumulated AND the side it is folding into them.
//
// WHAT THIS IS NOT, yet. The caller supplies the plan -- which conjuncts went to which
// side, which equalities drive the hash -- because that splitting is settled, tested and
// identical wherever it runs, and re-deriving it here would be a second implementation of
// the same rules. Block-level zone skipping, which is the thing a client genuinely cannot
// do, is not here either: it belongs with the columnar machinery and arrives separately.
// What this buys is the wire and the single-side materialization.
//
// AND IT JOINS EXACTLY TWO TABLES, on purpose. A chain of three or more is left-deep -- this
// join run n-1 times, each step folding in one more table -- so an n-ary opcode would be a
// generalization rather than a second algorithm, and it is still not built. The measurement
// above sharpens with every table added: an expanding chain's overshipping grows (1.86x at
// two tables, 2.30x at three), and the chain that WOULD gain -- one that filters -- gains
// more from being started at its selective table than from being moved here at all. On that
// fixture a client that orders its chain ships 13,351 bytes where a three-way join in the
// store ships 30,679. Join ORDER is the lever that case wants; join LOCATION is not.

import (
	"fmt"
	"iter"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// JoinKind is which rows survive a join with no match.
type JoinKind uint8

const (
	// JoinInner keeps only paired rows.
	JoinInner JoinKind = iota
	// JoinLeft keeps every left row, null-extending the ones that paired with nothing.
	JoinLeft
	// JoinCross pairs every row with every row; its condition, if any, decides.
	JoinCross
)

// JoinSide is one side of a join: which table, under what alias, and what to push into its
// own scan.
type JoinSide struct {
	Table string
	// Alias is the member name this side takes in the joined row. It is what the caller's
	// expressions reference, so it must match what they were written against.
	Alias string
	// Constraint is the predicate pushed into this side's scan; "" matches everything.
	Constraint string
	// Project names the attributes this side contributes to the joined row. Nil carries the
	// whole ad.
	//
	// This is the output projection, and it is the one that matters: the rows do not cross
	// a wire until they are joined, so what bounds the result is how much of each paired ad
	// is carried, not how much was read.
	Project []string
}

// JoinEqui is one `<left expr> == <right expr>` conjunct of the condition, each half written
// against its OWN side's ad rather than the joined row. They drive the hash.
//
// They narrow candidates only. Every surviving pair is still checked against the full
// condition, so the hash never has to reproduce ClassAd's equality semantics exactly -- it
// only has to avoid putting two rows in different buckets when they would compare equal.
type JoinEqui struct {
	Left, Right string
}

// JoinSpec is a planned two-table join.
type JoinSpec struct {
	Kind        JoinKind
	Left, Right JoinSide

	// Cond are the ON conjuncts that reference both sides -- the join condition proper --
	// as ClassAd expression text over the joined row. A pair matches when every one is true.
	Cond []string
	// Post are the WHERE conjuncts over the joined row, applied after the pair is formed
	// and, for a LEFT join, after an unmatched left row has been null-extended.
	Post []string
	// Equi are the equality conjuncts of Cond whose halves each read only one side.
	Equi []JoinEqui

	// Limit stops after this many joined rows (<= 0 = all).
	Limit int
}

// JoinLimits bound what one join may spend. They are the store's own, not the caller's: a
// request must not be able to ask a daemon to hold or produce an unbounded amount, so these
// are applied here and a caller can only ask for less (see clamp).
type JoinLimits struct {
	// MaxHashRows caps the rows held for the hashed side. Only one side is held, but it is
	// held entirely, so this is the memory bound.
	MaxHashRows int
	// MaxPairs caps candidate pairs EXAMINED. Neither the per-side cap nor hashing bounds
	// this: a key with one distinct value puts every row in one bucket, and that bucket is
	// the cross product. So the budget is spent where pairs are actually examined, which is
	// the only place that sees both sides.
	MaxPairs int
	// MaxRows caps joined rows produced.
	MaxRows int
}

// DefaultJoinLimits are the limits a join runs under when a caller names none.
func DefaultJoinLimits() JoinLimits {
	return JoinLimits{MaxHashRows: 500_000, MaxPairs: 20_000_000, MaxRows: 1_000_000}
}

// clamp lowers each limit to the caller's, never raises it past the store's own.
func (l JoinLimits) clamp(want JoinLimits) JoinLimits {
	lower := func(mine, theirs int) int {
		if theirs > 0 && theirs < mine {
			return theirs
		}
		return mine
	}
	return JoinLimits{
		MaxHashRows: lower(l.MaxHashRows, want.MaxHashRows),
		MaxPairs:    lower(l.MaxPairs, want.MaxPairs),
		MaxRows:     lower(l.MaxRows, want.MaxRows),
	}
}

// JoinStats is what the join did, and whether it finished.
//
// Err carries a failure met WHILE streaming -- a limit reached, most likely -- which the
// result sequence cannot report, since it yields rows. A caller must check it after draining
// or it will read a truncated answer as a complete one.
type JoinStats struct {
	HashRows int // rows held for the hashed side
	ScanRows int // rows streamed from the other side
	Pairs    int // candidate pairs examined
	Rows     int // joined rows produced
	Groups   int // groups a reduced join returned (0 when the rows were not reduced)
	Err      error

	// ProbeAttr and ProbeKeys describe an index-based equi join: the attribute the hashed
	// side's scan was narrowed on, and how many distinct key values it was narrowed to.
	// ProbeKeys is 0 when the scan was not narrowed, and ProbeWhy then says why -- because
	// a plan that silently did not use an index reads exactly like one that had none.
	ProbeAttr string
	ProbeKeys int
	ProbeWhy  string
}

// joinPlan is a compiled JoinSpec.
type joinPlan struct {
	kind         JoinKind
	leftAlias    string
	rightAlias   string
	leftProject  []string
	rightProject []string
	cond         []*classad.Expr
	post         []*classad.Expr
	// condAlias is cond compiled to read the two sides directly, or nil when a term
	// needs a real record. See joincond.go.
	condAlias *aliasedCond
	// postAlias is the same for the residual WHERE. It is not only a speed-up there: the
	// filter has to see the sides as they were READ, not as the output projection leaves
	// them (see emit).
	postAlias *aliasedCond
	equiLeft  []*classad.Expr
	equiRight []*classad.Expr
	// equiLeftAttr and equiRightAttr name the attribute each equality half reads when it
	// is a bare reference, and are empty when it is anything else. A bare half is read off
	// the row's bytes; anything else is evaluated against the decoded ad. See key.
	equiLeftAttr  []string
	equiRightAttr []string
	limit         int
	limits        JoinLimits
}

// Join pairs two tables and streams the joined rows.
//
// Setup problems -- an unknown table, an expression that does not parse -- are returned
// before any row. A limit met part way through is reported in stats.Err, because by then the
// sequence has already yielded rows and has no other way to say it stopped early.
func (cat *Catalog) Join(spec JoinSpec, want JoinLimits, stats *JoinStats) (iter.Seq[*classad.ClassAd], error) {
	return cat.joinWith(spec, DefaultJoinLimits().clamp(want), stats)
}

// joinWith is Join with the limits already settled, so a caller whose rows do not cross a
// wire can run the same join under its own defaults (see DefaultJoinAggLimits) without
// DefaultJoinLimits being applied on top of them. It clamps nothing: every caller clamps
// against its own defaults before it gets here, which is what keeps "a caller can only ask
// for less" a property of every entry point rather than of this one.
func (cat *Catalog) joinWith(spec JoinSpec, limits JoinLimits, stats *JoinStats) (iter.Seq[*classad.ClassAd], error) {
	return joinStream(cat, spec, limits, stats,
		func(p *joinPlan, l, r *joinRow) (*classad.ClassAd, bool) { return p.composite(l, r), true })
}

// joinStream is the join with the form of its output rows left to the caller: mk turns a
// surviving pair into one row, and reports false to drop it.
//
// It exists so that a caller wanting something other than a composite ClassAd -- rows
// rendered straight from the two sides' wire bytes, say -- gets the SAME pair loop rather
// than a second one beside it. The loop is where a join's correctness lives: which pairs the
// condition keeps, when an unmatched LEFT row is null-extended, which limit stops what. A
// second copy of that is a second set of answers to those questions.
func joinStream[T any](cat *Catalog, spec JoinSpec, limits JoinLimits, stats *JoinStats,
	mk func(p *joinPlan, l, r *joinRow) (T, bool)) (iter.Seq[T], error) {
	p, err := compileJoin(spec, limits)
	if err != nil {
		return nil, err
	}
	// The RIGHT side is hashed and the left streamed, matching the caller's own naming so a
	// plan reads the same here as where it was made.
	readProj := planJoinRead(spec)
	scanSeq, err := cat.joinSource(spec.Left, readProj.left)
	if err != nil {
		return nil, err
	}
	if stats == nil {
		stats = &JoinStats{}
	}

	// Before reading the hashed side whole, see whether an index can be made to read only
	// the rows whose key the other side actually holds (see db/joinprobe.go).
	right := spec.Right
	if probe, why := cat.planIndexProbe(p, spec, scanSeq); probe != nil {
		if why == "" {
			right.Constraint = probe.cons
			stats.ProbeKeys = probe.keys
			stats.ProbeAttr = probe.attr
		} else {
			stats.ProbeWhy = why
		}
		// The streamed side was held to count its keys; stream it from there either way,
		// so declining the probe does not cost a second scan.
		scanSeq = rowsSeq(probe.held)
	} else {
		stats.ProbeWhy = why
		// A nil probe can mean the decision gave up PART WAY through the streamed side --
		// the one gate that declines after reading rows is the one for a side too large to
		// hold. Whether a source sequence may be ranged twice is a property of each kind
		// of table, not something this can assume, so the side is resolved again rather
		// than re-ranged. It costs a query's construction, not a scan.
		if scanSeq, err = cat.joinSource(spec.Left, readProj.left); err != nil {
			return nil, err
		}
	}

	hashSeq, err := cat.joinSource(right, readProj.right)
	if err != nil {
		return nil, err
	}
	return runJoin(p, hashSeq, scanSeq, stats, mk), nil
}

// joinSource resolves one side to a sequence of its rows, whatever kind of thing it names.
//
// A view resolves to BOTH of its halves. A continuous aggregate moves closed buckets out of
// its live backing into an archive, so a join that read only the backing would silently join
// against the unsealed tail -- the same bug the read paths had.
func (cat *Catalog) joinSource(s JoinSide, read []string) (iter.Seq[*joinRow], error) {
	constraint := s.Constraint
	if strings.TrimSpace(constraint) == "" {
		constraint = "true"
	}
	if d, ok := cat.Table(s.Table); ok {
		if seq, narrowed := narrowTableRead(d, constraint, read); narrowed {
			return seq, nil
		}
		ads, err := d.Query(constraint)
		if err != nil {
			return nil, err
		}
		return adRows(ads), nil
	}
	if d, ok := cat.ViewBacking(s.Table); ok {
		live, err := d.Query(constraint)
		if err != nil {
			return nil, err
		}
		v, has := cat.View(s.Table)
		if !has {
			return adRows(live), nil
		}
		arch, sealed := v.SealedArchive()
		if !sealed {
			return adRows(live), nil
		}
		old, err := arch.Query(constraint)
		if err != nil {
			return nil, err
		}
		return adRows(func(yield func(*classad.ClassAd) bool) {
			for ad := range live {
				if !yield(ad) {
					return
				}
			}
			for ad := range old {
				if !yield(ad) {
					return
				}
			}
		}), nil
	}
	if a, ok := cat.ArchiveTable(s.Table); ok {
		if seq, narrowed := narrowArchiveRead(a, constraint, read); narrowed {
			return seq, nil
		}
		ads, err := a.Query(constraint)
		if err != nil {
			return nil, err
		}
		return adRows(ads), nil
	}
	return nil, fmt.Errorf("no such table: %s", s.Table)
}

func compileJoin(spec JoinSpec, limits JoinLimits) (*joinPlan, error) {
	if spec.Left.Alias == "" || spec.Right.Alias == "" {
		return nil, fmt.Errorf("a join needs an alias for each side")
	}
	if strings.EqualFold(spec.Left.Alias, spec.Right.Alias) {
		return nil, fmt.Errorf("both sides of the join are named %q", spec.Left.Alias)
	}
	p := &joinPlan{
		kind: spec.Kind, leftAlias: spec.Left.Alias, rightAlias: spec.Right.Alias,
		leftProject: spec.Left.Project, rightProject: spec.Right.Project,
		limit: spec.Limit, limits: limits,
	}
	var err error
	if p.cond, err = compileAll("ON", spec.Cond); err != nil {
		return nil, err
	}
	p.condAlias = compileAliasedCond(spec.Cond, p.leftAlias, p.rightAlias)
	if p.post, err = compileAll("WHERE", spec.Post); err != nil {
		return nil, err
	}
	p.postAlias = compileAliasedCond(spec.Post, p.leftAlias, p.rightAlias)
	for i, e := range spec.Equi {
		l, lerr := classad.ParseExpr(e.Left)
		if lerr != nil {
			return nil, fmt.Errorf("join equality %d, left half %q: %w", i, e.Left, lerr)
		}
		r, rerr := classad.ParseExpr(e.Right)
		if rerr != nil {
			return nil, fmt.Errorf("join equality %d, right half %q: %w", i, e.Right, rerr)
		}
		p.equiLeft = append(p.equiLeft, l)
		p.equiRight = append(p.equiRight, r)
		p.equiLeftAttr = append(p.equiLeftAttr, bareAttrName(e.Left))
		p.equiRightAttr = append(p.equiRightAttr, bareAttrName(e.Right))
	}
	return p, nil
}

func compileAll(what string, srcs []string) ([]*classad.Expr, error) {
	out := make([]*classad.Expr, 0, len(srcs))
	for _, src := range srcs {
		ex, err := classad.ParseExpr(src)
		if err != nil {
			return nil, fmt.Errorf("%s term %q: %w", what, src, err)
		}
		out = append(out, ex)
	}
	return out, nil
}

// runJoin builds the hash from one side and streams the other against it, handing each
// surviving pair to mk for the caller's row form.
func runJoin[T any](p *joinPlan, hashSeq, scanSeq iter.Seq[*joinRow], stats *JoinStats,
	mk func(p *joinPlan, l, r *joinRow) (T, bool)) iter.Seq[T] {
	return func(yield func(T) bool) {
		// Hash the right side on the equality keys when there are any; otherwise every
		// left row is paired against every right row and decided by the condition alone.
		var buckets map[string][]*joinRow
		var all []*joinRow
		hashed := len(p.equiRight) > 0
		if hashed {
			buckets = map[string][]*joinRow{}
		}
		for ad := range hashSeq {
			if stats.HashRows >= p.limits.MaxHashRows {
				stats.Err = fmt.Errorf("the join would hold more than %d rows from %s; "+
					"narrow it with a constraint on that side", p.limits.MaxHashRows, p.rightAlias)
				return
			}
			stats.HashRows++
			if !hashed {
				all = append(all, ad)
				continue
			}
			k, ok := p.key(ad, false)
			if !ok {
				continue // an undefined key equals nothing: ClassAd == propagates undefined
			}
			buckets[k] = append(buckets[k], ad)
		}

		for l := range scanSeq {
			stats.ScanRows++
			candidates := all
			if hashed {
				k, ok := p.key(l, true)
				if !ok {
					candidates = nil // an undefined key on the left matches nothing
				} else {
					candidates = buckets[k]
				}
			}
			matched := false
			for _, r := range candidates {
				stats.Pairs++
				if stats.Pairs > p.limits.MaxPairs {
					stats.Err = fmt.Errorf("the join examined more than %d candidate pairs; "+
						"it needs a more selective condition", p.limits.MaxPairs)
					return
				}
				// The hash only narrowed the candidates; the condition decides, in full,
				// with ClassAd's own semantics.
				if !p.condHolds(l, r) {
					continue
				}
				// The pair matched. Whether the row then survives the post-filter is a
				// SEPARATE question, and conflating the two is a wrong answer: a matched
				// row that WHERE drops must not come back as a null-extended one.
				matched = true
				if stop := emitJoin(p, l, r, stats, mk, yield); stop {
					return
				}
			}
			if !matched && p.kind == JoinLeft {
				if stop := emitJoin(p, l, nil, stats, mk, yield); stop {
					return
				}
			}
			if p.limit > 0 && stats.Rows >= p.limit {
				return
			}
		}
	}
}

// emit forms the joined row, applies the post-filter and yields it. It reports only whether
// the join should STOP -- whether the row survived the filter is deliberately not reported,
// because the one caller that used to branch on it drew the wrong conclusion from it.
func emitJoin[T any](p *joinPlan, l, r *joinRow, stats *JoinStats,
	mk func(p *joinPlan, l, r *joinRow) (T, bool), yield func(T) bool) (stop bool) {
	if !p.postHolds(l, r) {
		return false
	}
	row, ok := mk(p, l, r)
	if !ok {
		return false // the row form could not be made; it is not a reason to stop
	}
	if stats.Rows >= p.limits.MaxRows {
		stats.Err = fmt.Errorf("the join produced more than %d rows; narrow it with a constraint",
			p.limits.MaxRows)
		return true
	}
	stats.Rows++
	return !yield(row)
}

// postHolds reports whether every residual WHERE term is TRUE for this pair.
//
// It reads the two sides AS READ, not through the output projection. That distinction is the
// whole point of this existing separately from emit: a WHERE is not a projection, and
// `WHERE e.MemoryUsage > 1000` has to work whether or not the caller asked to SEE
// MemoryUsage. Filtering the projected row instead -- which is what this used to do, by
// building the row first -- made such a term read UNDEFINED and silently match nothing, so a
// join answered a perfectly ordinary query with no rows and no error.
//
// A nil right side is an unmatched LEFT row, and every reference through its alias is
// undefined, exactly as it is for the ON condition. Dropping the row here does NOT bring it
// back null-extended: run decides that from whether the pair MATCHED, which is a separate
// question answered before this (see the comment there).
func (p *joinPlan) postHolds(l, r *joinRow) bool {
	if len(p.post) == 0 {
		return true
	}
	if p.postAlias != nil {
		if held, ok := p.postAlias.holds(l, r); ok {
			return held
		}
	}
	// A term the alias compiler would not take reads the sides out of a composite, which is
	// built WITHOUT the output projection for the reason above. Building it decodes both
	// sides, which is exactly what the wire form avoids -- so it is built only here, for the
	// terms that genuinely need a record.
	row := classad.New()
	if l != nil {
		row.InsertAttrClassAd(p.leftAlias, l.record())
	}
	if r != nil {
		row.InsertAttrClassAd(p.rightAlias, r.record())
	}
	for _, ex := range p.post {
		if !isTrue(ex.Eval(row)) {
			return false
		}
	}
	return true
}

// composite builds the joined row: one ClassAd whose members are the two sides under their
// aliases. A nil right side -- an unmatched LEFT row -- is simply absent, so every reference
// through its alias evaluates to undefined.
func (p *joinPlan) composite(l, r *joinRow) *classad.ClassAd {
	row := classad.New()
	if l != nil {
		row.InsertAttrClassAd(p.leftAlias, l.member(p.leftProject))
	}
	if r != nil {
		row.InsertAttrClassAd(p.rightAlias, r.member(p.rightProject))
	}
	return row
}

// project narrows an ad to the named attributes, carrying each one's stored form rather than
// its value -- an attribute holding an expression stays an expression, as it would in any
// other projection. Nil attrs carries the whole ad.
func project(ad *classad.ClassAd, attrs []string) *classad.ClassAd {
	if attrs == nil {
		return ad
	}
	out := classad.New()
	for _, a := range attrs {
		if ex, ok := ad.Lookup(a); ok {
			out.InsertExpr(a, ex)
		}
	}
	return out
}

// condHolds reports whether every condition term is TRUE for this pair.
//
// Only a true boolean keeps a pair, which is what makes splitting the condition into
// per-side constraints and this residual sound even though ClassAd's && is left-biased and
// not commutative: the only way `A && B` is true is for both to be true, which does not
// depend on their order. The VALUE of the conjunction is order-dependent; the match set is
// not.
func (p *joinPlan) condHolds(l, r *joinRow) bool {
	if len(p.cond) == 0 {
		return true
	}
	if p.condAlias != nil {
		if held, ok := p.condAlias.holds(l, r); ok {
			return held
		}
	}
	row := classad.New()
	row.InsertAttrClassAd(p.leftAlias, l.record())
	if r != nil {
		row.InsertAttrClassAd(p.rightAlias, r.record())
	}
	for _, ex := range p.cond {
		if !isTrue(ex.Eval(row)) {
			return false
		}
	}
	return true
}

// key renders one side's equality halves as a bucket key.
//
// It must not separate two values that would compare equal, which is why a number is
// rendered through float64 whatever its stored width and a string is folded: ClassAd compares
// 1 and 1.0 equal, and strings case-insensitively. Anything with no scalar form -- undefined,
// error, a list, a nested ad -- equals nothing and reports false.
func (p *joinPlan) key(row *joinRow, left bool) (string, bool) {
	var b strings.Builder
	for i := range p.equiLeft {
		ex := p.equiRight[i]
		if left {
			ex = p.equiLeft[i]
		}
		// An equality half is a bare attribute name in every plan that drives a hash, and
		// then the row answers it without being decoded. Anything else is an expression
		// that has to be evaluated in the context of its own ad.
		var v classad.Value
		name := p.equiRightAttr[i]
		if left {
			name = p.equiLeftAttr[i]
		}
		if name != "" {
			v = row.eval(name)
		} else {
			v = ex.Eval(row.record())
		}
		// Each accessor's error is consulted rather than discarded: a value that claims a
		// kind and then will not produce it has no key, and hashing the zero it returns
		// would put it in a bucket it does not belong to.
		switch {
		case v.IsBool():
			x, err := v.BoolValue()
			if err != nil {
				return "", false
			}
			if x {
				b.WriteString("n1")
			} else {
				b.WriteString("n0")
			}
		case v.IsInteger():
			n, err := v.IntValue()
			if err != nil {
				return "", false
			}
			b.WriteString("n")
			b.WriteString(strconv.FormatFloat(float64(n), 'g', -1, 64))
		case v.IsReal():
			f, err := v.RealValue()
			if err != nil {
				return "", false
			}
			b.WriteString("n")
			b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
		case v.IsString():
			str, err := v.StringValue()
			if err != nil {
				return "", false
			}
			b.WriteString("s")
			b.WriteString(strings.ToLower(str))
		default:
			return "", false
		}
		if i < len(p.equiLeft)-1 {
			b.WriteByte(0)
		}
	}
	return b.String(), true
}

// isTrue reports whether a value is the boolean true. Undefined, error and a non-boolean are
// all not-true, which is what keeps a pair out rather than letting it through.
func isTrue(v classad.Value) bool {
	if !v.IsBool() {
		return false
	}
	b, err := v.BoolValue()
	return err == nil && b
}
