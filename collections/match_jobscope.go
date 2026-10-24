package collections

import (
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// jobScope is the job side of a match, computed once per job: which of the job's
// attributes can read the match target, and the values of those that cannot.
//
// A job attribute reads the target when its expression has a TARGET.x reference, an
// unscoped reference to a name the job does not define (ClassAd matchmaking resolves
// that in the target), or an eval() call (whose references are not statically known),
// directly or through another job attribute it references. Such an attribute has no
// value independent of the slot: evaluating it once with no target set would bake a
// per-slot value as a constant. One that reads a CurrentTime the job does not define is
// treated the same way: its value is the clock's, which a baked value would freeze. Only target-independent attributes are evaluated
// here; every match path treats a target-dependent one as unknown until evaluated
// against the slot.
type jobScope struct {
	vals  map[string]classad.Value // folded name -> value, target-independent attributes only
	dep   map[string]bool          // folded names of target-dependent attributes
	reads map[string]*jobAttrReads // folded name -> what the attribute's expression reads
}

// jobAttrReads is what one job expression reads, split by where each reference
// resolves when the job is evaluated against a slot.
type jobAttrReads struct {
	slot   []string // slot attributes: TARGET.x, and unscoped x the job does not define
	job    []string // folded names of job attributes: MY.x, and unscoped x the job defines
	opaque bool     // calls eval() or has an unrecognized node: reads are not statically known
	clock  bool     // reads a CurrentTime the job does not define: the time, not the slot's
}

// jobRefKind classifies a job-side attribute reference.
type jobRefKind uint8

const (
	jobRefConst  jobRefKind = iota // a target-independent attribute: its value is known
	jobRefDep                      // a target-dependent attribute: its value varies by slot
	jobRefAbsent                   // the job does not define the attribute
)

// jobValues builds the jobScope for job. It evaluates only the target-independent
// attributes, with no target set, so the shared job is not mutated concurrently.
func jobValues(job *classad.ClassAd) *jobScope {
	attrs := job.AST().Attributes
	js := &jobScope{
		vals:  make(map[string]classad.Value, len(attrs)),
		reads: make(map[string]*jobAttrReads, len(attrs)),
	}
	for _, a := range attrs {
		js.reads[strings.ToLower(a.Name)] = &jobAttrReads{}
	}
	has := func(name string) bool {
		_, ok := js.reads[strings.ToLower(name)]
		return ok
	}
	for _, a := range attrs {
		collectJobReads(a.Value, has, js.reads[strings.ToLower(a.Name)])
	}
	js.dep = targetDependent(js.reads)

	orig := job.GetTarget()
	job.SetTarget(nil)
	for _, a := range attrs {
		name := strings.ToLower(a.Name)
		if !js.dep[name] {
			js.vals[name] = job.EvaluateAttr(a.Name)
		}
	}
	job.SetTarget(orig)
	return js
}

// targetDependent returns the folded names of the attributes whose value can depend on
// the target: those reading a slot attribute or eval() directly, and every attribute
// that references one of them.
func targetDependent(reads map[string]*jobAttrReads) map[string]bool {
	dep := map[string]bool{}
	users := map[string][]string{} // job attribute -> attributes that reference it
	var work []string
	for name, r := range reads {
		for _, ref := range r.job {
			users[ref] = append(users[ref], name)
		}
		// An attribute that reads the clock is not baked either: one evaluation here and
		// another during the match could read different times.
		if len(r.slot) > 0 || r.opaque || r.clock {
			dep[name] = true
			work = append(work, name)
		}
	}
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		for _, u := range users[name] {
			if !dep[u] {
				dep[u] = true
				work = append(work, u)
			}
		}
	}
	return dep
}

