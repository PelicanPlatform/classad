package canon

import "math"

// FloatTolerance controls how reals (and the seconds component of times) are
// compared. Two engines performing the same IEEE-754 operations usually agree
// bit-for-bit, but transcendental functions (sqrt, pow, ...) may differ in the
// last ULP across libm versions. A small relative tolerance suppresses that
// noise without masking genuine semantic differences.
type FloatTolerance struct {
	Rel float64
	Abs float64
}

// DefaultTolerance is intentionally tight: we want to catch real arithmetic
// divergences, only forgiving sub-ULP rounding.
var DefaultTolerance = FloatTolerance{Rel: 1e-12, Abs: 1e-12}

func (t FloatTolerance) equal(a, b float64) bool {
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	diff := math.Abs(a - b)
	if diff <= t.Abs {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= t.Rel*scale
}

// Equal reports whether two canonical values are semantically equal under the
// given float tolerance. Type (Kind) must match exactly: an integer and a real
// of equal magnitude are NOT equal, because the int-vs-real distinction is a
// first-class part of ClassAd semantics and a frequent source of divergence.
func Equal(a, b Value, tol FloatTolerance) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KUndef, KError:
		return true
	case KBool:
		return a.B == b.B
	case KInt:
		return a.I == b.I
	case KReal, KReltime:
		return tol.equal(a.R, b.R)
	case KAbstime:
		return tol.equal(a.R, b.R) && a.Off == b.Off
	case KString:
		return a.S == b.S
	case KList:
		if len(a.List) != len(b.List) {
			return false
		}
		for i := range a.List {
			if !Equal(a.List[i], b.List[i], tol) {
				return false
			}
		}
		return true
	case KClassad:
		if len(a.Map) != len(b.Map) {
			return false
		}
		for i := range a.Map {
			if a.Map[i].Key != b.Map[i].Key {
				return false
			}
			if !Equal(a.Map[i].Val, b.Map[i].Val, tol) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Describe renders a value as a short human-readable string for divergence
// reports (distinct from the wire Encode format, which is for machines).
func Describe(v Value) string {
	switch v.Kind {
	case KUndef:
		return "undefined"
	case KError:
		return "error"
	case KBool:
		if v.B {
			return "true"
		}
		return "false"
	case KInt:
		return "int(" + itoa(v.I) + ")"
	case KReal:
		return "real(" + formatReal(v.R) + ")"
	case KReltime:
		return "reltime(" + formatReal(v.R) + "s)"
	case KAbstime:
		return "abstime(" + formatReal(v.R) + "s," + itoa(v.Off) + ")"
	case KString:
		return "string(" + quote(v.S) + ")"
	case KList:
		out := "list["
		for i, e := range v.List {
			if i > 0 {
				out += ","
			}
			out += Describe(e)
		}
		return out + "]"
	case KClassad:
		out := "classad{"
		for i, kv := range v.Map {
			if i > 0 {
				out += ";"
			}
			out += kv.Key + "=" + Describe(kv.Val)
		}
		return out + "}"
	default:
		return "?"
	}
}

func itoa(i int64) string {
	return formatInt(i)
}
