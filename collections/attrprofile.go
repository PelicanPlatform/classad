package collections

import (
	"math"
	"sort"

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
// PRIVACY. Values are never reported -- only counts, and bounds for numeric kinds. An
// encrypted value classifies as wire.ClassEncrypted and is never looked through, so its
// contents cannot reach a profile. Private (secret) attributes are NOT filtered here: this is
// an in-process measurement, and the READ-level stripping that hides them lives at the serving
// boundary. A caller that exposes a profile to a client must apply that filter itself --
// bounds on a private numeric attribute are a disclosure even though the value is not.

// AttrTypeProfile is what one attribute held across a sample of records.
//
// Counts are records, out of the sampled total that AttrTypeProfiles returns alongside. An
// attribute absent from a record is counted nowhere: Present is how many records had it at
// all, so Present < sampled is an absence rate, which is what decides whether a declared type
// has to admit nulls.
type AttrTypeProfile struct {
	Name string `json:"name"`
	// Present is the records in which the attribute appeared.
	Present int `json:"present"`
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

// Uniform reports whether every sampled value was the same class -- the case where a declared
// type needs no coercion at all. It says nothing about absence: an attribute present in half
// the sample and an integer every time it is present is uniform.
func (p AttrTypeProfile) Uniform() bool { return len(p.Classes) == 1 }

// maxExactFloat64Int is the largest integer magnitude a float64 represents exactly. Above it,
// consecutive integers share a float64, so a conversion loses values rather than precision in
// the usual rounding sense.
const maxExactFloat64Int = int64(1) << 53

// AttrTypeProfiles samples up to sampleMax records and reports, per attribute, the classes of
// value it held and the bounds of its numeric ones. It returns the profiles and the number of
// records actually sampled.
//
// Profiles are ordered by Present descending, then by name, so the attributes a consumer is
// most likely to declare come first and the order is stable across calls on the same data.
//
// It reads samples and classifies them; it decodes no expressions, evaluates nothing, and
// writes nothing. Returns (nil, 0) when there is nothing to sample.
func (c *Collection) AttrTypeProfiles(sampleMax int) ([]AttrTypeProfile, int) {
	samples := c.CollectSamplesRecentN(sampleMax)
	if c.inline {
		// Classification is keyed by interned attribute id, so an inline-names record has
		// to be rewritten to the global intern table first -- the same conversion
		// SchemaFit does, and for the same reason. A record that will not convert is
		// dropped rather than counted against a wrong id.
		interned := make([][]byte, 0, len(samples))
		for _, w := range samples {
			if iw, ok := c.recordToInterned(nil, w); ok {
				interned = append(interned, iw)
			}
		}
		samples = interned
	}
	if len(samples) == 0 {
		return nil, 0
	}

	acc := map[uint32]*AttrTypeProfile{}
	for _, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			p := acc[id]
			if p == nil {
				p = &AttrTypeProfile{Classes: map[string]int{}}
				acc[id] = p
			}
			p.Present++
			class := wire.ClassifyNode(node)
			p.Classes[class.String()]++
			p.observe(class, node)
			return true
		})
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
func (p *AttrTypeProfile) observe(class wire.NodeClass, node []byte) {
	if class != wire.ClassInt && class != wire.ClassReal {
		return
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return // a tag this classifier calls numeric but the decoder rejects: malformed
	}
	switch class {
	case wire.ClassInt:
		p.Ints++
		// Explicit comparisons: this package defines its own int-typed min/max, which
		// shadow the builtins and will not take an int64.
		switch {
		case p.Ints == 1:
			p.IntMin, p.IntMax = lit.Int, lit.Int
		case lit.Int < p.IntMin:
			p.IntMin = lit.Int
		case lit.Int > p.IntMax:
			p.IntMax = lit.Int
		}
		// -maxExactFloat64Int is itself exact, so the test is on magnitude strictly
		// ABOVE the bound. Written as two comparisons because negating math.MinInt64
		// overflows.
		if lit.Int > maxExactFloat64Int || lit.Int < -maxExactFloat64Int {
			p.IntsLossyAsFloat++
		}
	case wire.ClassReal:
		// Reals counts every real, NaNs included, so the class counts and this one
		// agree. The bounds track only the ordered ones, and after the increment
		// Reals-RealNaNs is how many of those there are -- 1 on the first.
		p.Reals++
		if math.IsNaN(lit.Real) {
			p.RealNaNs++
			return // a member of no order: contributes to neither bound
		}
		if p.Reals-p.RealNaNs == 1 {
			p.RealMin, p.RealMax = lit.Real, lit.Real
		} else {
			p.RealMin = math.Min(p.RealMin, lit.Real)
			p.RealMax = math.Max(p.RealMax, lit.Real)
		}
	}
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
