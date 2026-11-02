package collections

import (
	"math"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// What an attribute actually HOLDS, as opposed to how the schema stores it.
//
// SchemaFit answers a storage question: of the fields the schema carries, how often does a
// value miss its fixed slot. That is the right measure for deciding a layout, and it is not
// enough to decide a TYPE, for three reasons:
//
//   - it reports only schema fields, so an attribute in the cold tail -- a candidate for
//     promotion, or for a consumer that needs it typed -- is invisible;
//   - it collapses every escape into one rate, so "3% escaped" does not distinguish a string
//     among the integers (a different type) from an integer too wide for the slot (the same
//     type, a narrow slot) from a NaN (the same type, excluded from ordering);
//   - it reports no value bounds, so there is no way to tell whether an integer column's
//     values survive a conversion to a float type.
//
// A consumer that has to publish one declared type per attribute needs all three. So this
// profiles the SAMPLE rather than the schema: every attribute of every sampled record,
// classified by wire.ClassifyNode, with exact bounds accumulated per numeric kind.
//
// STORED CLASSES, AND EVALUATED ONES BESIDE THEM. The default measurement reports what the
// records HOLD. An attribute stored as an expression is reported as wire.ClassExpr even though
// evaluating it would yield a scalar -- on a real pool, MemoryUsage is an expression in almost
// every ad and an integer in a few, and its evaluated type is integer throughout. A consumer
// whose rule is "evaluate first, then type" therefore cannot read a type decision off these
// counts for any attribute with a ClassExpr share, and reading them anyway is how such an
// attribute ends up with no declared type at all despite being uniformly an integer.
//
// AttrTypeProfilesWith(Evaluate: true) adds that distribution, as a second AttrValueCounts over
// the same cells. It is the more expensive measurement -- an expression has to be resolved
// against its record rather than classified from one node -- which is why it is asked for
// rather than always taken. What the stored counts answer on their own is whether an attribute
// IS expression-valued and how often, which is what decides whether to ask.
//
// PRIVACY. Values are never reported -- only counts, and bounds for numeric kinds. An
// encrypted value classifies as wire.ClassEncrypted and is never looked through, so its
// contents cannot reach a profile. Which PROFILES are reported is not decided here: private
// attributes keep their own, because this is an in-process measurement and the READ-level
// stripping that hides them lives at the serving boundary. A caller exposing a profile to a
// client must apply that filter itself -- bounds on a private numeric attribute are a
// disclosure even though the value is not.
//
// Evaluating changes that division, and the part that cannot wait for the serving boundary is
// handled here. An expression reads other attributes, so a cell named for nothing private can
// carry a private attribute's bounds into a profile the name filter has no reason to drop. A
// reference to a private attribute therefore resolves to undefined while this measurement
// evaluates -- withheld from the input, which is exact, rather than filtered from the output,
// which a name cannot be. Entitlement to SEALED values is the caller's to state (Redact),
// because unlike privateness it is a property of the reader rather than of the attribute.

// AttrValueCounts is the measured shape of a set of values: which classes they fell into, and
// the exact bounds of the numeric ones.
//
// It is a separate type because the same measurement is taken twice over the same cells -- once
// on what they STORE and once on what they EVALUATE to -- and a consumer comparing the two
// wants the same fields on both, not a parallel set of names it has to keep aligned by hand.
type AttrValueCounts struct {
	// Classes counts the values by wire.NodeClass name ("int", "string", "expr", ...).
	// A single entry means the attribute is uniformly one kind in this sample.
	Classes map[string]int `json:"classes"`
	// Dominant is the most frequent class, and ties break toward the lexically smaller
	// name so the field is deterministic.
	Dominant string `json:"dominant"`

	// Ints counts integer values; IntMin/IntMax are their exact bounds.
	Ints   int   `json:"ints,omitempty"`
	IntMin int64 `json:"intMin,omitempty"`
	IntMax int64 `json:"intMax,omitempty"`
	// IntsLossyAsFloat counts integers whose magnitude exceeds 2^53, so they do not
	// round-trip through a float64. It is what decides whether an integer attribute may be
	// declared as a float type: zero means every value survives, non-zero means the
	// conversion would silently alter values.
	IntsLossyAsFloat int `json:"intsLossyAsFloat,omitempty"`

	// Reals counts real values. RealMin/RealMax exclude NaNs, which are a member of no
	// order and would otherwise make the bounds meaningless; RealNaNs counts them
	// separately. +/-Inf is ordered and so is included in the bounds.
	Reals    int     `json:"reals,omitempty"`
	RealNaNs int     `json:"realNaNs,omitempty"`
	RealMin  float64 `json:"realMin,omitempty"`
	RealMax  float64 `json:"realMax,omitempty"`
}

// AttrTypeProfile is what one attribute held across a sample of records.
//
// Counts are records, out of the sampled total that AttrTypeProfiles returns alongside. An
// attribute absent from a record is counted nowhere: Present is how many records had it at
// all, so Present < sampled is an absence rate, which is what decides whether a declared type
// has to admit nulls.
//
// The embedded counts are the STORED forms. Evaluated, when it is there, is the same
// measurement over what those cells evaluate to; see AttrTypeProfilesWith.
type AttrTypeProfile struct {
	Name string `json:"name"`
	// Present is the records in which the attribute appeared.
	Present int `json:"present"`

	AttrValueCounts

	// Evaluated is what the same cells EVALUATED to, or nil when evaluation was not
	// asked for. Its class counts sum to Present exactly as the stored ones do: a cell
	// that could not be evaluated is counted under its stored class, so a residual
	// "expr" entry is the share this measurement could not resolve rather than a gap.
	Evaluated *AttrValueCounts `json:"evaluated,omitempty"`
}

// UNDEFINED AND ERROR ARE NOT TYPES. A stored `undefined` says the attribute has no value
// here, which is the same thing an absent attribute says and the same thing a consumer
// represents as a null -- it does not compete with int or string for what the attribute's TYPE
// is. Measured on a real pool, conflating them badly overstates how mixed the data is: the two
// most heterogeneous-looking attributes in a 2000-ad history sample were 25 strings against
// 1975 `undefined`, which is a uniformly-string attribute that is usually absent, not a mixed
// one. So the type questions are asked over ValueClasses, and undefined/error are counted as
// nulls alongside absence.

// ValueClasses is Classes without the classes that carry no type -- undefined and error. It is
// the set a declared type has to cover.
func (p AttrValueCounts) ValueClasses() map[string]int {
	out := make(map[string]int, len(p.Classes))
	for name, n := range p.Classes {
		if name == wire.ClassUndefined.String() || name == wire.ClassError.String() {
			continue
		}
		out[name] = n
	}
	return out
}

// Nulls counts the sampled values that carry no type: stored `undefined` and `error`. Absence
// is NOT included -- that is Present against the sampled total, and a consumer that nulls both
// has to add them.
func (p AttrValueCounts) Nulls() int {
	return p.Classes[wire.ClassUndefined.String()] + p.Classes[wire.ClassError.String()]
}

// Uniform reports whether every sampled value that carries a type was the same class -- the
// case where a declared type needs no coercion. Nulls and absence do not break uniformity;
// they decide whether the declared type must admit nulls, which is a separate question.
//
// False for an attribute that was never anything but undefined: there is no type to be uniform
// about, and reporting true would invite a caller to declare one. Distinguish that case with
// len(ValueClasses()) == 0.
func (p AttrValueCounts) Uniform() bool { return len(p.ValueClasses()) == 1 }

// evalNodeCacheMax bounds the decoded-expression cache the evaluated pass keeps. It is far
// above the number of DISTINCT expressions a table holds when the recurring case is in play,
// and it is what keeps the other case from costing an AST per sampled cell.
const evalNodeCacheMax = 4096

// maxExactFloat64Int is the largest integer magnitude a float64 represents exactly. Above it,
// consecutive integers share a float64, so a conversion loses values rather than precision in
// the usual rounding sense.
const maxExactFloat64Int = int64(1) << 53

// AttrTypeOptions selects what AttrTypeProfilesWith measures.
type AttrTypeOptions struct {
	// SampleMax bounds the records drawn; zero or negative leaves the sampler's default.
	SampleMax int
	// Evaluate fills each profile's Evaluated counts as well: the same cells measured by
	// what they EVALUATE to rather than by what they store. It is the only way to type an
	// attribute a pool stores as an expression (see the header), and it is the expensive
	// option -- roughly twice the stored pass on a 45-attribute fixture.
	//
	// Each cell is evaluated against its own record and nothing else. On a CHAINED
	// collection that means a PARENT reference resolves to undefined, because a sample is
	// one record rather than a chain, so an attribute written over its parent's is counted
	// as the undefined it evaluates to here. The stored counts have the same blindness and
	// report it as "expr"; neither is a reason to declare a type from.
	Evaluate bool
	// Redact evaluates as a reader NOT entitled to sealed values does: with no key, so a
	// sealed value is undefined in the scope an expression is evaluated against. It leaves
	// the stored counts alone, which never look through a sealed node in any case.
	Redact bool
}

// AttrTypeProfiles samples up to sampleMax records and reports, per attribute, the classes of
// value it held and the bounds of its numeric ones. It returns the profiles and the number of
// records actually sampled.
//
// Profiles are ordered by Present descending, then by name, so the attributes a consumer is
// most likely to declare come first and the order is stable across calls on the same data.
//
// It reads samples and classifies them; it decodes no expressions, evaluates nothing, and
// writes nothing. Returns (nil, 0) when there is nothing to sample. AttrTypeProfilesWith is
// the same measurement with the evaluated one alongside it.
func (c *Collection) AttrTypeProfiles(sampleMax int) ([]AttrTypeProfile, int) {
	return c.AttrTypeProfilesWith(AttrTypeOptions{SampleMax: sampleMax})
}

// AttrTypeProfilesWith is AttrTypeProfiles with the measurement selected by o.
func (c *Collection) AttrTypeProfilesWith(o AttrTypeOptions) ([]AttrTypeProfile, int) {
	raw := c.CollectSamplesRecentN(o.SampleMax)
	samples := raw
	if c.inline {
		// Classification is keyed by interned attribute id, so an inline-names record has
		// to be rewritten to the global intern table first -- the same conversion
		// SchemaFit does, and for the same reason. A record that will not convert is
		// dropped rather than counted against a wrong id.
		//
		// The ORIGINAL of each kept record is kept beside it, because the evaluated pass
		// reads through this collection's own context (c.evalCtx), which names attributes
		// the way the collection stores them -- inline here -- and not the way the
		// converted copy does. Dropping a record from one list and not the other would
		// make the two measurements describe different samples.
		interned := make([][]byte, 0, len(samples))
		kept := make([][]byte, 0, len(samples))
		for _, w := range samples {
			if iw, ok := c.recordToInterned(nil, w); ok {
				interned = append(interned, iw)
				kept = append(kept, w)
			}
		}
		samples, raw = interned, kept
	}
	if len(samples) == 0 {
		return nil, 0
	}

	acc := map[uint32]*AttrTypeProfile{}
	for _, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			p := acc[id]
			if p == nil {
				p = &AttrTypeProfile{AttrValueCounts: AttrValueCounts{Classes: map[string]int{}}}
				acc[id] = p
			}
			p.Present++
			class := wire.ClassifyNode(node)
			p.Classes[class.String()]++
			p.observe(class, node)
			return true
		})
	}

	var evaluated map[string]*AttrValueCounts
	if o.Evaluate {
		evaluated = c.evaluatedCounts(raw, o.Redact)
	}

	out := make([]AttrTypeProfile, 0, len(acc))
	for id, p := range acc {
		name, ok := c.schemaFieldName(id)
		if !ok {
			// An id with no name in the intern table cannot be attributed to an
			// attribute, so reporting it would invent one. Counting how many such ids
			// appeared is a separate concern (opendiag's territory); here they are
			// dropped.
			continue
		}
		p.Name = name
		p.Dominant = dominantClass(p.Classes)
		// Keyed by folded name, because the two passes read the record in two different
		// forms -- one interned, one as the collection stores it -- and a name is the only
		// key both have. Attribute names are case-insensitive, so the fold is the join.
		if ev := evaluated[strings.ToLower(name)]; ev != nil {
			ev.Dominant = dominantClass(ev.Classes)
			p.Evaluated = ev
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Present != out[j].Present {
			return out[i].Present > out[j].Present
		}
		return out[i].Name < out[j].Name
	})
	return out, len(samples)
}

