package classad

import (
	"strings"
	"testing"
)

// TestParseExprOldKeepsUnknownEscapes is the regression for the schedd job_queue.log parse
// failure: a TransferInput string whose filename contains an escaped comma and space (so it
// does not split the transfer list) is written by the schedd as old-ClassAd text. Only
// ParseExprOld keeps them literally (old-ClassAd semantics) so the value round-trips, which
// is why every job_queue.log value must go through it.
//
// ParseExpr does not reject the value -- it used to, and this test used to assert that, but
// the reference accepts an undefined escape in new syntax too and merely drops the backslash
// (convert_escapes(), classad/util.cpp). That makes the wrong parser WORSE than a loud
// failure, not better: it silently rewrites the filename. Pinned here so the distinction
// stays visible at the call sites that have to choose.
func TestParseExprOldKeepsUnknownEscapes(t *testing.T) {
	in := `"osdf:///chtc/staging/c/ckoch5/tests/space-comma/testing\,\ more"`

	newSyntax, err := ParseExpr(in)
	if err != nil {
		t.Fatalf("ParseExpr should accept undefined escapes as the reference does: %v", err)
	}
	if s, serr := newSyntax.Eval(New()).StringValue(); serr != nil {
		t.Fatalf("expected a string value: %v", serr)
	} else if strings.Contains(s, `\`) {
		t.Errorf("new-ClassAd parsing must drop the backslashes, got %q", s)
	} else if !strings.HasSuffix(s, "testing, more") {
		t.Errorf("new-ClassAd parsing should yield the de-escaped name, got %q", s)
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
