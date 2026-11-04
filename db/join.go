package db

// Joining two or more tables inside the store.
//
// A joined row is a ClassAd whose members are the sides under their aliases, so `ON`,
// the residual `WHERE` and everything a caller writes afterwards are ordinary ClassAd
// expressions over one ad. Nothing here teaches the evaluator a new rule: `h.ClusterId` is
// the select operator over a nested ad, and an unmatched LEFT row simply has no member for
// the side that matched nothing, which is ClassAd's own spelling of SQL's NULL.
//
// WHAT IT SHIPS AND WHAT IT COSTS ARE DIFFERENT QUESTIONS, and they have different answers.
// A joined row carries every side, so a join that EXPANDS ships more from here than its
// inputs were: an earlier side is repeated once per output row, 1.86x the bytes a client
// pairing the same rows ships at two tables and 2.30x at three. That is measured, it is
// structural, and it is NOT the cost -- the same expanding chain still answers faster here.
// Bytes are worth knowing about for a link that is actually narrow; they are not the reason
// to choose where a join runs. Both measurements are in dbrpc:
// TestNwayWireVolumeGrowsWithTheChain for the bytes, BenchmarkJoinChainWhereItRuns for the
// clock.
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
// AND IT JOINS A CHAIN, not only a pair. A chain of three or more is left-deep -- this join
// run n-1 times, each step folding in one more table -- so it is a generalization rather than
// a second algorithm, and it runs as one nested loop over one hash per step.
//
// It is worth doing here for the reason the two-table case is, only more so: measured end to
// end over a connection, a three-table chain answers faster in the store than a client pairing
// the same rows does, on every shape tried and whichever order both are given -- 1.14x on a
// chain that expands, 3.00x on one that filters, with a fifth of the allocations in the best
// case. The numbers and the fixture are in dbrpc, under BenchmarkJoinChainWhereItRuns.
//
// The expanding case is the one to notice, because it is where this ships MORE: it wins
// anyway. Which says that what a join costs is not what it ships.