// observe accumulates one value's numeric contribution. Only int and real carry bounds; every
// other class is accounted for by its Classes entry alone.
func (p *AttrValueCounts) observe(class wire.NodeClass, node []byte) {
	if class != wire.ClassInt && class != wire.ClassReal {
		return
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return // a tag this classifier calls numeric but the decoder rejects: malformed
	}
	switch class {
	case wire.ClassInt:
		p.addInt(lit.Int)
	case wire.ClassReal:
		p.addReal(lit.Real)
	}
}

// addInt and addReal accumulate one numeric value's contribution to the bounds. They are
// separate from observe because the evaluated pass has a classad.Value rather than a node and
// must land in exactly the same counters -- a second copy of this arithmetic is how the two
// measurements would drift apart.
func (p *AttrValueCounts) addInt(n int64) {
	p.Ints++
	// Explicit comparisons: this package defines its own int-typed min/max, which
	// shadow the builtins and will not take an int64.
	switch {
	case p.Ints == 1:
		p.IntMin, p.IntMax = n, n
	case n < p.IntMin:
		p.IntMin = n
	case n > p.IntMax:
		p.IntMax = n
	}
	// -maxExactFloat64Int is itself exact, so the test is on magnitude strictly
	// ABOVE the bound. Written as two comparisons because negating math.MinInt64
	// overflows.
	if n > maxExactFloat64Int || n < -maxExactFloat64Int {
		p.IntsLossyAsFloat++
	}
}

