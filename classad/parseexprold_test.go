package classad

import (
	"strings"
	"testing"
)

// TestParseExprOldKeepsUnknownEscapes is the regression for the schedd job_queue.log parse
// failure: a TransferInput string whose filename contains an escaped comma and space (so it
// does not split the transfer list) is written by the schedd as old-ClassAd text. Strict
// ParseExpr rejects the unknown escapes and the whole attribute is dropped; ParseExprOld keeps
// them literally (old-ClassAd semantics) so the value round-trips.
func TestParseExprOldKeepsUnknownEscapes(t *testing.T) {
	in := `"osdf:///chtc/staging/c/ckoch5/tests/space-comma/testing\,\ more"`

	if _, err := ParseExpr(in); err == nil {
		t.Fatal("ParseExpr should reject the unknown escapes (strict new-ClassAd mode)")
	}

	e, err := ParseExprOld(in)
	if err != nil {
		t.Fatalf("ParseExprOld rejected old-ClassAd escapes: %v", err)
	}
	v := e.Eval(New())
	s, err := v.StringValue()
	if err != nil {
		t.Fatalf("expected a string value: %v", err)
	}
	// Old-ClassAd keeps the backslash and the following character both.
	if !strings.Contains(s, `\,`) || !strings.Contains(s, `\ `) {
		t.Errorf("old-ClassAd should keep backslash escapes literally, got %q", s)
	}
	if !strings.Contains(s, "space-comma") {
		t.Errorf("value not preserved: %q", s)
	}
}
