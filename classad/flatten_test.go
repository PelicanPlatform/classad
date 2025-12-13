package classad

import (
	"strings"
	"testing"
)

func TestFlattenPartialEvaluation(t *testing.T) {
	ad, err := Parse(`[A = 1; B = 2]`)
	if err != nil {
		t.Fatalf("failed to parse base ad: %v", err)
	}

	expr, err := ParseExpr("A + B + C")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	flat := ad.Flatten(expr)
	if !strings.Contains(flat.String(), "3") {
		t.Fatalf("expected flattened expression to include computed constant, got %s", flat.String())
	}

	ad.InsertAttr("C", 4)
	if val := flat.Eval(ad); !val.IsInteger() {
		t.Fatalf("flattened eval not integer: %v", val.Type())
	} else if i, _ := val.IntValue(); i != 7 {
		t.Fatalf("unexpected flattened eval result: %d", i)
	}
}

func TestFlattenUnaryPreservesUndefined(t *testing.T) {
	ad, err := Parse(`[X = 5]`)
	if err != nil {
		t.Fatalf("parse ad failed: %v", err)
	}

	expr, err := ParseExpr("-(UndefinedAttr) + X")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	flat := ad.Flatten(expr)
	if strings.Contains(flat.String(), "-UndefinedAttr") == false {
		t.Fatalf("expected undefined attribute to remain in expression, got %s", flat.String())
	}
}

func TestFlattenListValue(t *testing.T) {
	ad := New()
	InsertAttrList(ad, "List", []int64{1, 2, 3})
	expr, err := ParseExpr("List")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	flat := ad.Flatten(expr)
	if flat.String() == "undefined" {
		t.Fatalf("expected list flattening to produce list literal, got %s", flat.String())
	}
	if !strings.Contains(flat.String(), "{1, 2, 3}") {
		t.Fatalf("expected flattened list literal content, got %s", flat.String())
	}
}

func TestFlattenBoolValue(t *testing.T) {
	ad := New()
	ad.InsertAttrBool("Flag", true)

	expr, err := ParseExpr("Flag")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	flat := ad.Flatten(expr)
	if flat.String() != "true" {
		t.Fatalf("expected boolean literal after flatten, got %s", flat.String())
	}
}

func TestFlattenClassAdValue(t *testing.T) {
	inner := New()
	inner.InsertAttr("X", 1)

	outer := New()
	outer.InsertAttrClassAd("Nested", inner)

	expr, err := ParseExpr("Nested")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	flat := outer.Flatten(expr)
	if flat.String() == "undefined" {
		t.Fatalf("expected flatten to yield record literal, got %s", flat.String())
	}
	if !strings.Contains(flat.String(), "X = 1") {
		t.Fatalf("expected flattened record content, got %s", flat.String())
	}
}

func TestFlattenPreservesUnknownReference(t *testing.T) {
	ad := New()
	ad.InsertAttr("Known", 2)

	expr, err := ParseExpr("Unknown + 1")
	if err != nil {
		t.Fatalf("parse expr failed: %v", err)
	}

	flat := ad.Flatten(expr)
	if !strings.Contains(flat.String(), "Unknown + 1") {
		t.Fatalf("expected unknown reference to remain, got %s", flat.String())
	}
}
