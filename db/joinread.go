package db

import (
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/parser"
)

// READING ONLY WHAT A JOIN NEEDS.
//
// Both sides of a join were read whole. That is not a detail of how rows are carried, it is
// most of what a join costs: on a history-to-epoch_history join over ads of the width those
// tables really have, reading the two sides accounted for roughly four fifths of the whole
// operation, and the rows the query asked for were a handful of attributes out of eighty.
//
// So each side is read with a projection, and the rows come back as wire-form rows that are
// decoded into the narrow ads the rest of the join already expects. Nothing downstream
// changes: the pair loop, the condition and the output projection all see what they saw.
//
// WHAT MAKES THIS SAFE, which is the whole difficulty. A read projection is not the output
// projection. An attribute that no output carries is still needed if the CONDITION reads it,
// and dropping it does not raise an error -- it reads UNDEFINED, the condition stops holding,
// and the join returns too few rows, or, for a LEFT join, the same count with every right
// side gone. A wrong projection here is a silently wrong answer, not a failure.
//
// It is therefore computed from every expression that can read a side, and any expression
// this cannot account for in full abandons the projection for that side and reads the whole
// ad. Declining costs the old speed; guessing costs the right answer.

// forceWholeRead makes planJoinRead decline every projection, so a test can run the same
// join both ways and compare the rows. It is the oracle's switch and nothing else: there is
// no configuration that reaches it, because reading whole ads is not a behaviour anyone
// should be choosing, only the answer this falls back to.
//
// A test that sets it must not run in parallel with another that does not.
var forceWholeRead bool

// joinReadProjection is what each side must be read with, indexed by side: a list of
// attribute names, or nil for "the whole ad", which is both the safe answer and the old
// behaviour.
type joinReadProjection [][]string

// planJoinRead works out what each side has to carry.
//
// The sources of a reference to a side, which must be complete or the result is wrong:
//
//   - the output projection, which is what the caller asked to see;
//   - every step's ON terms and the residual WHERE, written over the joined row, where a
//     side is read as `alias.Attr`;
//   - each equality's two halves: the one reading the accumulated row, which names a side
//     the same way the ON terms do, and the one reading the step's own table, written
//     against that table's ad;
//   - each band's attribute, read from the step's own table, and its two bounds, read over
//     the accumulated row. A band is not in any ON term -- it is declared beside them -- so
//     an attribute windowed on and nowhere else is exactly the attribute this would drop.
//
// A nil output projection on a side means the caller wants that side's whole ad, so there is
// nothing to narrow it to and it is read whole.
func planJoinRead(spec JoinSpec) joinReadProjection {
	sides, steps, err := spec.chain()
	if err != nil {
		return make(joinReadProjection, 2)
	}
	whole := make(joinReadProjection, len(sides))
	if forceWholeRead {
		return whole
	}
	// A semi or anti step's side is not in the row, so its output projection is empty
	// whatever the caller wrote -- compileJoin refuses a Project on one. Standing that
	// empty projection in here is what narrows its read to the attributes its own
	// condition tests, which for an anti join against a wide table is the difference
	// between reading one attribute and reading every ad whole.
	project := make([][]string, len(sides))
	for i, sd := range sides {
		project[i] = sd.Project
	}
	anyProjected := false
	for i := range steps {
		if !steps[i].Kind.carriesSide() && i+1 < len(project) {
			project[i+1] = []string{}
			anyProjected = true
		}
	}
	for _, pr := range project {
		if pr != nil {
			anyProjected = true
		}
	}
	if !anyProjected {
		return whole
	}
	aliases := make([]string, len(sides))
	for i, sd := range sides {
		aliases[i] = sd.Alias
	}
	sets := make([]*attrSet, len(sides))
	for i := range sets {
		sets[i] = newAttrSet()
	}

	ok := make([]bool, len(sides))
	for i := range ok {
		ok[i] = true
	}

	// A term written over the joined row can read ANY side, so one declined term takes
	// EVERY side's projection away: the term may have been reaching into any of them.
	overRow := append([]string{}, spec.Post...)
	for i := range steps {
		st := &steps[i]
		overRow = append(overRow, st.Cond...)
	}
	for _, src := range overRow {
		if strings.TrimSpace(src) == "" {
			continue
		}
		q, qerr := vm.ParseAliases(src, aliases)
		if qerr != nil || !q.AliasNative() {
			return whole
		}
		for _, r := range q.AliasRefs() {
			if r.Alias < 0 || r.Alias >= len(sets) {
				return whole
			}
			sets[r.Alias].add(r.Name)
		}
	}

	// An equality's halves are NOT terms over the joined row, and the difference matters:
	// each half reads a known part of it, so a half this cannot account for costs only
	// what that half could have read rather than every side's projection.
	for i := range steps {
		st := &steps[i]
		side := sideIndexOf(sides, st.Side.Alias)
		for _, e := range st.Equi {
			// The table half reads this step's own table, written against that table's ad.
			if !addSideRefs(sets[side], e.Right) {
				ok[side] = false
			}
			if side == 1 {
				// At the first step the row half may be bare -- written against the FROM
				// side's own ad -- or qualified. Try the qualified reading first, so a
				// chain that qualifies every step is read as narrowly as one that does
				// not; fall back to the bare reading, which is the two-table spelling.
				if q, qerr := vm.ParseAliases(e.Left, aliases); qerr == nil && q.AliasNative() {
					if refs := q.AliasRefs(); len(refs) > 0 {
						for _, r := range refs {
							if r.Alias < 0 || r.Alias >= len(sets) {
								return whole
							}
							sets[r.Alias].add(r.Name)
						}
						continue
					}
				}
				if !addSideRefs(sets[0], e.Left) {
					ok[0] = false
				}
				continue
			}
			// Past the first step the row half is qualified over the row, and can reach
			// any side already folded in -- so a half this cannot read takes those sides'
			// projections and leaves the later ones narrow.
			q, qerr := vm.ParseAliases(e.Left, aliases)
			if qerr != nil || !q.AliasNative() {
				for i := 0; i < side; i++ {
					ok[i] = false
				}
				continue
			}
			refs := q.AliasRefs()
			if len(refs) == 0 {
				for i := 0; i < side; i++ {
					ok[i] = false
				}
				continue
			}
			for _, r := range refs {
				if r.Alias < 0 || r.Alias >= len(sets) {
					return whole
				}
				sets[r.Alias].add(r.Name)
			}
		}
		// A band reads the same two places an equality does, and is accounted for the same
		// way: the attribute from the step's own table, the bounds over the row so far.
		for _, b := range st.Band {
			if name := strings.TrimSpace(b.Attr); name == "" || !addSideRefs(sets[side], name) {
				ok[side] = false
			}
			for _, src := range []string{b.Lo, b.Hi} {
				if strings.TrimSpace(src) == "" {
					continue
				}
				if !addRowHalfRefs(sets, aliases, src) {
					return whole
				}
			}
		}
	}

	out := make(joinReadProjection, len(sides))
	for i := range sides {
		if ok[i] {
			out[i] = sets[i].union(project[i])
		}
	}
	return out
}

