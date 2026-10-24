package gen

import (
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The values the aggregate fuzzer exists to exercise have to be in the data BY CONSTRUCTION. Every
// one of them is a case where a fast path and the record scan have disagreed before, and a
// generator that merely makes them likely produces runs that silently skip them -- which is
// indistinguishable, from the outside, from runs where they passed.
func TestGeneratedAdsCarryTheSharpValues(t *testing.T) {
	var (
		siteAbsent, siteUndefKeyword, siteUndefText, siteNumber, siteNumText int
		numLikeString, numLikeInt                                            int
		rateNaN, ratePosInf, rateNegInf, rateIntegralReal, rateInt           int
		memOverflowsSlot, flagBool, exprExpression, errError, rarePresent    int
		calcArithNaN, calcLiteral                                            int
	)
	s := ShapeByID(0)
	ads := s.Ads()
	for _, ad := range ads {
		site, hasSite := ad.Lookup("Site")
		switch v := ad.EvaluateAttr("Site"); {
		case !hasSite:
			siteAbsent++
		case v.IsUndefined():
			siteUndefKeyword++
		case v.IsInteger():
			siteNumber++
		case v.IsString():
			if str, _ := v.StringValue(); str == "undefined" {
				siteUndefText++
			} else if str == "42" {
				siteNumText++
			}
		default:
			_ = site
		}
		switch v := ad.EvaluateAttr("NumLike"); {
		case v.IsString():
			numLikeString++
		case v.IsInteger():
			numLikeInt++
		}
		switch v := ad.EvaluateAttr("Rate"); {
		case v.IsReal():
			r, _ := v.RealValue()
			switch {
			case math.IsNaN(r):
				rateNaN++
			case math.IsInf(r, 1):
				ratePosInf++
			case math.IsInf(r, -1):
				rateNegInf++
			case r == math.Trunc(r):
				rateIntegralReal++
			}
		case v.IsInteger():
			rateInt++
		}
		// Calc's NaN arrives by arithmetic, so what distinguishes it is that the stored node is
		// NOT a literal while the evaluated value IS NaN -- the combination that sends a record to
		// the cold tail and makes only the fallback reader see the NaN.
		if e, ok := ad.Lookup("Calc"); ok {
			v := ad.EvaluateExprWithTarget(e, nil)
			r, _ := v.RealValue()
			switch {
			case v.IsReal() && math.IsNaN(r):
				calcArithNaN++
			case e.String() == v.String():
				calcLiteral++
			}
		}
		if m, ok := ad.EvaluateAttrInt("Mem"); ok && m > 65535 {
			memOverflowsSlot++
		}
		if ad.EvaluateAttr("Flag").IsBool() {
			flagBool++
		}
		if e, ok := ad.Lookup("Expr"); ok {
			if _, isLit := literalInt(ad, e); !isLit {
				exprExpression++
			}
		}
		if ad.EvaluateAttr("Err").IsError() {
			errError++
		}
		if _, ok := ad.Lookup("Rare"); ok {
			rarePresent++
		}
	}

	for _, c := range []struct {
		name string
		n    int
		min  int
	}{
		{"Site absent (groups as undefined)", siteAbsent, 10},
		{"Site = the UNDEFINED keyword", siteUndefKeyword, 10},
		{`Site = the literal text "undefined"`, siteUndefText, 10},
		{"Site = a number in a string column", siteNumber, 10},
		{`Site = the string "42" beside that number`, siteNumText, 10},
		{"NumLike as a string", numLikeString, 500},
		{"NumLike as an integer (escapes the string column)", numLikeInt, 30},
		{"Rate = NaN (a stored literal, reaches the column)", rateNaN, 5},
		{"Calc = NaN by arithmetic (an expression, escapes to the cold tail)", calcArithNaN, 3},
		{"Calc = a stored real literal", calcLiteral, 1000},
		{"Rate = +Inf", ratePosInf, 5},
		{"Rate = -Inf", rateNegInf, 5},
		{"Rate = an INTEGRAL real (renders like an integer)", rateIntegralReal, 50},
		{"Rate = an integer in a real column", rateInt, 30},
		{"Mem past its 2-byte slot (escapes to the cold tail)", memOverflowsSlot, 10},
		{"Flag = a boolean", flagBool, 500},
		{"Expr = an expression where a literal usually is", exprExpression, 50},
		{"Err = an ERROR value", errError, 20},
		{"Rare present (below the schema presence floor)", rarePresent, 200},
	} {
		if c.n < c.min {
			t.Errorf("%s: %d of %d records, want at least %d", c.name, c.n, len(ads), c.min)
		}
	}

	// The presence rates the schema derivation acts on. A string column needs its dominant kind in
	// at least 90% of records to become a field at all, and both of these sit deliberately just
	// above that floor -- close enough that a careless change to the exception moduli would drop
	// them out of the columns and quietly send every query on them to the scan.
	t.Logf("Site string share %.3f, NumLike string share %.3f, Flag bool share %.3f",
		float64(len(ads)-siteAbsent-siteUndefKeyword-siteNumber)/float64(len(ads)),
		float64(numLikeString)/float64(len(ads)),
		float64(flagBool)/float64(len(ads)))
	if got := float64(len(ads)-siteAbsent-siteUndefKeyword-siteNumber) / float64(len(ads)); got < 0.905 {
		t.Errorf("Site's string share is %.3f, too close to the 0.90 presence floor to rely on", got)
	}
	if got := float64(numLikeString) / float64(len(ads)); got < 0.905 {
		t.Errorf("NumLike's string share is %.3f, too close to the 0.90 presence floor to rely on", got)
	}
}

// literalInt reports whether an attribute's expression is a plain integer literal.
func literalInt(ad *classad.ClassAd, e *classad.Expr) (int64, bool) {
	v := ad.EvaluateExprWithTarget(e, nil)
	if !v.IsInteger() {
		return 0, false
	}
	// An expression that happens to evaluate to an integer is not a literal; compare the
	// unparsed source against the rendered value, which only matches for a literal.
	i, _ := v.IntValue()
	return i, e.String() == v.String()
}

// TestGeneratedAdsAreDeterministic pins the property the shrinker and every reproducer rest on: a
// record is a pure function of its shape and index.
func TestGeneratedAdsAreDeterministic(t *testing.T) {
	for _, s := range Shapes() {
		for _, i := range []int{0, 7, 101, s.N - 1} {
			if a, b := s.Source(i), s.Source(i); a != b {
				t.Fatalf("shape %d record %d is not stable:\n%s\n---\n%s", s.ID, i, a, b)
			}
		}
	}
}

// TestGeneratedQueriesCoverTheShapes pins that the query generator still emits every shape the
// dispatch chain gates on. A generator that stopped emitting, say, the two-column "per owner per
// day" form would take the bucketed index path out of the run without any test noticing.
func TestGeneratedQueriesCoverTheShapes(t *testing.T) {
	var (
		ungrouped, oneRaw, oneBucketed, twoCols, strGroup, matchAll, matchNone int
		loneCountStar, loneNumAgg, filtered, distinct, zoneWide, zoneWideGroup int
	)
	const n = 3000
	for k := 0; k < n; k++ {
		q := QueryAt(k)
		switch len(q.Groups) {
		case 0:
			ungrouped++
		case 1:
			if q.Groups[0].BucketWidth > 0 {
				oneBucketed++
			} else {
				oneRaw++
			}
			if IsStringAttr(q.Groups[0].Attr) {
				strGroup++
			}
		default:
			twoCols++
		}
		switch q.Constraint {
		case "", "true", "1 == 1", "true && true":
			matchAll++
		case "false", "1 == 2", "undefined", "error", "!true":
			matchNone++
		}
		if len(q.Aggs) == 1 && q.Aggs[0].Func == FuncCount && q.Aggs[0].Arg == "*" && q.Aggs[0].Filter == "" {
			loneCountStar++
		}
		// The zone-verdict shape, counted both ways: the grouped form is the one that attributes
		// whole segments from their zone verdicts, and it is the narrow one.
		for _, c := range zoneWideConds {
			if q.Constraint == c {
				zoneWide++
				if len(q.Groups) == 1 && q.Groups[0].Attr == "Owner" && q.Groups[0].BucketWidth == 0 {
					zoneWideGroup++
				}
				break
			}
		}
		if len(q.Aggs) == 1 && q.Aggs[0].Arg != "*" {
			loneNumAgg++
		}
		for _, a := range q.Aggs {
			if a.Filter != "" {
				filtered++
			}
			if a.Func == FuncCountDistinct {
				distinct++
			}
		}
	}
	for _, c := range []struct {
		name string
		n    int
		min  int
	}{
		{"ungrouped", ungrouped, n / 20}, {"one raw group column", oneRaw, n / 20},
		{"one bucketed group column", oneBucketed, n / 20}, {"two group columns", twoCols, n / 50},
		{"a string group column", strGroup, n / 50}, {"match-all", matchAll, n / 20},
		{"match-none", matchNone, n / 100}, {"a lone unfiltered COUNT(*)", loneCountStar, n / 10},
		{"a lone aggregate over an attribute", loneNumAgg, n / 20},
		{"a per-aggregate FILTER", filtered, n / 200}, {"COUNT(DISTINCT)", distinct, n / 100},
		{"a range every zone-bounded value satisfies", zoneWide, n / 50},
		{"... the same, grouped by the categorical column", zoneWideGroup, n / 100},
	} {
		if c.n < c.min {
			t.Errorf("%s: %d of %d queries, want at least %d", c.name, c.n, n, c.min)
		}
	}
	t.Logf("ungrouped=%d oneRaw=%d oneBucketed=%d twoCols=%d strGroup=%d matchAll=%d matchNone=%d "+
		"loneCount*=%d loneAgg=%d filtered=%d distinct=%d zoneWide=%d (grouped %d)",
		ungrouped, oneRaw, oneBucketed, twoCols, strGroup, matchAll, matchNone,
		loneCountStar, loneNumAgg, filtered, distinct, zoneWide, zoneWideGroup)
}
