package classad

import (
	"encoding/json"
	"testing"
)

func TestMarshalJSON_SimpleValues(t *testing.T) {
	ad, err := Parse(`[
		x = 5;
		y = 3.14;
		name = "test";
		active = true;
		inactive = false
	]`)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	// Unmarshal to verify structure
	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Verify values
	if result["x"] != float64(5) {
		t.Errorf("Expected x=5, got %v", result["x"])
	}
	if result["y"] != 3.14 {
		t.Errorf("Expected y=3.14, got %v", result["y"])
	}
	if result["name"] != "test" {
		t.Errorf("Expected name='test', got %v", result["name"])
	}
	if result["active"] != true {
		t.Errorf("Expected active=true, got %v", result["active"])
	}
	if result["inactive"] != false {
		t.Errorf("Expected inactive=false, got %v", result["inactive"])
	}
}

func TestMarshalJSON_Expression(t *testing.T) {
	ad, err := Parse(`[
		x = 5;
		y = x + 3
	]`)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	// Unmarshal to check the expression format
	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// x should be a simple number
	if result["x"] != float64(5) {
		t.Errorf("Expected x=5, got %v", result["x"])
	}

	// y should be an expression string
	// Note: json.Unmarshal automatically converts \/ to / when parsing JSON
	yStr, ok := result["y"].(string)
	if !ok {
		t.Fatalf("Expected y to be a string, got %T", result["y"])
	}
	if yStr != "/Expr((x + 3))/" {
		t.Errorf("Expected y to be '/Expr((x + 3))/', got %q", yStr)
	}
}

func TestMarshalJSON_List(t *testing.T) {
	ad, err := Parse(`[nums = {1, 2, 3, 4, 5}]`)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	numsList, ok := result["nums"].([]interface{})
	if !ok {
		t.Fatalf("Expected nums to be a list, got %T", result["nums"])
	}

	expected := []float64{1, 2, 3, 4, 5}
	if len(numsList) != len(expected) {
		t.Fatalf("Expected list length %d, got %d", len(expected), len(numsList))
	}

	for i, exp := range expected {
		if numsList[i] != exp {
			t.Errorf("Element %d: expected %v, got %v", i, exp, numsList[i])
		}
	}
}

func TestMarshalJSON_NestedClassAd(t *testing.T) {
	ad, err := Parse(`[
		config = [
			timeout = 30;
			retries = 3;
			server = "example.com"
		];
		enabled = true
	]`)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Check nested object
	config, ok := result["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected config to be a nested object, got %T", result["config"])
	}

	if config["timeout"] != float64(30) {
		t.Errorf("Expected timeout=30, got %v", config["timeout"])
	}
	if config["retries"] != float64(3) {
		t.Errorf("Expected retries=3, got %v", config["retries"])
	}
	if config["server"] != "example.com" {
		t.Errorf("Expected server='example.com', got %v", config["server"])
	}
}

func TestUnmarshalJSON_SimpleValues(t *testing.T) {
	jsonStr := `{
		"x": 5,
		"y": 3.14,
		"name": "test",
		"active": true,
		"inactive": false
	}`

	var ad ClassAd
	if err := json.Unmarshal([]byte(jsonStr), &ad); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Evaluate and check values
	x := ad.EvaluateAttr("x")
	if !x.IsInteger() || x.String() != "5" {
		t.Errorf("Expected x=5, got %v", x)
	}

	y := ad.EvaluateAttr("y")
	if !y.IsReal() {
		t.Errorf("Expected y to be real, got %v", y.Type())
	}

	name := ad.EvaluateAttr("name")
	if !name.IsString() {
		t.Errorf("Expected name to be string, got %v", name.Type())
	}
	nameStr, _ := name.StringValue()
	if nameStr != "test" {
		t.Errorf("Expected name='test', got %q", nameStr)
	}

	active := ad.EvaluateAttr("active")
	if !active.IsBool() {
		t.Errorf("Expected active to be bool, got %v", active.Type())
	}
	activeBool, _ := active.BoolValue()
	if !activeBool {
		t.Errorf("Expected active=true, got false")
	}
}