import (
	"fmt"
	"iter"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
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

// JoinEqui is one `<left expr> == <right expr>` conjunct of a step's condition, split into
// the half that reads what is already in the row and the half that reads the table being
// folded in. They drive the hash.
//
// THE TWO HALVES ARE IN DIFFERENT SCOPES, which is the one asymmetry a reader has to hold.
// Right reads this step's own table, so it is a bare path -- `ClusterId`, not `c.ClusterId`.
// Left reads the accumulated row, which has one member per side folded in so far, so past the
// first step it must be QUALIFIED -- `a.ClusterId` -- because at the third step it might
// read the first table or the second. At the first step the row holds one side and a bare
// path is unambiguous, which is why the two-table spelling has always written Left bare and
// keeps working unchanged.
//
// They narrow candidates only. Every surviving pair is still checked against the full
// condition, so the hash never has to reproduce ClassAd's equality semantics exactly -- it
// only has to avoid putting two rows in different buckets when they would compare equal.
type JoinEqui struct {
	Left, Right string
}

// JoinStep folds one more table into the joined row.
//
// A chain of tables is LEFT-DEEP: the row starts as the first side and each step adds one
// member to it. That is the only shape SQL's syntax can express without parentheses, and it
// is why an n-table join is this join run n-1 times rather than a second algorithm.
//
// Kind is this step's own, and it decides null-extension for THIS side alone. In a left-deep
// chain a side is null-extendable exactly when its own step is a LEFT join: every earlier
// table is preserved by every later step, so being preserved once is being preserved
// throughout. That is why the rule is one comparison and not a walk back along the chain.
type JoinStep struct {
	Kind JoinKind
	// Side is the table this step folds in, under the alias the row will carry it as.
	Side JoinSide
	// Cond are this step's ON conjuncts, over the row AS IT IS AFTER this step. A conjunct
	// naming a side a LATER step folds in is refused rather than deferred: SQL does not
	// allow a forward reference, and deferring one would silently answer a different query.
	Cond []string
	// Equi are the equality conjuncts of Cond that drive this step's hash. Left is written
	// over the accumulated row and Right over this step's own table; see JoinEqui.
	Equi []JoinEqui
}

// JoinSpec is a planned join of two or more tables.
//
// There are two spellings and a spec carries exactly one. Left/Right/Kind/Cond/Equi is the
// two-table spelling, unchanged and still what the two-table opcodes send. Left plus Steps is
// the chain spelling, where Left is the FROM table and each step folds in one more. The two
// are the same plan: the two-table spelling IS the chain with a single step, and compiles to
// it, so there is one pair loop rather than one per spelling.
type JoinSpec struct {
	Kind        JoinKind
	Left, Right JoinSide

	// Steps is the chain spelling: one step per table after Left. When it is set, Right,
	// Kind, Cond and Equi must be empty -- two spellings in one spec have no single
	// meaning, and picking one would answer a query nobody wrote.
	Steps []JoinStep

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
	//
	// They describe the FIRST step, which for a two-table join is the only one. A longer
	// chain decides per step, and ProbeSteps carries all of them.
	ProbeAttr string
	ProbeKeys int
	ProbeWhy  string

	// ProbeSteps is the probe decision for each step of the chain, in order.
	ProbeSteps []JoinProbeStat
}

// JoinProbeStat is one step's index-probe decision.
type JoinProbeStat struct {
	// Alias names the side this step folds in.
	Alias string
	// Attr is the attribute a membership constraint was built on, empty when none was.
	Attr string
	// Keys is how many distinct key values the scan was narrowed to, 0 when it was not.
	Keys int
	// Why says why the scan was not narrowed, empty when it was.
	Why string
}

// syncFirstProbe copies the first step's decision into the single-step fields, so a two-table
// caller reads what it always read.
func (s *JoinStats) syncFirstProbe() {
	if len(s.ProbeSteps) == 0 {
		return
	}
	s.ProbeAttr = s.ProbeSteps[0].Attr
	s.ProbeKeys = s.ProbeSteps[0].Keys
	s.ProbeWhy = s.ProbeSteps[0].Why
	if s.ProbeKeys == 0 {
		// A step that was gated out before an attribute was chosen reports no attribute,
		// matching what the two-table path reported when it declined.
		s.ProbeAttr = ""
	}
}

// empty reports whether a side was never filled in, which is how the chain spelling says it
// is not using the two-table one.
func (s JoinSide) empty() bool {
	return s.Table == "" && s.Alias == "" && s.Constraint == "" && s.Project == nil
}

// chain resolves a spec into the canonical left-deep form: the FROM side, then one step per
// table folded in after it.
//
// The two-table spelling becomes a chain of one step, so everything below sees one shape. A
// spec carrying BOTH spellings is refused rather than reconciled: there is no reading of it
// that is obviously what the caller meant, and picking one would answer a different query
// without saying so.
func (s JoinSpec) chain() ([]JoinSide, []JoinStep, error) {
	if len(s.Steps) == 0 {
		return []JoinSide{s.Left, s.Right}, []JoinStep{{
			Kind: s.Kind, Side: s.Right, Cond: s.Cond, Equi: s.Equi,
		}}, nil
	}
	if !s.Right.empty() || s.Kind != JoinInner || len(s.Cond) > 0 || len(s.Equi) > 0 {
		return nil, nil, fmt.Errorf("a join spec carries both the two-table spelling and a " +
			"chain of steps; use one or the other")
	}
	sides := make([]JoinSide, 0, len(s.Steps)+1)
	sides = append(sides, s.Left)
	for i := range s.Steps {
		st := &s.Steps[i]
		sides = append(sides, st.Side)
	}
	return sides, s.Steps, nil
}

// joinPlan is a compiled JoinSpec: the sides the row is made of, and the steps that fold
// them in.
type joinPlan struct {
	// aliases and projects are indexed by SIDE, and a side's index is its position in the
	// chain. Every compiled expression below refers to a side by that index, which is what
	// makes the alias resolver a slice lookup rather than a name comparison.
	aliases  []string
	projects [][]string
	steps    []*joinStepPlan
	post     []*classad.Expr
	// postAlias is post compiled to read the sides directly, or nil when a term needs a
	// real record. It is not only a speed-up: the filter has to see the sides as they were
	// READ, not as the output projection leaves them (see emit).
	postAlias *aliasedCond
	limit     int
	limits    JoinLimits
}

// nSides is how many tables the row is made of.
func (p *joinPlan) nSides() int { return len(p.aliases) }

// joinStepPlan is one compiled step: the table it folds in, and the terms that decide which
// rows of it join to the row so far.
type joinStepPlan struct {
	kind JoinKind
	// side indexes the table this step folds in. It is always the step's position + 1, so
	// the sides visible to this step's terms are 0..side.
	side int
	cond []*classad.Expr
	// condAlias is cond compiled to read the sides directly, or nil when a term needs a
	// real record. See joincond.go.
	condAlias *aliasedCond
	// equiRow and equiSide are the two halves of each equality: the half reading the
	// accumulated row and the half reading this step's own table.
	equiRow  []*classad.Expr
	equiSide []*classad.Expr
	// equiRowAttr[i] names the attribute the row-half reads when it is a bare reference AND
	// the row holds exactly one side, which is the only case where a bare path is
	// unambiguous. Empty otherwise, and then rowAliased or equiRow answers instead.
	equiRowAttr []string
	// rowAliased[i] is the row-half compiled to read the sides directly, or nil when it
	// declined and equiRow must be evaluated against a composite of the row so far.
	rowAliased []*aliasedValue
	// equiSideAttr[i] names the attribute the side-half reads when it is a bare reference,
	// which is what it is in every plan that drives a hash.
	equiSideAttr []string
	// rowReadsOnlySide0 says the row halves read the FROM side and nothing else, so their
	// values are known from a scan of it alone -- before any earlier step has run. That is
	// what lets a step past the first be index-probed: the keys it would be narrowed to are
	// the FROM side's keys, which one scan yields for every such step at once.
	rowReadsOnlySide0 bool
}

// JoinsChains reports that this catalog's join accepts a chain of more than two tables.
//
// It is a capability marker, for a relay deciding whether to advertise the chain opcode. A
// catalog from a build whose join predates chains satisfies the same join interface and would
// accept a chain request and answer a different query, so "it has a Join method" is not the
// question a relay needs answered.
func (cat *Catalog) JoinsChains() bool { return true }

// Join pairs two or more tables and streams the joined rows.
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
		func(p *joinPlan, rows []*joinRow) (*classad.ClassAd, bool) { return p.composite(rows), true })
}

