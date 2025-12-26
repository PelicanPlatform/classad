package classad

import (
	"encoding/json"
	"testing"
)

func TestExprEqual(t *testing.T) {
	a, err := ParseExpr("Foo + bar")
	if err != nil {
		t.Fatalf("parse expr a: %v", err)
	}
	b, err := ParseExpr("foo + BAR")
	if err != nil {
		t.Fatalf("parse expr b: %v", err)
	}
	c, err := ParseExpr("foo + baz")
	if err != nil {
		t.Fatalf("parse expr c: %v", err)
	}

	if !a.Equal(b) {
		t.Fatalf("expected expressions to be equal")
	}
	if a.Equal(c) {
		t.Fatalf("expected expressions to differ")
	}
}

func TestClassAdEqual(t *testing.T) {
	ad1, err := Parse(`[
        A = 1;
        list = {1, 2};
        nested = [b = 2; a = 1];
    ]`)
	if err != nil {
		t.Fatalf("parse ad1: %v", err)
	}

	ad2, err := Parse(`[
        nested = [a = 1; b = 2];
        list = {1, 2};
        a = 1;
    ]`)
	if err != nil {
		t.Fatalf("parse ad2: %v", err)
	}

	ad3, err := Parse(`[
        nested = [a = 1; b = 3];
        list = {1, 2};
        a = 1;
    ]`)
	if err != nil {
		t.Fatalf("parse ad3: %v", err)
	}

	if !ad1.Equal(ad2) {
		t.Fatalf("expected ad1 and ad2 to be equal")
	}
	if ad1.Equal(ad3) {
		t.Fatalf("expected ad1 and ad3 to differ")
	}
}

func TestClassAdEqualAfterJSONRoundTrip(t *testing.T) {
	original, err := Parse(`[
        Z = 1;
        nested = [b = 2; A = 1; c = 3];
        alpha = [y = 20; x = 10];
        outer = [
            inner = [b = 5; a = 4];
            num = 7
        ];
        list = { [z = 9; y = 8; x = 7], 5, 4 }
    ]`)
	if err != nil {
		t.Fatalf("parse original: %v", err)
	}

	bytes1, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}

	var roundTripped ClassAd
	if err := json.Unmarshal(bytes1, &roundTripped); err != nil {
		t.Fatalf("unmarshal copy: %v", err)
	}

	if !original.Equal(&roundTripped) {
		t.Fatalf("expected original and copy to be equal after round-trip")
	}
}

func TestCaseInsensitiveAttributeReferences(t *testing.T) {
	ad, err := Parse(`[Foo = bar; BAR = 3]`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	val := ad.EvaluateAttr("Foo")
	if !val.IsInteger() {
		t.Fatalf("expected Foo to evaluate to integer, got %v", val.Type())
	}
	intVal, _ := val.IntValue()
	if intVal != 3 {
		t.Fatalf("expected Foo to evaluate to 3, got %d", intVal)
	}
}