// collectJobReads walks a job expression, appending its references to r. has reports
// whether the job defines an attribute. An unscoped reference inside a nested record may
// resolve in that record; classifying it by the job's attributes over-approximates the
// reads, which only costs the fast path.
func collectJobReads(e ast.Expr, has func(string) bool, r *jobAttrReads) {
	switch n := e.(type) {
	case nil:
	case *ast.AttributeReference:
		switch n.Scope {
		case ast.TargetScope:
			r.slot = append(r.slot, n.Name)
		case ast.ParentScope:
			// A top-level job ad has no parent scope: always undefined.
		case ast.MyScope:
			if has(n.Name) {
				r.job = append(r.job, strings.ToLower(n.Name))
			} else if classad.IsCurrentTimeName(n.Name) {
				r.clock = true
			}
		default:
			if has(n.Name) {
				r.job = append(r.job, strings.ToLower(n.Name))
			} else if classad.IsCurrentTimeName(n.Name) {
				r.clock = true // the time, whatever the slot holds
			} else {
				r.slot = append(r.slot, n.Name) // falls through to the target
			}
		}
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral, *ast.BooleanLiteral,
		*ast.UndefinedLiteral, *ast.ErrorLiteral:
	case *ast.ParenExpr:
		collectJobReads(n.Inner, has, r)
	case *ast.UnaryOp:
		collectJobReads(n.Expr, has, r)
	case *ast.BinaryOp:
		collectJobReads(n.Left, has, r)
		collectJobReads(n.Right, has, r)
	case *ast.ConditionalExpr:
		collectJobReads(n.Condition, has, r)
		collectJobReads(n.TrueExpr, has, r)
		collectJobReads(n.FalseExpr, has, r)
	case *ast.ElvisExpr:
		collectJobReads(n.Left, has, r)
		collectJobReads(n.Right, has, r)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			collectJobReads(el, has, r)
		}
	case *ast.RecordLiteral:
		if n.ClassAd != nil {
			for _, a := range n.ClassAd.Attributes {
				collectJobReads(a.Value, has, r)
			}
		}
	case *ast.FunctionCall:
		if strings.EqualFold(n.Name, "eval") {
			r.opaque = true
		}
		for _, a := range n.Args {
			collectJobReads(a, has, r)
		}
	case *ast.SelectExpr:
		collectJobReads(n.Record, has, r)
	case *ast.SubscriptExpr:
		collectJobReads(n.Container, has, r)
		collectJobReads(n.Index, has, r)
	default:
		r.opaque = true
	}
}

// lookup classifies a job-side reference to name and, for a target-independent
// attribute, returns its value. A nil jobScope (or one with nil maps) reports every
// name absent.
func (js *jobScope) lookup(name string) (classad.Value, jobRefKind) {
	if js == nil {
		return classad.NewUndefinedValue(), jobRefAbsent
	}
	fold := strings.ToLower(name)
	if v, ok := js.vals[fold]; ok {
		return v, jobRefConst
	}
	if js.dep[fold] {
		return classad.NewUndefinedValue(), jobRefDep
	}
	return classad.NewUndefinedValue(), jobRefAbsent
}

// slotReads returns the slot attributes that evaluating e in the job's scope can read:
// its own TARGET references and unscoped references to names the job does not define,
// plus those of every job attribute it reads, transitively. ok is false when any of
// them calls eval(), so the reads are not statically known.
func (js *jobScope) slotReads(e ast.Expr) (names []string, ok bool) {
	root := &jobAttrReads{}
	has := func(name string) bool {
		if js == nil {
			return false
		}
		_, ok := js.reads[strings.ToLower(name)]
		return ok
	}
	collectJobReads(e, has, root)
	return js.closeReads(root, nil)
}

// attrSlotReads is slotReads for the job attribute with the given folded name.
func (js *jobScope) attrSlotReads(fold string) (names []string, ok bool) {
	r := js.reads[fold]
	if r == nil {
		return nil, true
	}
	return js.closeReads(r, map[string]bool{fold: true})
}

// closeReads follows root's job references transitively, collecting slot reads.
func (js *jobScope) closeReads(root *jobAttrReads, seenJob map[string]bool) (names []string, ok bool) {
	ok = !root.opaque
	seenSlot := map[string]bool{}
	add := func(r *jobAttrReads) {
		for _, s := range r.slot {
			if f := strings.ToLower(s); !seenSlot[f] {
				seenSlot[f] = true
				names = append(names, s)
			}
		}
	}
	add(root)
	if seenJob == nil {
		seenJob = map[string]bool{}
	}
	work := append([]string(nil), root.job...)
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		if seenJob[name] || js == nil || !js.dep[name] {
			continue // a target-independent attribute reads no slot attribute
		}
		seenJob[name] = true
		r := js.reads[name]
		if r == nil {
			continue
		}
		if r.opaque {
			ok = false
		}
		add(r)
		work = append(work, r.job...)
	}
	return names, ok
}

