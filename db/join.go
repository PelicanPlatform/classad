package db

// Joining two tables inside the store.
//
// A joined row is a ClassAd whose members are the two sides under their aliases, so `ON`,
// the residual `WHERE` and everything a caller writes afterwards are ordinary ClassAd
// expressions over one ad. Nothing here teaches the evaluator a new rule: `h.ClusterId` is
// the select operator over a nested ad, and an unmatched LEFT row simply has no member for
// the right side, which is ClassAd's own spelling of SQL's NULL.
//
// WHY IT IS WORTH DOING HERE rather than above the store. A client join must fetch both
// sides whole and pair them at the far end, so every surviving row of both tables is
// serialized, shipped and rebuilt before a single pair is examined -- measured at a flat
// ~2.2x on a narrow projection and ~4.2x on an unprojected one, before any network. A join
// is the one query shape where selectivity can only be applied AFTER serialization from
// outside, and before it from inside. Only the joined output crosses from here.
//
// It also materializes less. The hash is built on one side and the other is streamed
// against it, so one side is held at a time; a client join holds both.
//
// WHAT THIS IS NOT, yet. The caller supplies the plan -- which conjuncts went to which
// side, which equalities drive the hash -- because that splitting is settled, tested and
// identical wherever it runs, and re-deriving it here would be a second implementation of
// the same rules. Block-level zone skipping, which is the thing a client genuinely cannot
// do, is not here either: it belongs with the columnar machinery and arrives separately.
// What this buys is the wire and the single-side materialization.

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
	Err      error
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
	equiLeft     []*classad.Expr
	equiRight    []*classad.Expr
	limit        int
	limits       JoinLimits
}

// Join pairs two tables and streams the joined rows.
//
// Setup problems -- an unknown table, an expression that does not parse -- are returned
// before any row. A limit met part way through is reported in stats.Err, because by then the
// sequence has already yielded rows and has no other way to say it stopped early.
func (cat *Catalog) Join(spec JoinSpec, want JoinLimits, stats *JoinStats) (iter.Seq[*classad.ClassAd], error) {
	p, err := compileJoin(spec, DefaultJoinLimits().clamp(want))
	if err != nil {
		return nil, err
	}
	// The RIGHT side is hashed and the left streamed, matching the caller's own naming so a
	// plan reads the same here as where it was made.
	hashSeq, err := cat.joinSource(spec.Right)
	if err != nil {
		return nil, err
	}
	scanSeq, err := cat.joinSource(spec.Left)
	if err != nil {
		return nil, err
	}
	if stats == nil {
		stats = &JoinStats{}
	}
	return p.run(hashSeq, scanSeq, stats), nil
}

// joinSource resolves one side to a sequence of its ads, whatever kind of thing it names.
//
// A view resolves to BOTH of its halves. A continuous aggregate moves closed buckets out of
// its live backing into an archive, so a join that read only the backing would silently join
// against the unsealed tail -- the same bug the read paths had.
func (cat *Catalog) joinSource(s JoinSide) (iter.Seq[*classad.ClassAd], error) {
	constraint := s.Constraint
	if strings.TrimSpace(constraint) == "" {
		constraint = "true"
	}
	if d, ok := cat.Table(s.Table); ok {
		return d.Query(constraint)
	}
	if d, ok := cat.ViewBacking(s.Table); ok {
		live, err := d.Query(constraint)
		if err != nil {
			return nil, err
		}
		v, has := cat.View(s.Table)
		if !has {
			return live, nil
		}
		arch, sealed := v.SealedArchive()
		if !sealed {
			return live, nil
		}
		old, err := arch.Query(constraint)
		if err != nil {
			return nil, err
		}
		return func(yield func(*classad.ClassAd) bool) {
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
		}, nil
	}
	if a, ok := cat.ArchiveTable(s.Table); ok {
		return a.Query(constraint)
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
	if p.post, err = compileAll("WHERE", spec.Post); err != nil {
		return nil, err
	}
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

// run builds the hash from one side and streams the other against it.
func (p *joinPlan) run(hashSeq, scanSeq iter.Seq[*classad.ClassAd], stats *JoinStats) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		// Hash the right side on the equality keys when there are any; otherwise every
		// left row is paired against every right row and decided by the condition alone.
		var buckets map[string][]*classad.ClassAd
		var all []*classad.ClassAd
		hashed := len(p.equiRight) > 0
		if hashed {
			buckets = map[string][]*classad.ClassAd{}
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
				if stop := p.emit(l, r, stats, yield); stop {
					return
				}
			}
			if !matched && p.kind == JoinLeft {
				if stop := p.emit(l, nil, stats, yield); stop {
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
func (p *joinPlan) emit(l, r *classad.ClassAd, stats *JoinStats, yield func(*classad.ClassAd) bool) (stop bool) {
	row := p.composite(l, r)
	for _, ex := range p.post {
		if !isTrue(ex.Eval(row)) {
			return false
		}
	}
	if stats.Rows >= p.limits.MaxRows {
		stats.Err = fmt.Errorf("the join produced more than %d rows; narrow it with a constraint",
			p.limits.MaxRows)
		return true
	}
	stats.Rows++
	return !yield(row)
}

// composite builds the joined row: one ClassAd whose members are the two sides under their
// aliases. A nil right side -- an unmatched LEFT row -- is simply absent, so every reference
// through its alias evaluates to undefined.
func (p *joinPlan) composite(l, r *classad.ClassAd) *classad.ClassAd {
	row := classad.New()
	if l != nil {
		row.InsertAttrClassAd(p.leftAlias, project(l, p.leftProject))
	}
	if r != nil {
		row.InsertAttrClassAd(p.rightAlias, project(r, p.rightProject))
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
func (p *joinPlan) condHolds(l, r *classad.ClassAd) bool {
	if len(p.cond) == 0 {
		return true
	}
	row := classad.New()
	row.InsertAttrClassAd(p.leftAlias, l)
	if r != nil {
		row.InsertAttrClassAd(p.rightAlias, r)
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
func (p *joinPlan) key(ad *classad.ClassAd, left bool) (string, bool) {
	var b strings.Builder
	for i := range p.equiLeft {
		ex := p.equiRight[i]
		if left {
			ex = p.equiLeft[i]
		}
		// Each accessor's error is consulted rather than discarded: a value that claims a
		// kind and then will not produce it has no key, and hashing the zero it returns
		// would put it in a bucket it does not belong to.
		v := ex.Eval(ad)
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
