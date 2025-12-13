package classad

import "testing"

// evalBuiltin is a helper to evaluate a raw ClassAd expression.
func evalBuiltin(t *testing.T, expr string) Value {
	// Evaluate in an empty ClassAd scope.
	e, err := ParseExpr(expr)
	if err != nil {
		t.Fatalf("parse failed for %s: %v", expr, err)
	}
	return e.Eval(New())
}

func TestJoinVariants(t *testing.T) {
	if val := evalBuiltin(t, "join()"); !val.IsError() {
		t.Fatalf("expected error for empty join, got %v", val.Type())
	}

	noSep := evalBuiltin(t, `join({"a", undefined, 2, true, 1.5})`)
	if s, _ := noSep.StringValue(); s != "a2true1.5" {
		t.Fatalf("unexpected join(list) result: %q", s)
	}

	listForm := evalBuiltin(t, `join(",", {"a", "b", "c"})`)
	if s, _ := listForm.StringValue(); s != "a,b,c" {
		t.Fatalf("unexpected join(sep,list) result: %q", s)
	}

	variadic := evalBuiltin(t, `join("-", "x", 3, false)`)
	if s, _ := variadic.StringValue(); s != "x-3-false" {
		t.Fatalf("unexpected join variadic result: %q", s)
	}

	if val := evalBuiltin(t, `join(123, "a")`); !val.IsError() {
		t.Fatalf("expected error for non-string separator, got %v", val.Type())
	}
}

func TestSplitAndSlots(t *testing.T) {
	ws := evalBuiltin(t, `split("a  b   c")`)
	fields, _ := ws.ListValue()
	if len(fields) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(fields))
	}

	custom := evalBuiltin(t, `split("a,b;c", ",;")`)
	parts, _ := custom.ListValue()
	if len(parts) != 3 {
		t.Fatalf("expected 3 fields with custom delimiter, got %d", len(parts))
	}

	if val := evalBuiltin(t, `split(123)`); !val.IsError() {
		t.Fatalf("expected error for non-string split input")
	}

	slot := evalBuiltin(t, `splitSlotName("slot1@machine")`)
	slotParts, _ := slot.ListValue()
	if len(slotParts) != 2 {
		t.Fatalf("expected 2 slot parts, got %d", len(slotParts))
	}
	left, _ := slotParts[0].StringValue()
	right, _ := slotParts[1].StringValue()
	if left != "slot1" || right != "machine" {
		t.Fatalf("unexpected splitSlotName parts: %q, %q", left, right)
	}

	noAt := evalBuiltin(t, `splitSlotName("machine")`)
	noAtParts, _ := noAt.ListValue()
	first, _ := noAtParts[0].StringValue()
	second, _ := noAtParts[1].StringValue()
	if first != "" || second != "machine" {
		t.Fatalf("unexpected splitSlotName without @: %q, %q", first, second)
	}

	user := evalBuiltin(t, `splitUserName("alice@example.com")`)
	userParts, _ := user.ListValue()
	if u, _ := userParts[0].StringValue(); u != "alice" {
		t.Fatalf("unexpected username part: %q", u)
	}
	if dom, _ := userParts[1].StringValue(); dom != "example.com" {
		t.Fatalf("unexpected domain part: %q", dom)
	}
}

func TestStringComparisons(t *testing.T) {
	cmp := evalBuiltin(t, `strcmp("apple", "banana")`)
	if v, _ := cmp.IntValue(); v >= 0 {
		t.Fatalf("expected apple < banana, got %d", v)
	}

	ci := evalBuiltin(t, `stricmp("Case", "case")`)
	if v, _ := ci.IntValue(); v != 0 {
		t.Fatalf("expected case-insensitive match, got %d", v)
	}

	numeric := evalBuiltin(t, `strcmp(10, 2)`)
	if v, _ := numeric.IntValue(); v >= 0 {
		t.Fatalf("expected string compare 10 vs 2 to be negative, got %d", v)
	}

	if errVal := evalBuiltin(t, `strcmp({1}, "x")`); !errVal.IsError() {
		t.Fatalf("expected error for invalid strcmp types")
	}
}

