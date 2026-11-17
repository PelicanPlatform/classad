package collections

import (
	"math"

	"github.com/PelicanPlatform/classad/classad"
)

// ONE TOTAL ORDER OVER CLASSAD VALUES.
//
// Several places here have to put values in a definite order: a GROUP BY result, the k best rows
// of a group, a sorted group key. ClassAd's own `<` cannot serve on its own, because it is not a
// total order -- it is undefined across types, so a set holding a number and a string has no
// ClassAd answer at all, and a comparator that reports "neither is less" for such a pair leaves
// a sort free to return a different answer each run.
//
// So this agrees with ClassAd `<` wherever ClassAd defines it, and settles the rest by a fixed
// class precedence: numbers, then strings, then booleans, then undefined, then error, then
// everything else. The precedence is arbitrary; being FIXED is the point.
//
// TWO INTEGERS ARE COMPARED AS INTEGERS. Past 2^53 a float64 cannot hold consecutive integers
// apart: 9007199254740992 and 9007199254740993 convert to the same float, so comparing as floats
// reports two distinct job ids equal, and whichever sorted first stays first. Job ids and record
// sequences reach that range, so the int64s are compared directly when both values hold one. A
// mixed integer/real pair still compares as floats, which is what ClassAd means by ordering 2
// against 2.0 -- they are equal, and the aggregate engine groups them together for the same
// reason.

// value class precedence. Fixed, not meaningful: see the note above.
const (
	orderClassNumber = iota
	orderClassString
	orderClassBool
	orderClassUndefined
	orderClassError
	orderClassOther
)

// valueOrderClass is the precedence class v sorts in.
func valueOrderClass(v classad.Value) int {
	switch {
	case v.IsUndefined():
		return orderClassUndefined
	case v.IsError():
		return orderClassError
	case v.IsBool():
		return orderClassBool
	case v.IsString():
		return orderClassString
	case v.IsInteger(), v.IsReal():
		return orderClassNumber
	}
	return orderClassOther
}

// CompareValues orders two ClassAd values totally: negative when a sorts before b, positive when
// it sorts after, zero when neither sorts before the other.
//
// Equal-comparing values are not necessarily the same value. Two strings that differ only by case
// fold equal under ClassAd's own comparison and are then separated by their bytes, so distinct
// strings never report equal; but 2 and 2.0 do, as ClassAd says they should.
//
// NaN sorts after every other number rather than comparing false in both directions. A comparator
// that reports a NaN neither less nor greater is not a total order, and the sorts and the top-k
// keep that use this one both depend on being able to name a worst element.
func CompareValues(a, b classad.Value) int {
	// Both numeric is the overwhelming case -- a date, an id, a size -- and it is the one the
	// class walk below is slowest to reach, since number is tested last of the five. This is
	// not a second opinion about the order: two numbers are one class, so the walk would call
	// compareNumbers too. It just gets there in two comparisons instead of twelve.
	if a.IsNumber() && b.IsNumber() {
		return compareNumbers(a, b)
	}
	ac, bc := valueOrderClass(a), valueOrderClass(b)
	if ac != bc {
		return cmpInt(ac, bc)
	}
	switch ac {
	case orderClassNumber:
		return compareNumbers(a, b)
	case orderClassString:
		as, _ := a.StringValue() //nolint:errcheck // guarded by the class check above
		bs, _ := b.StringValue() //nolint:errcheck // guarded by the class check above
		// The evaluator's own case-insensitive comparison, which is also the order the block
		// dictionaries are sorted in, with a byte tiebreak so values that fold equal -- distinct
		// values, since a scan groups by exact text -- still have a stable place.
		if c := classad.CompareStringsFold(as, bs); c != 0 {
			return c
		}
		switch {
		case as < bs:
			return -1
		case as > bs:
			return 1
		}
		return 0
	case orderClassBool:
		ab, _ := a.BoolValue() //nolint:errcheck // guarded by the class check above
		bb, _ := b.BoolValue() //nolint:errcheck // guarded by the class check above
		switch {
		case !ab && bb:
			return -1
		case ab && !bb:
			return 1
		}
		return 0
	case orderClassOther:
		as, bs := a.String(), b.String()
		switch {
		case as < bs:
			return -1
		case as > bs:
			return 1
		}
		return 0
	}
	return 0 // undefined and error are one value each: nothing to order within
}

// compareNumbers orders two numeric values, exactly when both hold an integer.
func compareNumbers(a, b classad.Value) int {
	if a.IsInteger() && b.IsInteger() {
		ai, aerr := a.IntValue()
		bi, berr := b.IntValue()
		if aerr == nil && berr == nil {
			return cmpInt64(ai, bi)
		}
	}
	af, aerr := a.NumberValue()
	bf, berr := b.NumberValue()
	if aerr != nil || berr != nil {
		return 0
	}
	an, bn := math.IsNaN(af), math.IsNaN(bf)
	if an || bn {
		switch {
		case an && bn:
			return 0
		case an:
			return 1
		default:
			return -1
		}
	}
	switch {
	case af < bf:
		return -1
	case af > bf:
		return 1
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