// joinStream is the join with the form of its output rows left to the caller: mk turns a
// surviving combination into one row, and reports false to drop it.
//
// It exists so that a caller wanting something other than a composite ClassAd -- rows
// rendered straight from the sides' wire bytes, say -- gets the SAME pair loop rather than a
// second one beside it. The loop is where a join's correctness lives: which pairs the
// condition keeps, when an unmatched LEFT row is null-extended, which limit stops what. A
// second copy of that is a second set of answers to those questions.
func joinStream[T any](cat *Catalog, spec JoinSpec, limits JoinLimits, stats *JoinStats,
	mk func(p *joinPlan, rows []*joinRow) (T, bool)) (iter.Seq[T], error) {
	p, err := compileJoin(spec, limits)
	if err != nil {
		return nil, err
	}
	sides, _, err := spec.chain()
	if err != nil {
		return nil, err
	}
	readProj := planJoinRead(spec)
	scanSeq, err := cat.joinSource(sides[0], readProj[0])
	if err != nil {
		return nil, err
	}
	if stats == nil {
		stats = &JoinStats{}
	}

	// Before reading any step's table whole, see whether an index can be made to read only
	// the rows whose key the FROM side actually holds (see db/joinprobe.go).
	probes := cat.planIndexProbes(p, sides, scanSeq, stats)
	if probes.held != nil {
		// The FROM side was held to count its keys; stream it from there, so a probe that
		// was decided and then declined does not cost a second scan.
		scanSeq = rowsSeq(probes.held)
	} else if probes.rescan {
		// The decision gave up PART WAY through the FROM side -- the one gate that declines
		// after reading rows is the one for a side too large to hold. Whether a source
		// sequence may be ranged twice is a property of each kind of table, not something
		// this can assume, so the side is resolved again rather than re-ranged. It costs a
		// query's construction, not a scan.
		if scanSeq, err = cat.joinSource(sides[0], readProj[0]); err != nil {
			return nil, err
		}
	}

	hashSeqs := make([]iter.Seq[*joinRow], 0, len(p.steps))
	for i, sp := range p.steps {
		side := sides[sp.side]
		if cons, ok := probes.cons[i]; ok {
			side.Constraint = cons
		}
		seq, serr := cat.joinSource(side, readProj[sp.side])
		if serr != nil {
			return nil, serr
		}
		hashSeqs = append(hashSeqs, seq)
	}
	return runJoin(p, hashSeqs, scanSeq, stats, mk), nil
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
	sides, steps, err := spec.chain()
	if err != nil {
		return nil, err
	}
	p := &joinPlan{limit: spec.Limit, limits: limits}
	seen := map[string]bool{}
	for _, sd := range sides {
		if sd.Alias == "" {
			return nil, fmt.Errorf("a join needs an alias for each side")
		}
		if seen[strings.ToLower(sd.Alias)] {
			return nil, fmt.Errorf("two sides of the join are named %q", sd.Alias)
		}
		seen[strings.ToLower(sd.Alias)] = true
		p.aliases = append(p.aliases, sd.Alias)
		p.projects = append(p.projects, sd.Project)
	}

	if p.post, err = compileAll("WHERE", spec.Post); err != nil {
		return nil, err
	}
	// The residual WHERE is applied once, to the finished row, so every side is in scope
	// for it and there is no forward reference to refuse.
	if scopeErr := refuseUnscopedSides("WHERE", spec.Post, p.aliases, len(p.aliases)-1); scopeErr != nil {
		return nil, scopeErr
	}
	p.postAlias = compileAliasedCond(spec.Post, p.aliases)

	for i := range steps {
		st := &steps[i]
		sp := &joinStepPlan{kind: st.Kind, side: i + 1}
		if sp.cond, err = compileAll("ON", st.Cond); err != nil {
			return nil, err
		}
		// A step's ON terms see the sides folded in up to and including its own. Naming a
		// later one is an error here rather than a silent undefined, which is what it would
		// otherwise read as -- and an ON clause that is quietly never true is a wrong
		// answer with no symptom.
		if err := refuseUnscopedSides("ON", st.Cond, p.aliases, sp.side); err != nil {
			return nil, err
		}
		sp.condAlias = compileAliasedCond(st.Cond, p.aliases)
		for j, e := range st.Equi {
			row, rerr := classad.ParseExpr(e.Left)
			if rerr != nil {
				return nil, fmt.Errorf("join equality %d of step %d, row half %q: %w", j, i, e.Left, rerr)
			}
			sd, serr := classad.ParseExpr(e.Right)
			if serr != nil {
				return nil, fmt.Errorf("join equality %d of step %d, table half %q: %w", j, i, e.Right, serr)
			}
			sp.equiRow = append(sp.equiRow, row)
			sp.equiSide = append(sp.equiSide, sd)
			sp.equiSideAttr = append(sp.equiSideAttr, bareAttrName(e.Right))
			// A bare row-half is unambiguous only while the row holds one side, and at
			// the first step that is exactly what it holds -- so there the half is read
			// against the FROM side's own ad, which is what the two-table spelling has
			// always meant by it. Past the first step it has to be qualified.
			// A row half may name a side a later step folds in at ANY step, and it reads
			// undefined if it does -- so the whole chain is checked, not only past the
			// first step.
			if err := refuseUnscopedSides("join equality", []string{e.Left}, p.aliases, sp.side-1); err != nil {
				return nil, err
			}
			bare := ""
			var al *aliasedValue
			if sp.side == 1 {
				// At the first step the row holds one side, so BOTH spellings are
				// unambiguous and both are accepted: a bare path, which is what the
				// two-table spelling has always written and which the row answers without
				// being decoded, and a qualified one, which a caller writing a chain will
				// naturally write at every step including this one. Accepting only the
				// bare one would be a silent wrong answer rather than an error, since a
				// qualified half read against the side's own ad is undefined and an
				// undefined key matches nothing.
				bare = bareAttrName(e.Left)
				if bare == "" && namesASide(e.Left, p.aliases) {
					al = compileAliasedValue(e.Left, p.aliases)
				}
			} else {
				if err := requireNamesASide(e.Left, p.aliases); err != nil {
					return nil, err
				}
				al = compileAliasedValue(e.Left, p.aliases)
			}
			sp.equiRowAttr = append(sp.equiRowAttr, bare)
			sp.rowAliased = append(sp.rowAliased, al)
		}
		sp.rowReadsOnlySide0 = rowHalvesReadOnlySide0(st.Equi, p.aliases, sp.side)
		p.steps = append(p.steps, sp)
	}
	return p, nil
}