// sideIndexOf finds a side by its alias. The chain built the alias list, so a miss cannot
// happen; it answers 0 rather than panicking, which costs a wider read rather than a crash.
func sideIndexOf(sides []JoinSide, alias string) int {
	for i, s := range sides {
		if strings.EqualFold(s.Alias, alias) {
			return i
		}
	}
	return 0
}

// addRowHalfRefs accounts for a term written over the accumulated row -- a band's bound --
// by adding what it reads to the sides it reads from.
//
// It returns false when the term cannot be accounted for at all and every side must be read
// whole. A bare reference is taken as the FROM side's, which is what it means at the first
// step and what rowHalfScope evaluates it against; past the first step compileBands has
// already refused a term naming no side it may read.
func addRowHalfRefs(sets []*attrSet, aliases []string, src string) bool {
	q, err := vm.ParseAliases(src, aliases)
	if err != nil || !q.AliasNative() {
		return false
	}
	refs := q.AliasRefs()
	if len(refs) == 0 {
		// No qualified reference in it: either a constant, which reads nothing, or the
		// bare spelling, which reads the FROM side. The two have to be told apart, and
		// addSideRefs cannot do it -- it answers "is this a bare attribute name", so a
		// constant comes back false, which would widen every side's read to the whole ad
		// over a bound that reads nothing at all.
		//
		// SelfRefs names what the bare form reads and returns an empty list for a
		// constant, which is the answer "reads nothing" rather than "could not be read".
		// partialSafe is false where the refs cannot be known statically -- an eval() in
		// the bound -- and that one really is unaccountable.
		ex, perr := parser.ParseExpr(src)
		if perr != nil {
			return false
		}
		names, safe := vm.SelfRefsSafe(ex)
		if !safe {
			return false
		}
		for _, n := range names {
			sets[0].add(n)
		}
		return true
	}
	for _, r := range refs {
		if r.Alias < 0 || r.Alias >= len(sets) {
			return false
		}
		sets[r.Alias].add(r.Name)
	}
	return true
}