// depAttr is a target-dependent job attribute and the slot attributes it reads
// (transitively), for building a partial slot ad that the job side can be evaluated
// against.
type depAttr struct {
	name   string   // folded attribute name
	reads  []string // slot attributes it reads
	opaque bool     // reads are not statically known (eval)
}

// depAttrs lists the job's target-dependent attributes with their slot reads, sorted by
// name for determinism.
func (js *jobScope) depAttrs() []depAttr {
	if js == nil || len(js.dep) == 0 {
		return nil
	}
	out := make([]depAttr, 0, len(js.dep))
	for name := range js.dep {
		reads, ok := js.attrSlotReads(name)
		out = append(out, depAttr{name: name, reads: reads, opaque: !ok})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// slotFrameRef rewrites a job-side attribute reference (MY.x, unscoped x, PARENT.x) for
// a predicate over the slot. A target-independent attribute with a scalar, undefined or
// error value becomes that literal. Anything else whose value is not known without the
// slot -- a target-dependent attribute, or a list/record value -- becomes TARGET.x:
// in the slot's frame that is exactly the job's attribute, and it is opaque to constant
// folding and index probes, so the conjunct holding it yields no probe. An unscoped
// name the job does not define resolves in the slot, so it becomes the slot's own
// attribute; MY.x of an undefined name and PARENT.x (a top-level job has no parent) are
// undefined. absentUnscoped reports the unscoped-absent case so a caller can substitute
// its own approximation.
func slotFrameRef(n *ast.AttributeReference, js *jobScope) (e ast.Expr, absentUnscoped bool) {
	if n.Scope == ast.ParentScope {
		return &ast.UndefinedLiteral{}, false
	}
	v, kind := js.lookup(n.Name)
	switch kind {
	case jobRefConst:
		if lit := valueToLiteral(v); lit != nil {
			return lit, false
		}
		if v.IsUndefined() {
			return &ast.UndefinedLiteral{}, false
		}
		if v.IsError() {
			return &ast.ErrorLiteral{}, false
		}
		return &ast.AttributeReference{Name: n.Name, Scope: ast.TargetScope}, false
	case jobRefDep:
		return &ast.AttributeReference{Name: n.Name, Scope: ast.TargetScope}, false
	}
	if classad.IsCurrentTimeRef(n) {
		// The job does not define CurrentTime, yet it is not undefined: unscoped or MY, it
		// is the current time whatever the slot holds. No slot-frame form says that (the
		// slot's own CurrentTime is the slot's value when it has one), so it stays the
		// job's (opaque) reference.
		return &ast.AttributeReference{Name: n.Name, Scope: ast.TargetScope}, false
	}
	if n.Scope == ast.MyScope {
		return &ast.UndefinedLiteral{}, false
	}
	return &ast.AttributeReference{Name: n.Name, Scope: ast.NoScope}, true
}

// refersToJob reports whether a slot-frame predicate (from rewriteForSlot) still holds
// a reference to the job (TARGET.x), i.e. its value is not determined by the slot alone.
func refersToJob(e ast.Expr) bool {
	found := false
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		if found || x == nil {
			return
		}
		switch n := x.(type) {
		case *ast.AttributeReference:
			found = n.Scope == ast.TargetScope
		case *ast.ParenExpr:
			walk(n.Inner)
		case *ast.UnaryOp:
			walk(n.Expr)
		case *ast.BinaryOp:
			walk(n.Left)
			walk(n.Right)
		case *ast.ConditionalExpr:
			walk(n.Condition)
			walk(n.TrueExpr)
			walk(n.FalseExpr)
		case *ast.ElvisExpr:
			walk(n.Left)
			walk(n.Right)
		case *ast.FunctionCall:
			for _, a := range n.Args {
				walk(a)
			}
		case *ast.ListLiteral:
			for _, el := range n.Elements {
				walk(el)
			}
		case *ast.SubscriptExpr:
			walk(n.Container)
			walk(n.Index)
		case *ast.SelectExpr:
			walk(n.Record)
		case *ast.RecordLiteral:
			found = true // conservatively opaque
		}
	}
	walk(e)
	return found
}