func (p *AttrValueCounts) addReal(f float64) {
	// Reals counts every real, NaNs included, so the class counts and this one
	// agree. The bounds track only the ordered ones, and after the increment
	// Reals-RealNaNs is how many of those there are -- 1 on the first.
	p.Reals++
	if math.IsNaN(f) {
		p.RealNaNs++
		return // a member of no order: contributes to neither bound
	}
	if p.Reals-p.RealNaNs == 1 {
		p.RealMin, p.RealMax = f, f
	} else {
		p.RealMin = math.Min(p.RealMin, f)
		p.RealMax = math.Max(p.RealMax, f)
	}
}

// observeValue accumulates one EVALUATED value, under the class name its stored equivalent
// would carry, so the two measurements are comparable entry by entry rather than only in
// aggregate.
func (p *AttrValueCounts) observeValue(v classad.Value) {
	switch {
	case v.IsError():
		p.Classes[wire.ClassError.String()]++
	case v.IsUndefined():
		p.Classes[wire.ClassUndefined.String()]++
	case v.IsBool():
		p.Classes[wire.ClassBool.String()]++
	case v.IsInteger():
		p.Classes[wire.ClassInt.String()]++
		if n, err := v.IntValue(); err == nil {
			p.addInt(n)
		}
	case v.IsReal():
		p.Classes[wire.ClassReal.String()]++
		if f, err := v.RealValue(); err == nil {
			p.addReal(f)
		}
	case v.IsString():
		p.Classes[wire.ClassString.String()]++
	case v.IsList():
		p.Classes[wire.ClassList.String()]++
	case v.IsClassAd():
		p.Classes[wire.ClassRecord.String()]++
	default:
		p.Classes[wire.ClassInvalid.String()]++
	}
}