func TestUnmarshalJSON_Expression(t *testing.T) {
	// JSON with \/Expr format (forward slash escaped in JSON)
	jsonStr := "{\"x\": 5, \"y\": \"\\/Expr(x + 3)\\/\"}"

	var ad ClassAd
	if err := json.Unmarshal([]byte(jsonStr), &ad); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// x should evaluate to 5
	x := ad.EvaluateAttr("x")
	if !x.IsInteger() {
		t.Fatalf("Expected x to be integer, got %v", x.Type())
	}
	xInt, _ := x.IntValue()
	if xInt != 5 {
		t.Errorf("Expected x=5, got %d", xInt)
	}

	// y should evaluate to 8 (x + 3)
	y := ad.EvaluateAttr("y")
	if !y.IsInteger() {
		t.Fatalf("Expected y to be integer, got %v", y.Type())
	}
	yInt, _ := y.IntValue()
	if yInt != 8 {
		t.Errorf("Expected y=8, got %d", yInt)
	}
}

func TestUnmarshalJSON_List(t *testing.T) {
	jsonStr := `{"nums": [1, 2, 3, 4, 5]}`

	var ad ClassAd
	if err := json.Unmarshal([]byte(jsonStr), &ad); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	nums := ad.EvaluateAttr("nums")
	if !nums.IsList() {
		t.Fatalf("Expected nums to be a list, got %v", nums.Type())
	}

	numsList, _ := nums.ListValue()
	expected := []int64{1, 2, 3, 4, 5}
	if len(numsList) != len(expected) {
		t.Fatalf("Expected list length %d, got %d", len(expected), len(numsList))
	}

	for i, exp := range expected {
		if !numsList[i].IsInteger() {
			t.Errorf("Element %d: expected integer, got %v", i, numsList[i].Type())
			continue
		}
		val, _ := numsList[i].IntValue()
		if val != exp {
			t.Errorf("Element %d: expected %d, got %d", i, exp, val)
		}
	}
}

func TestUnmarshalJSON_NestedClassAd(t *testing.T) {
	jsonStr := `{
		"config": {
			"timeout": 30,
			"retries": 3,
			"server": "example.com"
		},
		"enabled": true
	}`

	var ad ClassAd
	if err := json.Unmarshal([]byte(jsonStr), &ad); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Check nested ClassAd
	config := ad.EvaluateAttr("config")
	if !config.IsClassAd() {
		t.Fatalf("Expected config to be a ClassAd, got %v", config.Type())
	}

	configAd, _ := config.ClassAdValue()
	timeout := configAd.EvaluateAttr("timeout")
	if !timeout.IsInteger() {
		t.Errorf("Expected timeout to be integer, got %v", timeout.Type())
	}
	timeoutInt, _ := timeout.IntValue()
	if timeoutInt != 30 {
		t.Errorf("Expected timeout=30, got %d", timeoutInt)
	}
}

func TestRoundTrip_ComplexClassAd(t *testing.T) {
	// Create a complex ClassAd
	original := `[
		x = 10;
		y = x * 2;
		name = "test";
		nums = {1, 2, 3};
		config = [a = 1; b = 2];
		result = x + y
	]`

	ad, err := Parse(original)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	// Marshal to JSON
	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	// Unmarshal back
	var ad2 ClassAd
	if err := json.Unmarshal(jsonBytes, &ad2); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Compare results
	result1 := ad.EvaluateAttr("result")
	result2 := ad2.EvaluateAttr("result")

	if result1.Type() != result2.Type() {
		t.Errorf("Type mismatch: %v vs %v", result1.Type(), result2.Type())
	}

	if result1.String() != result2.String() {
		t.Errorf("Value mismatch: %v vs %v", result1, result2)
	}
}

