package classad

import (
	"testing"
)

// Tests for nested ClassAds and lists

func TestEvaluateNestedList(t *testing.T) {
	ad, err := Parse(`[x = {1, 2, 3}; y = {4, 5, 6}]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	xVal := ad.EvaluateAttr("x")
	if !xVal.IsList() {
		t.Error("x should be a list")
	}

	list, err := xVal.ListValue()
	if err != nil {
		t.Fatalf("Failed to get list value: %v", err)
	}

	if len(list) != 3 {
		t.Errorf("Expected list length 3, got %d", len(list))
	}

	// Check values
	for i, expected := range []int64{1, 2, 3} {
		val, _ := list[i].IntValue()
		if val != expected {
			t.Errorf("Element %d: expected %d, got %d", i, expected, val)
		}
	}
}

func TestEvaluateNestedClassAd(t *testing.T) {
	ad, err := Parse(`[nested = [a = 1; b = 2]; outer = 10]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	nestedVal := ad.EvaluateAttr("nested")
	if !nestedVal.IsClassAd() {
		t.Error("nested should be a ClassAd")
	}

	nestedAd, err := nestedVal.ClassAdValue()
	if err != nil {
		t.Fatalf("Failed to get ClassAd value: %v", err)
	}

	// Evaluate attributes in nested ClassAd
	if a, ok := nestedAd.EvaluateAttrInt("a"); !ok || a != 1 {
		t.Errorf("Expected a=1, got %d, ok=%v", a, ok)
	}

	if b, ok := nestedAd.EvaluateAttrInt("b"); !ok || b != 2 {
		t.Errorf("Expected b=2, got %d, ok=%v", b, ok)
	}
}

func TestEvaluateListOfStrings(t *testing.T) {
	ad, err := Parse(`[names = {"alice", "bob", "charlie"}]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	namesVal := ad.EvaluateAttr("names")
	if !namesVal.IsList() {
		t.Error("names should be a list")
	}

	list, _ := namesVal.ListValue()
	expected := []string{"alice", "bob", "charlie"}

	for i, exp := range expected {
		val, _ := list[i].StringValue()
		if val != exp {
			t.Errorf("Element %d: expected %s, got %s", i, exp, val)
		}
	}
}

// Tests for 'is' and 'isnt' operators

func TestIsOperator(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected bool
	}{
		{"int is int same value", "[x = 5 is 5]", "x", true},
		{"int is int diff value", "[x = 5 is 3]", "x", false},
		{"int is real", "[x = 5 is 5.0]", "x", false}, // Different types
		{"string is string same", `[x = "hello" is "hello"]`, "x", true},
		{"string is string diff", `[x = "hello" is "world"]`, "x", false},
		{"bool is bool true", "[x = true is true]", "x", true},
		{"bool is bool false", "[x = false is false]", "x", true},
		{"bool is bool diff", "[x = true is false]", "x", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrBool(tt.attr)
			if !ok {
				t.Fatalf("Failed to evaluate %s as bool", tt.attr)
			}

			if val != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, val)
			}
		})
	}
}

func TestIsOperatorSpecialValues(t *testing.T) {
	// Test undefined is undefined - result should be true
	ad1, _ := Parse("[x = undefined is undefined]")
	val1 := ad1.EvaluateAttr("x")
	if !val1.IsBool() {
		t.Error("undefined is undefined should return boolean")
	}
	if b, _ := val1.BoolValue(); !b {
		t.Error("undefined is undefined should be true")
	}

	// Test error is error - result should be true
	ad2, _ := Parse("[x = error is error]")
	val2 := ad2.EvaluateAttr("x")
	if !val2.IsBool() {
		t.Error("error is error should return boolean")
	}
	if b, _ := val2.BoolValue(); !b {
		t.Error("error is error should be true")
	}

	// Test undefined is error - result should be false
	ad3, _ := Parse("[x = undefined is error]")
	val3 := ad3.EvaluateAttr("x")
	if !val3.IsBool() {
		t.Error("undefined is error should return boolean")
	}
	if b, _ := val3.BoolValue(); b {
		t.Error("undefined is error should be false")
	}
}

func TestIsntOperator(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected bool
	}{
		{"int isnt int same value", "[x = 5 isnt 5]", "x", false},
		{"int isnt int diff value", "[x = 5 isnt 3]", "x", true},
		{"int isnt real", "[x = 5 isnt 5.0]", "x", true}, // Different types
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrBool(tt.attr)
			if !ok {
				t.Fatalf("Failed to evaluate %s as bool", tt.attr)
			}

			if val != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, val)
			}
		})
	}
}

func TestIsntOperatorSpecialValues(t *testing.T) {
	// Test undefined isnt error - result should be true
	ad, _ := Parse("[x = undefined isnt error]")
	val := ad.EvaluateAttr("x")
	if !val.IsBool() {
		t.Error("undefined isnt error should return boolean")
	}
	if b, _ := val.BoolValue(); !b {
		t.Error("undefined isnt error should be true")
	}
}

func TestIsWithLists(t *testing.T) {
	ad, err := Parse(`[
		list1 = {1, 2, 3};
		list2 = {1, 2, 3};
		list3 = {1, 2, 4};
		same = list1 is list2;
		diff = list1 is list3
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if same, ok := ad.EvaluateAttrBool("same"); !ok || !same {
		t.Error("Identical lists should be 'is' each other")
	}

	if diff, ok := ad.EvaluateAttrBool("diff"); !ok || diff {
		t.Error("Different lists should not be 'is' each other")
	}
}

// Tests for built-in functions

func TestStrcatFunction(t *testing.T) {
	ad, err := Parse(`[greeting = strcat("Hello", " ", "World")]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	greeting, ok := ad.EvaluateAttrString("greeting")
	if !ok {
		t.Fatal("Failed to evaluate greeting")
	}

	if greeting != "Hello World" {
		t.Errorf("Expected 'Hello World', got '%s'", greeting)
	}
}

func TestSubstrFunction(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected string
	}{
		{"substr with length", `[x = substr("Hello World", 0, 5)]`, "x", "Hello"},
		{"substr without length", `[x = substr("Hello World", 6)]`, "x", "World"},
		{"substr negative offset", `[x = substr("Hello World", -5)]`, "x", "World"},
		{"substr out of bounds", `[x = substr("Hello", 10)]`, "x", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrString(tt.attr)
			if !ok {
				t.Fatalf("Failed to evaluate %s", tt.attr)
			}

			if val != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, val)
			}
		})
	}
}

func TestSizeFunction(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected int64
	}{
		{"size of string", `[x = size("hello")]`, "x", 5},
		{"size of list", `[x = size({1, 2, 3, 4})]`, "x", 4},
		{"size of empty list", `[x = size({})]`, "x", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrInt(tt.attr)
			if !ok {
				t.Fatalf("Failed to evaluate %s", tt.attr)
			}

			if val != tt.expected {
				t.Errorf("Expected %d, got %d", tt.expected, val)
			}
		})
	}
}

func TestToLowerUpperFunctions(t *testing.T) {
	ad, err := Parse(`[
		lower = toLower("HELLO");
		upper = toUpper("world")
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if lower, ok := ad.EvaluateAttrString("lower"); !ok || lower != "hello" {
		t.Errorf("Expected 'hello', got '%s'", lower)
	}

	if upper, ok := ad.EvaluateAttrString("upper"); !ok || upper != "WORLD" {
		t.Errorf("Expected 'WORLD', got '%s'", upper)
	}
}

func TestMathFunctions(t *testing.T) {
	ad, err := Parse(`[
		f = floor(3.7);
		c = ceiling(3.2);
		r = round(3.5);
		i = int(3.9);
		rl = real(5)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if f, ok := ad.EvaluateAttrInt("f"); !ok || f != 3 {
		t.Errorf("floor(3.7): expected 3, got %d", f)
	}

	if c, ok := ad.EvaluateAttrInt("c"); !ok || c != 4 {
		t.Errorf("ceiling(3.2): expected 4, got %d", c)
	}

	if r, ok := ad.EvaluateAttrInt("r"); !ok || r != 4 {
		t.Errorf("round(3.5): expected 4, got %d", r)
	}

	if i, ok := ad.EvaluateAttrInt("i"); !ok || i != 3 {
		t.Errorf("int(3.9): expected 3, got %d", i)
	}

	if rl, ok := ad.EvaluateAttrReal("rl"); !ok || rl != 5.0 {
		t.Errorf("real(5): expected 5.0, got %g", rl)
	}
}

func TestTypeCheckingFunctions(t *testing.T) {
	ad, err := Parse(`[
		str = "hello";
		num = 42;
		flt = 3.14;
		flag = true;
		lst = {1, 2, 3};
		rec = [a = 1];
		undef = undefined;
		err = error;
		
		checkStr = isString(str);
		checkInt = isInteger(num);
		checkReal = isReal(flt);
		checkBool = isBoolean(flag);
		checkList = isList(lst);
		checkClassAd = isClassAd(rec);
		checkUndef = isUndefined(undef);
		checkErr = isError(err);
		
		notStr = isString(num);
		notInt = isInteger(str)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	trueChecks := []string{"checkStr", "checkInt", "checkReal", "checkBool", "checkList", "checkClassAd", "checkUndef", "checkErr"}
	for _, attr := range trueChecks {
		if val, ok := ad.EvaluateAttrBool(attr); !ok || !val {
			t.Errorf("%s should be true, got %v", attr, val)
		}
	}

	falseChecks := []string{"notStr", "notInt"}
	for _, attr := range falseChecks {
		if val, ok := ad.EvaluateAttrBool(attr); !ok || val {
			t.Errorf("%s should be false, got %v", attr, val)
		}
	}
}

func TestMemberFunction(t *testing.T) {
	ad, err := Parse(`[
		list = {1, 2, 3, 4, 5};
		inList = member(3, list);
		notInList = member(10, list);
		names = {"alice", "bob", "charlie"};
		hasAlice = member("alice", names);
		hasEve = member("eve", names)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if val, ok := ad.EvaluateAttrBool("inList"); !ok || !val {
		t.Error("3 should be in list")
	}

	if val, ok := ad.EvaluateAttrBool("notInList"); !ok || val {
		t.Error("10 should not be in list")
	}

	if val, ok := ad.EvaluateAttrBool("hasAlice"); !ok || !val {
		t.Error("alice should be in names")
	}

	if val, ok := ad.EvaluateAttrBool("hasEve"); !ok || val {
		t.Error("eve should not be in names")
	}
}

func TestRandomFunction(t *testing.T) {
	ad, err := Parse(`[r = random()]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val, ok := ad.EvaluateAttrReal("r")
	if !ok {
		t.Fatal("Failed to evaluate random()")
	}

	if val < 0 || val > 1 {
		t.Errorf("random() should be between 0 and 1, got %g", val)
	}
}

func TestTimeFunction(t *testing.T) {
	ad, err := Parse(`[t = time()]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val, ok := ad.EvaluateAttrInt("t")
	if !ok {
		t.Fatal("Failed to evaluate time()")
	}

	// Check that it's a reasonable Unix timestamp (after year 2000, before 2100)
	if val < 946684800 || val > 4102444800 {
		t.Errorf("time() returned unreasonable value: %d", val)
	}
}

func TestFunctionWithUndefined(t *testing.T) {
	ad, err := Parse(`[x = undefined; result = size(x)]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	result := ad.EvaluateAttr("result")
	if !result.IsUndefined() {
		t.Error("Function with undefined argument should return undefined")
	}
}

func TestFunctionWithError(t *testing.T) {
	ad, err := Parse(`[x = error; result = size(x)]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	result := ad.EvaluateAttr("result")
	if !result.IsError() {
		t.Error("Function with error argument should return error")
	}
}

// Integration tests

func TestComplexNestedEvaluation(t *testing.T) {
	ad, err := Parse(`[
		data = {
			[name = "alice"; age = 30],
			[name = "bob"; age = 25],
			[name = "charlie"; age = 35]
		};
		count = size(data)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if count, ok := ad.EvaluateAttrInt("count"); !ok || count != 3 {
		t.Errorf("Expected count=3, got %d", count)
	}

	dataVal := ad.EvaluateAttr("data")
	if !dataVal.IsList() {
		t.Fatal("data should be a list")
	}

	list, _ := dataVal.ListValue()
	if len(list) != 3 {
		t.Errorf("Expected 3 elements, got %d", len(list))
	}

	// Check first element
	if list[0].IsClassAd() {
		firstAd, _ := list[0].ClassAdValue()
		if name, ok := firstAd.EvaluateAttrString("name"); !ok || name != "alice" {
			t.Errorf("Expected name='alice', got '%s'", name)
		}
	} else {
		t.Error("First element should be a ClassAd")
	}
}

func TestFunctionChaining(t *testing.T) {
	ad, err := Parse(`[result = toUpper(substr("hello world", 0, 5))]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	result, ok := ad.EvaluateAttrString("result")
	if !ok {
		t.Fatal("Failed to evaluate result")
	}

	if result != "HELLO" {
		t.Errorf("Expected 'HELLO', got '%s'", result)
	}
}
