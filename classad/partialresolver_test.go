package classad

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// overlay returns a partial resolver answering exactly the unscoped names in vals.
func overlay(vals map[string]Value) func(string, ast.AttributeScope) (Value, bool) {
	return func(name string, scope ast.AttributeScope) (Value, bool) {
		if scope != ast.NoScope {
			return Value{}, false
		}
		for k, v := range vals {
			if strings.EqualFold(k, name) {
				return v, true
			}
		}
		return Value{}, false
	}
}

func evalWithOverlay(t *testing.T, adSrc, exprSrc string, vals map[string]Value) Value {
	t.Helper()
	ad, err := Parse(adSrc)
	if err != nil {
		t.Fatalf("Parse(%q): %v", adSrc, err)
	}
	expr, err := parser.ParseExpr(exprSrc)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", exprSrc, err)
	}
	ev := NewEvaluator(ad)
	ev.SetPartialResolver(overlay(vals))
	return ev.Evaluate(expr)
}

// TestPartialResolverAnswersAndDeclines is the contract: an answered name is the overlay's, a
// declined one resolves exactly as it would with nothing installed.
func TestPartialResolverAnswersAndDeclines(t *testing.T) {
	const ad = `[ x = 1; y = 2; A = x + 10 ]`
	vals := map[string]Value{"x": NewIntValue(100)}

	cases := []struct {
		expr string
		want string
	}{
		{"x", "100"},             // answered
		{"y", "2"},               // declined: the ad
		{"Missing", "undefined"}, // declined: absent from the ad
		{"X", "100"},             // names fold
		{"MY.x", "1"},            // a scoped reference is declined by this overlay
		{"x + y", "102"},
		// AN ATTRIBUTE'S OWN VALUE IS NOT OVERLAID. A is `x + 10` with the AD's x.
		{"A", "11"},
		{"MY.A", "11"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			if got := evalWithOverlay(t, ad, tc.expr, vals).String(); got != tc.want {
				t.Errorf("%s = %s, want %s", tc.expr, got, tc.want)
			}
		})
	}
}

// TestPartialResolverCurrentTime covers the case a decline exists for. Returning undefined to say
// "not mine" cannot work here, because undefined is how the evaluator is told the ad does not
// define CurrentTime.
func TestPartialResolverCurrentTime(t *testing.T) {
	// The ad DEFINES CurrentTime as undefined. Declining leaves that intact: the ad's value wins,
	// and it is undefined rather than the clock.
	if got := evalWithOverlay(t, `[ CurrentTime = undefined ]`, "CurrentTime", nil).String(); got != "undefined" {
		t.Errorf("a declined CurrentTime the ad defines as undefined = %s, want undefined", got)
	}

	// An ad that does not define it still gets a time, which is a large integer rather than
	// undefined.
	v := evalWithOverlay(t, `[ Other = 1 ]`, "CurrentTime", nil)
	if !v.IsInteger() {
		t.Errorf("a declined CurrentTime in an ad without one = %s, want a time", v)
	}

	// An overlay that answers it owns it.
	got := evalWithOverlay(t, `[ Other = 1 ]`, "CurrentTime", map[string]Value{"CurrentTime": NewIntValue(7)})
	if got.String() != "7" {
		t.Errorf("an answered CurrentTime = %s, want 7", got)
	}
}

// TestPartialResolverComposesWithSetResolver checks what a decline falls through TO: the full
// resolver when one is installed, and not the ClassAd scope.
func TestPartialResolverComposesWithSetResolver(t *testing.T) {
	ad, err := Parse(`[ x = 1; y = 2 ]`)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := parser.ParseExpr("x + y")
	if err != nil {
		t.Fatal(err)
	}
	ev := NewEvaluator(ad)
	ev.SetResolver(func(name string, _ ast.AttributeScope) Value {
		return NewIntValue(1000) // every name the partial resolver declines
	})
	ev.SetPartialResolver(overlay(map[string]Value{"x": NewIntValue(5)}))
	if got := ev.Evaluate(expr).String(); got != "1005" {
		t.Errorf("x + y = %s, want 1005: x from the overlay, y from the full resolver", got)
	}
}

// TestPartialResolverClears checks that nil removes it.
func TestPartialResolverClears(t *testing.T) {
	ad, err := Parse(`[ x = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := parser.ParseExpr("x")
	if err != nil {
		t.Fatal(err)
	}
	ev := NewEvaluator(ad)
	ev.SetPartialResolver(overlay(map[string]Value{"x": NewIntValue(9)}))
	if got := ev.Evaluate(expr).String(); got != "9" {
		t.Fatalf("overlaid x = %s, want 9", got)
	}
	ev.SetPartialResolver(nil)
	if got := ev.Evaluate(expr).String(); got != "1" {
		t.Errorf("x after clearing = %s, want 1", got)
	}
}

// TestPartialResolverSurvivesCyclic checks that the suppression around an attribute's own
// expression is restored when a cyclic reference unwinds through it, so a later reference in the
// same evaluator is still overlaid.
func TestPartialResolverSurvivesCyclic(t *testing.T) {
	ad, err := Parse(`[ Cyc = Cyc + 1; x = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	ev := NewEvaluator(ad)
	ev.SetPartialResolver(overlay(map[string]Value{"x": NewIntValue(50)}))

	// Resolving Cyc enters evalAttrExpr, which suppresses the overlay, and leaves it by panicking.
	cyc, err := parser.ParseExpr("Cyc")
	if err != nil {
		t.Fatal(err)
	}
	var cycResult Value
	func() {
		defer RecoverCyclic(&cycResult)
		cycResult = ev.Evaluate(cyc)
	}()
	if !cycResult.IsError() {
		t.Fatalf("Cyc = %s, want error", cycResult)
	}

	xe, err := parser.ParseExpr("x")
	if err != nil {
		t.Fatal(err)
	}
	if v := ev.Evaluate(xe).String(); v != "50" {
		t.Errorf("x after a cyclic unwind = %s, want 50", v)
	}
}