func TestVersionComparisons(t *testing.T) {
	vc := evalBuiltin(t, `versioncmp("1.2", "1.10")`)
	if v, _ := vc.IntValue(); v >= 0 {
		t.Fatalf("expected 1.2 < 1.10, got %d", v)
	}

	if gt := evalBuiltin(t, `version_gt("8.9.1", "8.8.9")`); !gt.IsBool() {
		t.Fatalf("version_gt should return bool")
	} else if b, _ := gt.BoolValue(); !b {
		t.Fatalf("expected version_gt to be true")
	}

	inRange := evalBuiltin(t, `version_in_range("8.8.0", "8.7.0", "8.9.0")`)
	if ok, _ := inRange.BoolValue(); !ok {
		t.Fatalf("expected version in range")
	}

	undef := evalBuiltin(t, `versioncmp(undefined, "1")`)
	if !undef.IsUndefined() {
		t.Fatalf("expected undefined for versioncmp with undefined input")
	}
}

func TestIntervalAndIdentity(t *testing.T) {
	ival := evalBuiltin(t, `interval(3661)`)
	if s, _ := ival.StringValue(); s != "1:01:01" {
		t.Fatalf("unexpected interval formatting: %q", s)
	}

	ident := evalBuiltin(t, `identicalMember(1, {1, 1.0, "1"})`)
	if b, _ := ident.BoolValue(); !b {
		t.Fatalf("expected identicalMember to find matching int")
	}

	undefMatch := evalBuiltin(t, `identicalMember(undefined, {1, undefined})`)
	if b, _ := undefMatch.BoolValue(); !b {
		t.Fatalf("expected identicalMember to match undefined")
	}

	if errVal := evalBuiltin(t, `identicalMember({1}, {1})`); !errVal.IsError() {
		t.Fatalf("expected error when first arg is list")
	}
}

func TestAnyAllCompare(t *testing.T) {
	anyResult := evalBuiltin(t, `anyCompare("<", {1,2,3}, 2)`)
	if b, _ := anyResult.BoolValue(); !b {
		t.Fatalf("expected anyCompare to be true")
	}

	all := evalBuiltin(t, `allCompare(">=", {2,2,3}, 2)`)
	if b, _ := all.BoolValue(); !b {
		t.Fatalf("expected allCompare to be true")
	}

	nonList := evalBuiltin(t, `allCompare("==", 1, 1)`)
	if !nonList.IsError() {
		t.Fatalf("expected error for non-list input")
	}
}

func TestStringListBuiltins(t *testing.T) {
	size := evalBuiltin(t, `stringListSize("a, b, ,c")`)
	if v, _ := size.IntValue(); v != 3 {
		t.Fatalf("unexpected stringListSize: %d", v)
	}

	sum := evalBuiltin(t, `stringListSum("1,2.5,bad")`)
	if sum.IsInteger() {
		t.Fatalf("expected real sum due to decimal input")
	}
	if v, _ := sum.RealValue(); v != 3.5 {
		t.Fatalf("unexpected stringListSum: %g", v)
	}

	avg := evalBuiltin(t, `stringListAvg("")`)
	if v, _ := avg.RealValue(); v != 0.0 {
		t.Fatalf("expected zero average for empty list, got %g", v)
	}

	minVal := evalBuiltin(t, `stringListMin("5,3,7")`)
	if v, _ := minVal.IntValue(); v != 3 {
		t.Fatalf("unexpected stringListMin: %d", v)
	}

	maxVal := evalBuiltin(t, `stringListMax("1.5,2.5,2")`)
	if v, _ := maxVal.RealValue(); v != 2.5 {
		t.Fatalf("unexpected stringListMax: %g", v)
	}

	inter := evalBuiltin(t, `stringListsIntersect("a,b,c", "x,b,z")`)
	if b, _ := inter.BoolValue(); !b {
		t.Fatalf("expected intersection to be true")
	}

	subset := evalBuiltin(t, `stringListSubsetMatch("a,b", "a,b,c")`)
	if b, _ := subset.BoolValue(); !b {
		t.Fatalf("expected subset match to be true")
	}

	regexMember := evalBuiltin(t, `stringListRegexpMember("a.*", "abc,def")`)
	if b, _ := regexMember.BoolValue(); !b {
		t.Fatalf("expected regex member to match")
	}

	listRegex := evalBuiltin(t, `regexpMember("foo", {"bar", "foobar"})`)
	if b, _ := listRegex.BoolValue(); !b {
		t.Fatalf("expected regexpMember to match list element")
	}
}