// requireNamesASide rejects an equality's row half, past the first step, that names no side.
//
// Past the first step the row holds several sides, so an unqualified `ClusterId` does not say
// which one it means. It would read as an attribute of the composite row itself -- undefined
// -- which puts every row in the no-key bucket and matches nothing, with no error. Refusing
// says which side to name.
func requireNamesASide(src string, aliases []string) error {
	q, err := vm.ParseAliases(src, aliases)
	if err != nil || !q.AliasNative() {
		// Not something the alias compiler can read; it is evaluated against a composite of
		// the row, where the same ambiguity is at least evaluated consistently.
		return nil
	}
	if !namesASide(src, aliases) {
		return fmt.Errorf("join equality row half %q names no side; past the first step the "+
			"row holds several, so it must say which one (%s)", src, strings.Join(aliases, ", "))
	}
	return nil
}

// namesASide reports whether an expression qualifies at least one reference with a join
// alias, which is what makes it readable against the accumulated row rather than against one
// side's own ad.
func namesASide(src string, aliases []string) bool {
	q, err := vm.ParseAliases(src, aliases)
	if err != nil || !q.AliasNative() {
		return false
	}
	return len(q.AliasRefs()) > 0
}

// refuseUnscopedSides rejects a term that names a side not yet in the row, or that uses MY
// or TARGET where they no longer name anything definite.
//
// Both refusals are about a reference that WOULD evaluate, quietly, to the wrong thing. A
// forward reference reads undefined, so the term stops holding and the join returns too few
// rows with no error. MY and TARGET mean the first and second sides, which is a guess about
// which pair was meant once there are three -- the aliases are right there in the plan.
//
// MY and TARGET are NOT policed here, and that is a decision rather than an omission. They
// are refused past two tables by the planner, which is the only layer that ever gave them a
// side meaning. In the store a joined row's own attributes are the aliases, so MY.X and
// TARGET.X name an attribute of the composite and read undefined for ANY number of sides,
// two included -- there is no mapping here to become ambiguous. Detecting them soundly would
// mean walking an expression language this file does not own.
//
// A term the alias compiler will not take is passed over rather than refused: it is evaluated
// against a composite of the row, where a forward reference is undefined for the same reason
// it is here, and refusing on a reference this could not see would refuse valid plans.
func refuseUnscopedSides(what string, srcs, aliases []string, maxSide int) error {
	for _, src := range srcs {
		if strings.TrimSpace(src) == "" {
			continue
		}
		q, err := vm.ParseAliases(src, aliases)
		if err != nil || !q.AliasNative() {
			continue
		}
		for _, r := range q.AliasRefs() {
			if r.Alias > maxSide {
				return fmt.Errorf("%s term %q reads %s, which a later step folds in; "+
					"SQL does not allow a forward reference", what, src, aliases[r.Alias])
			}
		}
	}
	return nil
}