// addSideRefs adds the attributes src reads on its own side, reporting false if it cannot
// account for the whole expression.
//
// Deliberately narrow: a bare attribute name, which is what an equality half is in every
// plan that drives a hash. Anything else declines rather than being analysed, because the
// cost of declining is a wider read and the cost of analysing something wrongly is a wrong
// answer. An unknown node kind lands in the default and declines, so this stays correct if
// the expression language gains one.
func addSideRefs(into *attrSet, src string) bool {
	if strings.TrimSpace(src) == "" {
		return true
	}
	expr, err := parser.ParseExpr(src)
	if err != nil {
		return false
	}
	return walkSideRefs(into, expr)
}

func walkSideRefs(into *attrSet, e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.AttributeReference:
		// A scoped reference names another ad, not this side, so this cannot say what to
		// read; an absolute or TARGET. reference in an equality half is not something to
		// resolve by guessing.
		if v.Scope != ast.NoScope && v.Scope != ast.MyScope {
			return false
		}
		// The clock is not an attribute of the record, and an ad that happens to define
		// CurrentTime would change which it is. Not worth the doubt for a read plan.
		if classad.IsCurrentTimeName(v.Name) {
			return false
		}
		into.add(v.Name)
		return true
	case *ast.ParenExpr:
		return walkSideRefs(into, v.Inner)
	default:
		return false
	}
}

// attrSet collects attribute names case-insensitively while keeping the first spelling seen,
// because a projection is matched case-insensitively but is clearer in the caller's casing.
type attrSet struct {
	seen  map[string]struct{}
	names []string
}

func newAttrSet() *attrSet { return &attrSet{seen: map[string]struct{}{}} }

func (s *attrSet) add(name string) {
	k := strings.ToLower(name)
	if _, dup := s.seen[k]; dup {
		return
	}
	s.seen[k] = struct{}{}
	s.names = append(s.names, name)
}

// union returns the set plus the output projection. A nil output projection means the whole
// ad is wanted out, so there is nothing to narrow to and union reports nil to say so.
func (s *attrSet) union(project []string) []string {
	if project == nil {
		return nil
	}
	out := newAttrSet()
	for _, n := range s.names {
		out.add(n)
	}
	for _, n := range project {
		out.add(n)
	}
	if len(out.names) == 0 {
		// Nothing is read from this side at all. An empty projection would mean "the whole
		// ad" to the scan, so ask for one attribute that cannot exist instead of widening
		// the read back to everything by accident.
		return []string{"__join_reads_nothing"}
	}
	return out.names
}

// NARROWING ONE SIDE'S READ.
//
// A wire-form row is assembled from the stored bytes by slice copies, with no intern table
// and no ClassAd built, and is then decoded into the narrow ad the join wants. That is two
// steps where there was one, and it is still much the cheaper: what it skips is decoding the
// attributes nobody asked for, which on a real job ad is most of them.
//
// The REFS form is the one to use, not the plain one. A projected attribute whose value is an
// expression -- which job ads are full of -- reads the attributes that expression names, and a
// row carrying the expression without them evaluates it to UNDEFINED. The plain form
// reproduces HTCondor's projection protocol; this one carries the transitive closure so each
// row evaluates to the same thing it would have inside the whole ad.
//
// Unredacted, matching the whole-ad read it replaces exactly. A join runs inside the store
// against tables the caller already reached; redaction belongs to the relay that ships the
// result out, and applying it here would change what a join means depending on how its rows
// were read.
//
// Both narrow* return (_, false) to mean "not narrowed, read it the old way", which is what
// every refusal does: no projection was planned, the store cannot serve wire rows, or a row
// did not decode. None of those is an error, because the whole-ad read answers all of them.

func narrowTableRead(d *DB, constraint string, read []string) (iter.Seq[*joinRow], bool) {
	if len(read) == 0 {
		return nil, false
	}
	rows, err := d.QueryRawWireRefs(constraint, read, false)
	if err != nil {
		return nil, false
	}
	return wireRows(rows), true
}

func narrowArchiveRead(a *ArchiveTable, constraint string, read []string) (iter.Seq[*joinRow], bool) {
	if len(read) == 0 {
		return nil, false
	}
	rows, err := a.QueryRawWireRefs(constraint, read, false)
	if err != nil {
		return nil, false
	}
	return wireRows(rows), true
}