// evaluatedCounts measures what the sampled cells EVALUATE to, keyed by folded attribute name.
//
// Only an expression cell is evaluated. Evaluating a literal is the identity, so those carry
// across under their stored class at no cost, and a SEALED cell is deliberately never
// evaluated: reaching through to the plaintext's class is exactly the leak wire.ClassEncrypted
// exists to prevent, and it would be a leak this measurement performed on its own initiative
// rather than one a reader asked for.
//
// THE CHEAP PATH IS THE SAME ONE THE READERS USE. An expression cell is decoded on its own and
// evaluated against a wireScope over the record's bytes, with no ClassAd built -- which is what
// QueryProject does to serve the same attribute, so the report describes the values that read
// will actually produce. The whole-record decode is the fallback, taken for a cell whose
// references the scope cannot resolve (a reference that is itself an expression) or that
// evaluates to an error (a function that needs a real scope, eval() among them).
//
// Two caches make the difference, and both exploit the same property of real data: an
// expression attribute holds the SAME expression in every ad. Decoded nodes are memoized by
// their bytes, so a pool's MemoryUsage is parsed once per sample rather than once per record,
// and the folded names are memoized. Measured on a 45-attribute fixture against the stored pass
// alone (BenchmarkAttrTypeProfiles): taking the whole-record decode for every cell costs 3.8x
// and 626k allocations per 2000 records; through the scope it is 2.2x and 106k.
//
// The invariant a consumer relies on is that these counts sum to the same Present the stored
// ones do. A cell that cannot be evaluated at all -- an undecodable node or record -- is
// therefore counted under its stored class, so a residual "expr" entry means "this share could
// not be resolved", never "this share is missing".
func (c *Collection) evaluatedCounts(samples [][]byte, redact bool) map[string]*AttrValueCounts {
	acc := make(map[string]*AttrValueCounts)
	// Both memoize the fold, which is otherwise paid per attribute per record: by interned
	// id where the record carries one, by the name itself where it does not. A persistent
	// collection stores inline names, so the second map is the one that runs on an archive.
	keys := make(map[uint32]string)
	folded := make(map[string]string)
	nodes := make(map[string]ast.Expr)
	ctx := c.evalCtx(redact)
	rs := &wireScope{ctx: ctx}
	ev := classad.NewEvaluator(nil)
	// The private-attribute rule on the cheap path, and it is exact where a walk of the
	// expression would be a guess: every reference the evaluation actually makes comes
	// through here, including the ones a static walk cannot see because the expression
	// selects or subscripts its way to them. See evalScopeOf for why the rule is to withhold
	// the input rather than to filter the output.
	ev.SetResolver(func(name string, scope ast.AttributeScope) classad.Value {
		if classad.IsPrivateAttribute(name) {
			return classad.NewUndefinedValue()
		}
		return rs.resolve(name, scope)
	})
	for _, w := range samples {
		// The scope resolves references the way this collection stores attributes, so a
		// record in the OTHER form would resolve every one of them to nothing and count a
		// value that is undefined for no reason the data gives. Dropped rather than
		// half-evaluated, which also keeps this pass counting the same records the stored
		// one does -- that pass drops such a record too, by reading its keys as ids it
		// cannot name.
		if _, _, _, inlineRec, ok := wire.Ad(w).SplitBody(); !ok || inlineRec != c.inline {
			continue
		}
		rs.ad, rs.dict, rs.parent = wire.Ad(w), nil, nil
		var scope *classad.ClassAd
		decoded := false
		wire.Ad(w).ForEachRaw(func(id uint32, name string, node []byte) bool {
			key, known := keys[id]
			if name != "" {
				// An inline record carries the name and no id, so the fold is
				// memoized under the name instead.
				if key, known = folded[name]; !known {
					key = strings.ToLower(name)
					folded[name] = key
					known = true
				}
			} else if !known {
				var ok bool
				if name, ok = c.schemaFieldName(id); !ok {
					keys[id] = "" // unnameable: dropped, as in the stored pass
					return true
				}
				key = strings.ToLower(name)
				keys[id] = key
			}
			if key == "" {
				return true
			}
			if name == "" {
				name = key // only EvaluateAttr needs it, and it folds anyway
			}
			p := acc[key]
			if p == nil {
				p = &AttrValueCounts{Classes: map[string]int{}}
				acc[key] = p
			}
			class := wire.ClassifyNode(node)
			if class != wire.ClassExpr {
				p.Classes[class.String()]++
				p.observe(class, node)
				return true
			}
			// Memoized by the node's bytes: the map lookup does not allocate for a
			// []byte key, and on a real pool one expression recurs in every record.
			// Capped because the opposite data -- an expression that differs in every
			// record -- would make the cache pure overhead and hold an AST per sampled
			// cell; past the cap it simply stops growing and the decode is repaid.
			expr, seen := nodes[string(node)]
			if !seen {
				var err error
				if expr, err = ctx.decodeNode(node); err != nil {
					expr = nil
				}
				if len(nodes) < evalNodeCacheMax {
					nodes[string(node)] = expr
				}
			}
			if expr != nil {
				rs.fellBack = false
				v := evalThroughScope(ev, expr)
				if !rs.fellBack && !v.IsError() {
					p.observeValue(v)
					return true
				}
			}
			if !decoded {
				decoded = true
				scope = evalScopeOf(ctx, w)
			}
			if scope == nil {
				p.Classes[class.String()]++ // unevaluatable: counted as stored
				return true
			}
			p.observeValue(scope.EvaluateAttr(name))
			return true
		})
	}
	return acc
}

