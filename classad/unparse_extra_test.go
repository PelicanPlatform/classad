package classad

import "testing"

func TestUnparseInvalidArgument(t *testing.T) {
	ad, err := Parse(`[X = 1; Y = unparse(1)]`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	val := ad.EvaluateAttr("Y")
	if !val.IsError() {
		t.Fatalf("expected error value for unparse with non-attribute argument, got %v", val.Type())
	}
}
