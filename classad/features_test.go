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

// Tests for =?= and =!= operators (meta-equal operators)

func TestMetaEqualOperator(t *testing.T) {
	ad, err := Parse(`[
		intValue = 5;
		realValue = 5.0;

		metaEqual1 = (intValue =?= intValue);
		metaEqual2 = (intValue =?= realValue);
		metaEqual3 = (5 =?= 5);
		metaEqual4 = ("hello" =?= "hello");
		metaEqual5 = (true =?= true);
		metaEqual6 = (undefined =?= undefined);
		metaEqual7 = (error =?= error)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	tests := []struct {
		attr     string
		expected bool
	}{
		{"metaEqual1", true},  // same type and value
		{"metaEqual2", false}, // different types
		{"metaEqual3", true},  // same literal
		{"metaEqual4", true},  // same string
		{"metaEqual5", true},  // same boolean
		{"metaEqual6", true},  // both undefined
		{"metaEqual7", true},  // both error
	}

	for _, test := range tests {
		val, ok := ad.EvaluateAttrBool(test.attr)
		if !ok {
			t.Errorf("%s: failed to evaluate", test.attr)
			continue
		}
		if val != test.expected {
			t.Errorf("%s: expected %v, got %v", test.attr, test.expected, val)
		}
	}
}

func TestMetaNotEqualOperator(t *testing.T) {
	ad, err := Parse(`[
		intValue = 5;
		realValue = 5.0;

		metaNotEqual1 = (intValue =!= realValue);
		metaNotEqual2 = (intValue =!= intValue);
		metaNotEqual3 = ("hello" =!= "world");
		metaNotEqual4 = (true =!= false);
		metaNotEqual5 = (5 =!= 6)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	tests := []struct {
		attr     string
		expected bool
	}{
		{"metaNotEqual1", true},  // different types
		{"metaNotEqual2", false}, // same type and value
		{"metaNotEqual3", true},  // different strings
		{"metaNotEqual4", true},  // different booleans
		{"metaNotEqual5", true},  // different values
	}

	for _, test := range tests {
		val, ok := ad.EvaluateAttrBool(test.attr)
		if !ok {
			t.Errorf("%s: failed to evaluate", test.attr)
			continue
		}
		if val != test.expected {
			t.Errorf("%s: expected %v, got %v", test.attr, test.expected, val)
		}
	}
}

func TestMetaEqualWithLists(t *testing.T) {
	ad, err := Parse(`[
		list1 = {1, 2, 3};
		list2 = {1, 2, 3};
		list3 = {1, 2, 3, 4};

		same = (list1 =?= list2);
		different = (list1 =!= list3)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	same, ok := ad.EvaluateAttrBool("same")
	if !ok || !same {
		t.Error("Expected list1 =?= list2 to be true")
	}

	different, ok := ad.EvaluateAttrBool("different")
	if !ok || !different {
		t.Error("Expected list1 =!= list3 to be true")
	}
}

// Tests for attribute selection expressions

func TestSelectExpr(t *testing.T) {
	ad, err := Parse(`[
		person = [name = "Alice"; age = 30; city = "NYC"];
		personName = person.name;
		personAge = person.age;
		personCity = person.city
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	name, ok := ad.EvaluateAttrString("personName")
	if !ok || name != "Alice" {
		t.Errorf("Expected 'Alice', got '%s'", name)
	}

	age, ok := ad.EvaluateAttrInt("personAge")
	if !ok || age != 30 {
		t.Errorf("Expected 30, got %d", age)
	}

	city, ok := ad.EvaluateAttrString("personCity")
	if !ok || city != "NYC" {
		t.Errorf("Expected 'NYC', got '%s'", city)
	}
}

func TestSelectExprNested(t *testing.T) {
	ad, err := Parse(`[
		company = [
			name = "TechCorp";
			ceo = [
				name = "Bob";
				age = 45
			]
		];
		ceoName = company.ceo.name;
		companyName = company.name
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	ceoName, ok := ad.EvaluateAttrString("ceoName")
	if !ok || ceoName != "Bob" {
		t.Errorf("Expected 'Bob', got '%s'", ceoName)
	}

	companyName, ok := ad.EvaluateAttrString("companyName")
	if !ok || companyName != "TechCorp" {
		t.Errorf("Expected 'TechCorp', got '%s'", companyName)
	}
}

func TestSelectExprUndefined(t *testing.T) {
	ad, err := Parse(`[
		person = [name = "Alice"];
		missingAttr = person.nonexistent
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val := ad.EvaluateAttr("missingAttr")
	if !val.IsUndefined() {
		t.Error("Expected undefined for non-existent attribute")
	}
}

func TestSelectExprError(t *testing.T) {
	ad, err := Parse(`[
		notARecord = 42;
		result = notARecord.someAttr
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val := ad.EvaluateAttr("result")
	if !val.IsError() {
		t.Error("Expected error when selecting from non-ClassAd")
	}
}

// Tests for subscript expressions

func TestSubscriptExprList(t *testing.T) {
	ad, err := Parse(`[
		numbers = {10, 20, 30, 40, 50};
		first = numbers[0];
		third = numbers[2];
		last = numbers[4]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	first, ok := ad.EvaluateAttrInt("first")
	if !ok || first != 10 {
		t.Errorf("Expected 10, got %d", first)
	}

	third, ok := ad.EvaluateAttrInt("third")
	if !ok || third != 30 {
		t.Errorf("Expected 30, got %d", third)
	}

	last, ok := ad.EvaluateAttrInt("last")
	if !ok || last != 50 {
		t.Errorf("Expected 50, got %d", last)
	}
}

func TestSubscriptExprListOutOfBounds(t *testing.T) {
	ad, err := Parse(`[
		numbers = {10, 20, 30};
		outOfBounds = numbers[5];
		negative = numbers[-1]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	outOfBounds := ad.EvaluateAttr("outOfBounds")
	if !outOfBounds.IsUndefined() {
		t.Error("Expected undefined for out-of-bounds index")
	}

	negative := ad.EvaluateAttr("negative")
	if !negative.IsUndefined() {
		t.Error("Expected undefined for negative index")
	}
}

func TestSubscriptExprClassAd(t *testing.T) {
	ad, err := Parse(`[
		person = [name = "Alice"; age = 30; city = "NYC"];
		personName = person["name"];
		personAge = person["age"];
		personCity = person["city"]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	name, ok := ad.EvaluateAttrString("personName")
	if !ok || name != "Alice" {
		t.Errorf("Expected 'Alice', got '%s'", name)
	}

	age, ok := ad.EvaluateAttrInt("personAge")
	if !ok || age != 30 {
		t.Errorf("Expected 30, got %d", age)
	}

	city, ok := ad.EvaluateAttrString("personCity")
	if !ok || city != "NYC" {
		t.Errorf("Expected 'NYC', got '%s'", city)
	}
}

func TestSubscriptExprNestedLists(t *testing.T) {
	ad, err := Parse(`[
		matrix = {{1, 2, 3}, {4, 5, 6}, {7, 8, 9}};
		row1 = matrix[0];
		element = matrix[1][2]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	row1Val := ad.EvaluateAttr("row1")
	if !row1Val.IsList() {
		t.Fatal("row1 should be a list")
	}

	row1, _ := row1Val.ListValue()
	if len(row1) != 3 {
		t.Errorf("Expected row1 length 3, got %d", len(row1))
	}

	element, ok := ad.EvaluateAttrInt("element")
	if !ok || element != 6 {
		t.Errorf("Expected 6, got %d", element)
	}
}

func TestSubscriptExprError(t *testing.T) {
	ad, err := Parse(`[
		notAList = 42;
		result = notAList[0]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val := ad.EvaluateAttr("result")
	if !val.IsError() {
		t.Error("Expected error when subscripting non-list/non-ClassAd")
	}
}

func TestSubscriptExprListWithNonIntegerIndex(t *testing.T) {
	ad, err := Parse(`[
		numbers = {10, 20, 30};
		result = numbers["invalid"]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val := ad.EvaluateAttr("result")
	if !val.IsError() {
		t.Error("Expected error when using non-integer index on list")
	}
}

func TestSubscriptExprClassAdWithNonStringKey(t *testing.T) {
	ad, err := Parse(`[
		person = [name = "Alice"; age = 30];
		result = person[0]
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val := ad.EvaluateAttr("result")
	if !val.IsError() {
		t.Error("Expected error when using non-string key on ClassAd")
	}
}

// Integration tests combining multiple features

func TestSelectAndSubscriptCombined(t *testing.T) {
	ad, err := Parse(`[
		data = [
			users = {"alice", "bob", "charlie"};
			scores = {95, 87, 92}
		];
		firstUser = data.users[0];
		secondScore = data.scores[1];
		userCount = size(data.users)
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	firstUser, ok := ad.EvaluateAttrString("firstUser")
	if !ok || firstUser != "alice" {
		t.Errorf("Expected 'alice', got '%s'", firstUser)
	}

	secondScore, ok := ad.EvaluateAttrInt("secondScore")
	if !ok || secondScore != 87 {
		t.Errorf("Expected 87, got %d", secondScore)
	}

	userCount, ok := ad.EvaluateAttrInt("userCount")
	if !ok || userCount != 3 {
		t.Errorf("Expected 3, got %d", userCount)
	}
}

func TestComplexNestedAccessPatterns(t *testing.T) {
	ad, err := Parse(`[
		company = [
			name = "TechCorp";
			departments = {
				[name = "Engineering"; headcount = 50],
				[name = "Sales"; headcount = 30],
				[name = "HR"; headcount = 10]
			}
		];
		engDept = company.departments[0];
		engName = company.departments[0].name;
		salesHeadcount = company.departments[1].headcount
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	engDeptVal := ad.EvaluateAttr("engDept")
	if !engDeptVal.IsClassAd() {
		t.Error("engDept should be a ClassAd")
	}

	engName, ok := ad.EvaluateAttrString("engName")
	if !ok || engName != "Engineering" {
		t.Errorf("Expected 'Engineering', got '%s'", engName)
	}

	salesHeadcount, ok := ad.EvaluateAttrInt("salesHeadcount")
	if !ok || salesHeadcount != 30 {
		t.Errorf("Expected 30, got %d", salesHeadcount)
	}
}

// Tests for scoped attribute references (MY., TARGET., PARENT.)

func TestMyScopedReference(t *testing.T) {
	ad, err := Parse(`[
		x = 10;
		y = MY.x + 5
	]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	y, ok := ad.EvaluateAttrInt("y")
	if !ok || y != 15 {
		t.Errorf("Expected 15, got %d", y)
	}
}

func TestTargetScopedReference(t *testing.T) {
	job := New()
	job.InsertAttr("RequestCpus", 4)
	job.InsertAttr("RequestMemory", 8192)

	machine := New()
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("Memory", 16384)

	// Set target for job to reference machine
	job.SetTarget(machine)

	// Parse requirements expression that references TARGET
	reqExpr, err := Parse(`[req = (TARGET.Cpus >= RequestCpus) && (TARGET.Memory >= RequestMemory)]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Copy the requirement to job
	reqValue := reqExpr.Lookup("req")
	job.Insert("Requirements", reqValue)

	// Evaluate
	matches, ok := job.EvaluateAttrBool("Requirements")
	if !ok {
		t.Fatal("Failed to evaluate Requirements")
	}
	if !matches {
		t.Error("Expected Requirements to be true")
	}
}

func TestParentScopedReference(t *testing.T) {
	parent := New()
	parent.InsertAttr("ParentValue", 100)

	child := New()
	child.InsertAttr("ChildValue", 50)
	child.SetParent(parent)

	// Parse expression with PARENT reference
	expr, err := Parse(`[result = ChildValue + PARENT.ParentValue]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Copy expression to child
	resultExpr := expr.Lookup("result")
	child.Insert("Total", resultExpr)

	// Evaluate
	total, ok := child.EvaluateAttrInt("Total")
	if !ok {
		t.Fatal("Failed to evaluate Total")
	}
	if total != 150 {
		t.Errorf("Expected 150, got %d", total)
	}
}

func TestScopedReferenceUndefined(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)

	// Parse expression with TARGET reference (but no target set)
	expr, err := Parse(`[result = TARGET.SomeAttr]`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	resultExpr := expr.Lookup("result")
	ad.Insert("Result", resultExpr)

	// Should be undefined since no target is set
	val := ad.EvaluateAttr("Result")
	if !val.IsUndefined() {
		t.Error("Expected undefined when TARGET not set")
	}
}

func TestNestedScopedReferences(t *testing.T) {
	job := New()
	job.InsertAttr("RequestCpus", 4)

	machine := New()
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("MaxCpus", 16)

	job.SetTarget(machine)
	machine.SetTarget(job)

	// Job requires machine to have enough CPUs
	jobReqExpr, _ := Parse(`[req = TARGET.Cpus >= MY.RequestCpus]`)
	job.Insert("Requirements", jobReqExpr.Lookup("req"))

	// Machine requires job to not exceed MaxCpus
	machineReqExpr, _ := Parse(`[req = TARGET.RequestCpus <= MY.MaxCpus]`)
	machine.Insert("Requirements", machineReqExpr.Lookup("req"))

	jobReq, ok1 := job.EvaluateAttrBool("Requirements")
	machineReq, ok2 := machine.EvaluateAttrBool("Requirements")

	if !ok1 || !jobReq {
		t.Error("Job requirements should be true")
	}
	if !ok2 || !machineReq {
		t.Error("Machine requirements should be true")
	}
}

// Tests for MatchClassAd

func TestMatchClassAdCreation(t *testing.T) {
	job := New()
	job.InsertAttr("JobId", 1001)

	machine := New()
	machine.InsertAttrString("Name", "slot1@worker1")

	match := NewMatchClassAd(job, machine)

	if match.GetLeftAd() != job {
		t.Error("Left ad should be job")
	}
	if match.GetRightAd() != machine {
		t.Error("Right ad should be machine")
	}

	// Check that TARGET references are set up
	if job.GetTarget() != machine {
		t.Error("Job target should be machine")
	}
	if machine.GetTarget() != job {
		t.Error("Machine target should be job")
	}
}

func TestMatchClassAdSymmetry(t *testing.T) {
	job := New()
	job.InsertAttr("RequestCpus", 4)
	job.InsertAttr("RequestMemory", 8192)

	machine := New()
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("Memory", 16384)

	// Parse requirements with TARGET references
	jobReqExpr, _ := Parse(`[r = (TARGET.Cpus >= RequestCpus) && (TARGET.Memory >= RequestMemory)]`)
	job.Insert("Requirements", jobReqExpr.Lookup("r"))

	machineReqExpr, _ := Parse(`[r = (TARGET.RequestCpus <= Cpus) && (TARGET.RequestMemory <= Memory)]`)
	machine.Insert("Requirements", machineReqExpr.Lookup("r"))

	match := NewMatchClassAd(job, machine)

	if !match.Symmetry("Requirements", "Requirements") {
		t.Error("Symmetric requirements should be true")
	}

	if !match.Match() {
		t.Error("Match() should return true")
	}
}

func TestMatchClassAdSymmetryFailure(t *testing.T) {
	job := New()
	job.InsertAttr("RequestCpus", 16) // Too many CPUs

	machine := New()
	machine.InsertAttr("Cpus", 8)

	jobReqExpr, _ := Parse(`[r = TARGET.Cpus >= RequestCpus]`)
	job.Insert("Requirements", jobReqExpr.Lookup("r"))

	machineReqExpr, _ := Parse(`[r = TARGET.RequestCpus <= Cpus]`)
	machine.Insert("Requirements", machineReqExpr.Lookup("r"))

	match := NewMatchClassAd(job, machine)

	if match.Match() {
		t.Error("Match() should return false when requirements not met")
	}
}

func TestMatchClassAdRank(t *testing.T) {
	job := New()
	job.InsertAttrBool("Requirements", true)

	machine := New()
	machine.InsertAttrBool("Requirements", true)
	machine.InsertAttr("Rank", 100)

	match := NewMatchClassAd(job, machine)

	rank, ok := match.EvaluateRankRight()
	if !ok {
		t.Fatal("Failed to evaluate rank")
	}
	if rank != 100.0 {
		t.Errorf("Expected rank 100.0, got %f", rank)
	}
}

func TestMatchClassAdRankWithTarget(t *testing.T) {
	job := New()
	job.InsertAttr("Priority", 10)
	job.InsertAttrBool("Requirements", true)

	machine := New()
	machine.InsertAttrBool("Requirements", true)

	// Machine ranks jobs by their priority
	rankExpr, _ := Parse(`[r = TARGET.Priority * 10]`)
	machine.Insert("Rank", rankExpr.Lookup("r"))

	match := NewMatchClassAd(job, machine)

	rank, ok := match.EvaluateRankRight()
	if !ok {
		t.Fatal("Failed to evaluate rank with TARGET")
	}
	if rank != 100.0 {
		t.Errorf("Expected rank 100.0, got %f", rank)
	}
}

func TestMatchClassAdReplace(t *testing.T) {
	job1 := New()
	job1.InsertAttr("JobId", 1)

	job2 := New()
	job2.InsertAttr("JobId", 2)

	machine := New()
	machine.InsertAttrString("Name", "worker1")

	match := NewMatchClassAd(job1, machine)

	// Replace left ad
	match.ReplaceLeftAd(job2)

	if match.GetLeftAd() != job2 {
		t.Error("Left ad should be job2")
	}

	// Check TARGET references updated
	if job2.GetTarget() != machine {
		t.Error("job2 target should be machine")
	}
	if machine.GetTarget() != job2 {
		t.Error("machine target should be job2")
	}
}

func TestMatchClassAdComplexScenario(t *testing.T) {
	// Simulate HTCondor-style job/machine matching
	job := New()
	job.InsertAttrString("Owner", "alice")
	job.InsertAttr("RequestCpus", 4)
	job.InsertAttr("RequestMemory", 8192)
	job.InsertAttr("RequestDisk", 100000)

	machine := New()
	machine.InsertAttrString("Name", "slot1@execute-node-01")
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("Memory", 16384)
	machine.InsertAttr("Disk", 500000)
	machine.InsertAttrString("Arch", "X86_64")

	// Job requirements
	jobReqExpr, _ := Parse(`[r = (TARGET.Cpus >= RequestCpus) &&
	                              (TARGET.Memory >= RequestMemory) &&
	                              (TARGET.Disk >= RequestDisk) &&
	                              (TARGET.Arch == "X86_64")]`)
	job.Insert("Requirements", jobReqExpr.Lookup("r"))

	// Machine requirements
	machineReqExpr, _ := Parse(`[r = (TARGET.RequestCpus <= Cpus) &&
	                                  (TARGET.RequestMemory <= Memory) &&
	                                  (TARGET.RequestDisk <= Disk)]`)
	machine.Insert("Requirements", machineReqExpr.Lookup("r"))

	// Job ranks machines by available memory
	jobRankExpr, _ := Parse(`[r = TARGET.Memory]`)
	job.Insert("Rank", jobRankExpr.Lookup("r"))

	// Machine ranks jobs by requesting fewer resources
	machineRankExpr, _ := Parse(`[r = Cpus - TARGET.RequestCpus]`)
	machine.Insert("Rank", machineRankExpr.Lookup("r"))

	match := NewMatchClassAd(job, machine)

	// Check match
	if !match.Match() {
		t.Fatal("Job and machine should match")
	}

	// Check job rank
	jobRank, ok := match.EvaluateRankLeft()
	if !ok {
		t.Fatal("Failed to evaluate job rank")
	}
	if jobRank != 16384.0 {
		t.Errorf("Expected job rank 16384.0, got %f", jobRank)
	}

	// Check machine rank
	machineRank, ok := match.EvaluateRankRight()
	if !ok {
		t.Fatal("Failed to evaluate machine rank")
	}
	if machineRank != 4.0 {
		t.Errorf("Expected machine rank 4.0, got %f", machineRank)
	}
}
