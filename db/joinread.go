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

// joinReadProjection is what each side must be read with: a list of attribute names, or nil
// for "the whole ad", which is both the safe answer and the old behaviour.
type joinReadProjection struct {
	left, right []string
}

// planJoinRead works out what each side has to carry.
//
// The sources of a reference to a side, which must be complete or the result is wrong:
//
//   - the output projection, which is what the caller asked to see;
//   - the ON terms and the residual WHERE, written over the joined row, where a side is
//     read as `alias.Attr`;
//   - that side's half of each equality, written against the side's own ad, which the hash
//     evaluates before any pair exists.
//
// A nil output projection means the caller wants the whole ad out, so there is nothing to
// narrow to and the side is read whole.
func planJoinRead(spec JoinSpec) joinReadProjection {
	var p joinReadProjection
	if forceWholeRead {
		return p
	}
	if spec.Left.Project == nil && spec.Right.Project == nil {
		return p
	}
	aliases := []string{spec.Left.Alias, spec.Right.Alias}
	left, right := newAttrSet(), newAttrSet()
	sides := [2]*attrSet{left, right}

	// The ON and WHERE terms read either side, so one declined term takes BOTH sides'
	// projections away: the term may have been reaching into either.
	for _, src := range append(append([]string{}, spec.Cond...), spec.Post...) {
		q, err := vm.ParseAliases(src, aliases)
		if err != nil || !q.AliasNative() {
			return joinReadProjection{}
		}
		for _, r := range q.AliasRefs() {
			if r.Alias < 0 || r.Alias >= len(sides) {
				return joinReadProjection{}
			}
			sides[r.Alias].add(r.Name)
		}
	}

	// Each equality half reads only its own side, so a half this cannot account for costs
	// that side's projection and leaves the other narrow.
	leftOK, rightOK := true, true
	for _, e := range spec.Equi {
		if !addSideRefs(left, e.Left) {
			leftOK = false
		}
		if !addSideRefs(right, e.Right) {
			rightOK = false
		}
	}

	if leftOK {
		p.left = sides[0].union(spec.Left.Project)
	}
	if rightOK {
		p.right = sides[1].union(spec.Right.Project)
	}
	return p
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
