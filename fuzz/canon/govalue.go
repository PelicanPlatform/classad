package canon

import (
	"sort"

	classad "github.com/PelicanPlatform/classad/classad"
)

// FromGoValue converts a value produced by the native Go evaluator into the
// canonical representation.
//
// Composite values follow a rule applied identically to the C++ side so that
// any difference is genuine:
//   - List elements are encoded in order (the Go evaluator returns them already
//     evaluated).
//   - A nested ClassAd value is encoded as its attributes sorted by name, each
//     evaluated within the nested ad's own scope. This is a well-defined,
//     symmetric rule; it does not attempt to resolve references into the parent
//     scope (neither engine does so for a detached sub-ad).
//
// maxDepth guards against pathological self-referential structures.
func FromGoValue(v classad.Value) Value {
	return fromGoValue(v, 0)
}

const maxDepth = 64

// FromGoClassAd evaluates every top-level attribute of ad (in the ad's own
// scope) and returns the canonical classad value. This is the entry point used
// by the differential fuzzer for the Go engine; it mirrors what the C++ shim
// does in encodeClassAd.
func FromGoClassAd(ad *classad.ClassAd) Value {
	return fromGoClassAd(ad, 0)
}

func fromGoClassAd(ad *classad.ClassAd, depth int) Value {
	if ad == nil {
		return Value{Kind: KClassad}
	}
	attrs := ad.GetAttributes()
	sort.Strings(attrs)
	m := make([]Attr, 0, len(attrs))
	for _, name := range attrs {
		m = append(m, Attr{Key: name, Val: fromGoValue(ad.EvaluateAttr(name), depth+1)})
	}
	return Value{Kind: KClassad, Map: m}
}

func fromGoValue(v classad.Value, depth int) Value {
	if depth > maxDepth {
		return Value{Kind: KError}
	}
	switch v.Type() {
	case classad.UndefinedValue:
		return Value{Kind: KUndef}
	case classad.ErrorValue:
		return Value{Kind: KError}
	case classad.BooleanValue:
		b, err := v.BoolValue()
		if err != nil {
			return Value{Kind: KError}
		}
		return Value{Kind: KBool, B: b}
	case classad.IntegerValue:
		i, err := v.IntValue()
		if err != nil {
			return Value{Kind: KError}
		}
		return Value{Kind: KInt, I: i}
	case classad.RealValue:
		r, err := v.RealValue()
		if err != nil {
			return Value{Kind: KError}
		}
		return Value{Kind: KReal, R: r}
	case classad.StringValue:
		s, err := v.StringValue()
		if err != nil {
			return Value{Kind: KError}
		}
		return Value{Kind: KString, S: s}
	case classad.ListValue:
		elems, err := v.ListValue()
		if err != nil {
			return Value{Kind: KError}
		}
		out := make([]Value, 0, len(elems))
		for _, e := range elems {
			out = append(out, fromGoValue(e, depth+1))
		}
		return Value{Kind: KList, List: out}
	case classad.ClassAdValue:
		ad, err := v.ClassAdValue()
		if err != nil || ad == nil {
			return Value{Kind: KClassad}
		}
		return fromGoClassAd(ad, depth)
	default:
		// The Go engine has no dedicated absolute/relative-time value kind;
		// any unmapped type is reported as error so a divergence is visible.
		return Value{Kind: KError}
	}
}