func TestMarshalJSON_UndefinedValue(t *testing.T) {
	ad, err := Parse(`[x = undefined]`)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if result["x"] != nil {
		t.Errorf("Expected x to be null, got %v", result["x"])
	}
}

func TestUnmarshalJSON_ComplexExpression(t *testing.T) {
	// JSON with \/Expr format (forward slash escaped in JSON)
	jsonStr := "{\"cpus\": 4, \"memory\": 8192, \"score\": \"\\/Expr((cpus * 100) + (memory / 1024))\\/\"}"

	var ad ClassAd
	if err := json.Unmarshal([]byte(jsonStr), &ad); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	// Evaluate the expression
	score := ad.EvaluateAttr("score")
	if !score.IsInteger() {
		t.Fatalf("Expected score to be integer, got %v", score.Type())
	}

	// score should be (4 * 100) + (8192 / 1024) = 400 + 8 = 408
	scoreInt, _ := score.IntValue()
	if scoreInt != 408 {
		t.Errorf("Expected score=408, got %d", scoreInt)
	}
}

func TestMarshalJSON_EmptyClassAd(t *testing.T) {
	ad := New()
	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal empty ClassAd: %v", err)
	}

	if string(jsonBytes) != "{}" {
		t.Errorf("Expected '{}', got %q", string(jsonBytes))
	}
}

func TestUnmarshalJSON_EmptyObject(t *testing.T) {
	jsonStr := `{}`
	var ad ClassAd
	if err := json.Unmarshal([]byte(jsonStr), &ad); err != nil {
		t.Fatalf("Failed to unmarshal empty JSON: %v", err)
	}

	// Should have no attributes
	if ad.ad != nil && len(ad.ad.Attributes) != 0 {
		t.Errorf("Expected 0 attributes, got %d", len(ad.ad.Attributes))
	}
}

func TestMarshalJSON_ConditionalExpression(t *testing.T) {
	ad, err := Parse(`[x = 5; result = x > 3 ? "yes" : "no"]`)
	if err != nil {
		t.Fatalf("Failed to parse ClassAd: %v", err)
	}

	jsonBytes, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	// Unmarshal and check
	var ad2 ClassAd
	if err := json.Unmarshal(jsonBytes, &ad2); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	result := ad2.EvaluateAttr("result")
	if !result.IsString() {
		t.Fatalf("Expected result to be string, got %v", result.Type())
	}
	resultStr, _ := result.StringValue()
	if resultStr != "yes" {
		t.Errorf("Expected result='yes', got %q", resultStr)
	}
}

func TestUnmarshalJSON_BothExpressionFormats(t *testing.T) {
	// The JSON standard allows both / and \/ as equivalent representations.
	// When Go's json.Unmarshal processes JSON, it automatically converts
	// "\/Expr(...)\/" to "/Expr(...)/", so we only need to accept the
	// unescaped format in our code.
	tests := []struct {
		name     string
		jsonStr  string
		expected int64
	}{
		{
			name:     "Escaped format in JSON (auto-unescaped by json.Unmarshal)",
			jsonStr:  "{\"x\": 5, \"y\": \"\\/Expr(x + 3)\\/\"}",
			expected: 8,
		},
		{
			name:     "Unescaped format",
			jsonStr:  "{\"x\": 5, \"y\": \"/Expr(x + 3)/\"}",
			expected: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ad ClassAd
			if err := json.Unmarshal([]byte(tt.jsonStr), &ad); err != nil {
				t.Fatalf("Failed to unmarshal JSON: %v", err)
			}

			y := ad.EvaluateAttr("y")
			if !y.IsInteger() {
				t.Fatalf("Expected y to be integer, got %v", y.Type())
			}
			yInt, _ := y.IntValue()
			if yInt != tt.expected {
				t.Errorf("Expected y=%d, got %d", tt.expected, yInt)
			}
		})
	}
}