// rowHalvesReadOnlySide0 reports whether every row half of a step's equalities reads the FROM
// side and no other.
//
// A half at the first step is bare and reads the FROM side by definition. Past it, the test
// is on the references the half actually makes, not on its SHAPE: a probe evaluates the half
// rather than pattern-matching it, so any expression over the FROM side alone qualifies, and
// one reaching a side an earlier step folded in does not -- its value is not known until that
// step has run.
func rowHalvesReadOnlySide0(equi []JoinEqui, aliases []string, side int) bool {
	if len(equi) == 0 {
		return false
	}
	if side == 1 {
		// The row holds the FROM side and nothing else, so every half reads it whatever
		// shape the half is.
		return true
	}
	for _, e := range equi {
		q, err := vm.ParseAliases(e.Left, aliases)
		if err != nil || !q.AliasNative() {
			return false
		}
		refs := q.AliasRefs()
		if len(refs) == 0 {
			return false
		}
		for _, r := range refs {
			if r.Alias != 0 {
				return false
			}
		}
	}
	return true
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

// joinStage is one step with its hash built, ready to be probed by the rows so far.
type joinStage struct {
	step    *joinStepPlan
	buckets map[string][]*joinRow
	// all holds every row of the step's table when the step has no equality to hash on, so
	// each accumulated row is paired against all of them and the condition alone decides.
	all    []*joinRow
	hashed bool
}

// runJoin builds one hash per step and streams the FROM side through them, handing each
// surviving combination to mk for the caller's row form.
//
// The shape is a nested loop over the hashes rather than a fold that materializes each
// intermediate: one accumulated row exists at a time, held in `rows` and rewritten as the
// walk descends and backtracks. So the memory is the hashes plus one row, whatever the chain
// length, and a two-table join runs exactly the loop it ran before -- one hash, one level of
// descent.
//
// The hashes are built HERE rather than by the caller so that a limit met while building one
// is reported the way every other mid-stream limit is: in stats.Err, after the sequence has
// started.
func runJoin[T any](p *joinPlan, hashSeqs []iter.Seq[*joinRow], scanSeq iter.Seq[*joinRow],
	stats *JoinStats, mk func(p *joinPlan, rows []*joinRow) (T, bool)) iter.Seq[T] {
	return func(yield func(T) bool) {
		stages := make([]*joinStage, 0, len(hashSeqs))
		for i, seq := range hashSeqs {
			st, err := buildStage(p, p.steps[i], seq, stats)
			if err != nil {
				stats.Err = err
				return
			}
			stages = append(stages, st)
		}

		// rows is the accumulated row: one entry per side, nil where a side is not yet
		// folded in or was null-extended. Every compiled term indexes it by side, which is
		// what lets the condition read the sides where they already are.
		rows := make([]*joinRow, p.nSides())
		var walk func(depth int) (stop bool)
		walk = func(depth int) bool {
			if depth == len(stages) {
				return emitJoin(p, rows, stats, mk, yield)
			}
			st := stages[depth]
			candidates := st.all
			if st.hashed {
				k, ok := p.rowKey(st.step, rows)
				if !ok {
					candidates = nil // an undefined key matches nothing
				} else {
					candidates = st.buckets[k]
				}
			}
			matched := false
			for _, r := range candidates {
				stats.Pairs++
				if stats.Pairs > p.limits.MaxPairs {
					stats.Err = fmt.Errorf("the join examined more than %d candidate pairs; "+
						"it needs a more selective condition", p.limits.MaxPairs)
					return true
				}
				rows[st.step.side] = r
				// The hash only narrowed the candidates; the condition decides, in full,
				// with ClassAd's own semantics.
				if !p.condHolds(st.step, rows) {
					continue
				}
				// The pair matched. Whether the finished row then survives the post-filter
				// is a SEPARATE question, answered once at the end of the chain, and
				// conflating the two is a wrong answer: a matched row that WHERE drops
				// must not come back as a null-extended one.
				matched = true
				if walk(depth + 1) {
					return true
				}
			}
			rows[st.step.side] = nil
			// A side is null-extendable exactly when ITS OWN step is a LEFT join. Every
			// earlier side is preserved by every later step, so being preserved once is
			// being preserved throughout -- which is why this is one comparison and not a
			// walk back along the chain.
			if !matched && st.step.kind == JoinLeft {
				return walk(depth + 1)
			}
			return false
		}

		for l := range scanSeq {
			stats.ScanRows++
			rows[0] = l
			if walk(0) {
				return
			}
			if p.limit > 0 && stats.Rows >= p.limit {
				return
			}
		}
	}
}

// buildStage reads one step's table into its hash, or holds it whole when the step has no
// equality to hash on.
func buildStage(p *joinPlan, step *joinStepPlan, seq iter.Seq[*joinRow], stats *JoinStats) (*joinStage, error) {
	st := &joinStage{step: step, hashed: len(step.equiSide) > 0}
	if st.hashed {
		st.buckets = map[string][]*joinRow{}
	}
	for ad := range seq {
		// MaxHashRows bounds what is HELD, and every step holds its own table, so the
		// budget is spent across the chain rather than per step. A chain that would hold
		// more in total than one two-table join may hold is the thing this is bounding.
		if stats.HashRows >= p.limits.MaxHashRows {
			return nil, fmt.Errorf("the join would hold more than %d rows from %s; "+
				"narrow it with a constraint on that side", p.limits.MaxHashRows,
				p.aliases[step.side])
		}
		stats.HashRows++
		if !st.hashed {
			st.all = append(st.all, ad)
			continue
		}
		k, ok := p.sideKey(step, ad)
		if !ok {
			continue // an undefined key equals nothing: ClassAd == propagates undefined
		}
		st.buckets[k] = append(st.buckets[k], ad)
	}
	return st, nil
}

// emit forms the joined row, applies the post-filter and yields it. It reports only whether
// the join should STOP -- whether the row survived the filter is deliberately not reported,
// because the one caller that used to branch on it drew the wrong conclusion from it.
func emitJoin[T any](p *joinPlan, rows []*joinRow, stats *JoinStats,
	mk func(p *joinPlan, rows []*joinRow) (T, bool), yield func(T) bool) (stop bool) {
	if !p.postHolds(rows) {
		return false
	}
	row, ok := mk(p, rows)
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

// postHolds reports whether every residual WHERE term is TRUE for this row.
//
// It reads the sides AS READ, not through the output projection. That distinction is the
// whole point of this existing separately from emit: a WHERE is not a projection, and
// `WHERE e.MemoryUsage > 1000` has to work whether or not the caller asked to SEE
// MemoryUsage. Filtering the projected row instead -- which is what this used to do, by
// building the row first -- made such a term read UNDEFINED and silently match nothing, so a
// join answered a perfectly ordinary query with no rows and no error.
//
// It runs ONCE, on the finished row, because that is where SQL puts it: the FROM clause
// produces rows and WHERE filters them. An absent side is an unmatched LEFT row, and every
// reference through its alias is undefined, exactly as it is for the ON condition. Dropping
// the row here does NOT bring it back null-extended: run decides that from whether the pair
// MATCHED, which is a separate question answered before this.
func (p *joinPlan) postHolds(rows []*joinRow) bool {
	if len(p.post) == 0 {
		return true
	}
	if p.postAlias != nil {
		if held, ok := p.postAlias.holds(rows); ok {
			return held
		}
	}
	// A term the alias compiler would not take reads the sides out of a composite, which is
	// built WITHOUT the output projection for the reason above. Building it decodes every
	// side, which is exactly what the wire form avoids -- so it is built only here, for the
	// terms that genuinely need a record.
	row := p.recordRow(rows)
	for _, ex := range p.post {
		if !isTrue(ex.Eval(row)) {
			return false
		}
	}
	return true
}

// recordRow builds the joined row from the sides AS READ, with no output projection applied.
// It is what a term that needs a real record is evaluated against.
func (p *joinPlan) recordRow(rows []*joinRow) *classad.ClassAd {
	row := classad.New()
	for i, r := range rows {
		if r != nil {
			row.InsertAttrClassAd(p.aliases[i], r.record())
		}
	}
	return row
}

// composite builds the joined row: one ClassAd whose members are the sides under their
// aliases. An absent side -- one a later step has not folded in, or the null-extended half of
// an unmatched LEFT row -- is simply missing, so every reference through its alias evaluates
// to undefined.
func (p *joinPlan) composite(rows []*joinRow) *classad.ClassAd {
	row := classad.New()
	for i, r := range rows {
		if r != nil {
			row.InsertAttrClassAd(p.aliases[i], r.member(p.projects[i]))
		}
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

// condHolds reports whether every term of this step's condition is TRUE for the row as it
// stands, with this step's side just filled in.
//
// Only a true boolean keeps a pair, which is what makes splitting the condition into
// per-side constraints and this residual sound even though ClassAd's && is left-biased and
// not commutative: the only way `A && B` is true is for both to be true, which does not
// depend on their order. The VALUE of the conjunction is order-dependent; the match set is
// not.
func (p *joinPlan) condHolds(step *joinStepPlan, rows []*joinRow) bool {
	if len(step.cond) == 0 {
		return true
	}
	if step.condAlias != nil {
		if held, ok := step.condAlias.holds(rows); ok {
			return held
		}
	}
	row := p.recordRow(rows)
	for _, ex := range step.cond {
		if !isTrue(ex.Eval(row)) {
			return false
		}
	}
	return true
}

// rowKey renders the accumulated row's halves of this step's equalities as a bucket key.
//
// Three ways to read a half, cheapest first: a bare attribute of the first side, which the
// row answers without being decoded and which is the only shape the two-table spelling ever
// had; a qualified reference the alias compiler took, read out of the sides directly; and
// anything else, evaluated against a composite of the row so far.
func (p *joinPlan) rowKey(step *joinStepPlan, rows []*joinRow) (string, bool) {
	var b strings.Builder
	for i := range step.equiRow {
		var v classad.Value
		switch {
		case step.equiRowAttr[i] != "":
			if rows[0] == nil {
				return "", false
			}
			v = rows[0].eval(step.equiRowAttr[i])
		case step.rowAliased[i] != nil:
			got, ok := step.rowAliased[i].eval(rows)
			if !ok {
				v = step.equiRow[i].Eval(p.rowHalfScope(step, rows))
			} else {
				v = got
			}
		default:
			v = step.equiRow[i].Eval(p.rowHalfScope(step, rows))
		}
		if !appendKeyValue(&b, v) {
			return "", false
		}
		if i < len(step.equiRow)-1 {
			b.WriteByte(0)
		}
	}
	return b.String(), true
}

// rowHalfScope is the ad an equality's row half is evaluated against when it is not read
// through the alias machinery.
//
// At the FIRST step the row holds one side, and a half written there is written against THAT
// side's own ad -- `ClusterId`, not `h.ClusterId` -- which is what the two-table spelling has
// always meant. Past the first step the half is qualified and reads the composite.
func (p *joinPlan) rowHalfScope(step *joinStepPlan, rows []*joinRow) *classad.ClassAd {
	if step.side == 1 {
		return rows[0].record()
	}
	return p.recordRow(rows)
}

// sideKey renders one row of this step's own table as a bucket key, from its halves of the
// equalities.
func (p *joinPlan) sideKey(step *joinStepPlan, row *joinRow) (string, bool) {
	var b strings.Builder
	for i := range step.equiSide {
		// An equality half is a bare attribute name in every plan that drives a hash, and
		// then the row answers it without being decoded. Anything else is an expression
		// that has to be evaluated in the context of its own ad.
		var v classad.Value
		if name := step.equiSideAttr[i]; name != "" {
			v = row.eval(name)
		} else {
			v = step.equiSide[i].Eval(row.record())
		}
		if !appendKeyValue(&b, v) {
			return "", false
		}
		if i < len(step.equiSide)-1 {
			b.WriteByte(0)
		}
	}
	return b.String(), true
}

// appendKeyValue writes one value's bucket key, reporting false when it has none.
//
// It must not separate two values that would compare equal, which is why a number is
// rendered through float64 whatever its stored width and a string is folded: ClassAd compares
// 1 and 1.0 equal, and strings case-insensitively. Anything with no scalar form -- undefined,
// error, a list, a nested ad -- equals nothing and reports false.
//
// Each accessor's error is consulted rather than discarded: a value that claims a kind and
// then will not produce it has no key, and hashing the zero it returns would put it in a
// bucket it does not belong to.
func appendKeyValue(b *strings.Builder, v classad.Value) bool {
	switch {
	case v.IsBool():
		x, err := v.BoolValue()
		if err != nil {
			return false
		}
		if x {
			b.WriteString("n1")
		} else {
			b.WriteString("n0")
		}
	case v.IsInteger():
		n, err := v.IntValue()
		if err != nil {
			return false
		}
		b.WriteString("n")
		b.WriteString(strconv.FormatFloat(float64(n), 'g', -1, 64))
	case v.IsReal():
		f, err := v.RealValue()
		if err != nil {
			return false
		}
		b.WriteString("n")
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	case v.IsString():
		str, err := v.StringValue()
		if err != nil {
			return false
		}
		b.WriteString("s")
		b.WriteString(strings.ToLower(str))
	default:
		return false
	}
	return true
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