func TestRegexAndReplace(t *testing.T) {
	replaceFirst := evalBuiltin(t, `replace("ab", "xxabyyab", "Q")`)
	if s, _ := replaceFirst.StringValue(); s != "xxQyyab" {
		t.Fatalf("unexpected replace result: %q", s)
	}

	replaceAll := evalBuiltin(t, `replaceAll("ab", "xxabyyab", "Q")`)
	if s, _ := replaceAll.StringValue(); s != "xxQyyQ" {
		t.Fatalf("unexpected replaceAll result: %q", s)
	}

	regexpsVal := evalBuiltin(t, `regexps("[0-9]+", "a1b2c", "#")`)
	if s, _ := regexpsVal.StringValue(); s != "a#b#c" {
		t.Fatalf("unexpected regexps result: %q", s)
	}

	if errVal := evalBuiltin(t, `replace("(", "a", "b")`); !errVal.IsError() {
		t.Fatalf("expected error for invalid regex pattern")
	}
}

func TestMembershipFunctions(t *testing.T) {
	member := evalBuiltin(t, `member(2, {1, 2, 3})`)
	if b, _ := member.BoolValue(); !b {
		t.Fatalf("expected member to find value")
	}

	nonMember := evalBuiltin(t, `member("x", {"a", "b"})`)
	if b, _ := nonMember.BoolValue(); b {
		t.Fatalf("expected member to be false for missing value")
	}

	memberErr := evalBuiltin(t, `member(1, 2)`)
	if !memberErr.IsError() {
		t.Fatalf("expected error when member second argument is not a list")
	}

	memberUndef := evalBuiltin(t, `member(undefined, {1})`)
	if !memberUndef.IsUndefined() {
		t.Fatalf("expected undefined when member element is undefined")
	}

	strList := evalBuiltin(t, `stringListMember("foo", "bar, foo, baz")`)
	if b, _ := strList.BoolValue(); !b {
		t.Fatalf("expected stringListMember to match")
	}

	strListIgnore := evalBuiltin(t, `stringListIMember("FOO", "bar, foo")`)
	if b, _ := strListIgnore.BoolValue(); !b {
		t.Fatalf("expected case-insensitive member to match")
	}

	avg := evalBuiltin(t, `stringListAvg("1,2,3")`)
	if v, _ := avg.RealValue(); v != 2 {
		t.Fatalf("unexpected average value: %g", v)
	}
	avgDelim := evalBuiltin(t, `stringListAvg("1.0|2.0", "|")`)
	if v, _ := avgDelim.RealValue(); v != 1.5 {
		t.Fatalf("unexpected average with delimiter: %g", v)
	}
	avgErr := evalBuiltin(t, `stringListAvg(123)`)
	if !avgErr.IsError() {
		t.Fatalf("expected error for non-string average input")
	}
	avgUndef := evalBuiltin(t, `stringListAvg(undefined)`)
	if !avgUndef.IsUndefined() {
		t.Fatalf("expected undefined when average input is undefined")
	}

	stricmpInt := evalBuiltin(t, `stricmp(10, 10)`)
	if v, _ := stricmpInt.IntValue(); v != 0 {
		t.Fatalf("expected stricmp to treat integers as equal, got %d", v)
	}

	if errVal := evalBuiltin(t, `stricmp(undefined, "x")`); !errVal.IsError() {
		t.Fatalf("expected error when stricmp receives undefined")
	}

	regexMember := evalBuiltin(t, `regexpMember("foo", {"FOO"}, "i")`)
	if b, _ := regexMember.BoolValue(); !b {
		t.Fatalf("expected regexpMember to match with options")
	}

	noMatch := evalBuiltin(t, `replace("zz", "abc", "Q")`)
	if s, _ := noMatch.StringValue(); s != "abc" {
		t.Fatalf("expected replace to return original when no match, got %q", s)
	}
}