// evalThroughScope evaluates one expression with the evaluator's installed resolver. SetScope
// resets the recursion depth and in-progress set between cells without disturbing the resolver;
// RecoverCyclic turns a cyclic reference into an error value rather than a panic, which is what
// makes that cell fall to the whole-record decode instead of taking the profiler down.
func evalThroughScope(ev *classad.Evaluator, e ast.Expr) (v classad.Value) {
	defer classad.RecoverCyclic(&v)
	ev.SetScope(nil)
	return ev.Evaluate(e)
}

// evalScopeOf decodes one sampled record into the scope its expression cells are evaluated
// against, with private attributes removed. Returns nil when the record will not decode, which
// leaves its expression cells counted as stored.
//
// Removing the private attributes is this measurement's privacy rule, and it is applied to the
// INPUT rather than to the output. A profile carries no values, which is what makes it safe to
// serve; it does carry numeric BOUNDS, and an expression over a private attribute would carry
// those bounds into a profile named for an attribute that is not private -- past the filter
// that drops private profiles by name, which cannot see through a name it has no reason to
// suspect. With the attribute out of the scope the expression evaluates to undefined or error,
// which is counted honestly and discloses nothing.
//
// It is the input rather than the output for the same reason the redacted read paths hold no
// key: filtering what came back is a deny-list, and not having the value is not.
func evalScopeOf(ctx wireCtx, w []byte) *classad.ClassAd {
	a, err := ctx.decodeWire(w)
	if err != nil || a == nil {
		return nil
	}
	kept := a.Attributes[:0]
	for _, at := range a.Attributes {
		if classad.IsPrivateAttribute(at.Name) {
			continue
		}
		kept = append(kept, at)
	}
	a.Attributes = kept
	return classad.FromAST(a)
}

// dominantClass returns the most frequent class name, breaking ties toward the lexically
// smaller one so the result does not depend on map iteration order.
func dominantClass(classes map[string]int) string {
	best, bestN := "", -1
	for name, n := range classes {
		if n > bestN || (n == bestN && name < best) {
			best, bestN = name, n
		}
	}
	return best
}
