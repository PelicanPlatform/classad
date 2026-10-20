package collections

import (
	"strings"
	"testing"
)

// TestJobScopeTargetDependence pins the dependence classification the match paths
// rely on.
func TestJobScopeTargetDependence(t *testing.T) {
	t.Parallel()
	job := mustAd(t, `[ A = 1; B = MY.A + 1; C = TARGET.X; D = MY.C; E = Missing; F = MY.Missing;
		G = eval("X"); H = PARENT.X; I = { MY.D }; J = [ k = TARGET.Y ].k; K = MY.K2; K2 = MY.K ]`)
	js := jobValues(job)
	for name, dep := range map[string]bool{
		"a": false, "b": false, "c": true, "d": true, "e": true, "f": false,
		"g": true, "h": false, "i": true, "j": true, "k": false, "k2": false,
	} {
		if js.dep[name] != dep {
			t.Errorf("%s: target-dependent = %v, want %v", name, js.dep[name], dep)
		}
		if _, hasVal := js.vals[name]; hasVal == dep {
			t.Errorf("%s: has a precomputed value = %v, want %v", name, hasVal, !dep)
		}
	}
	reads, ok := js.slotReads(mustAd(t, `[ R = MY.D + Missing + MY.B ]`).AST().Attributes[0].Value)
	if !ok || strings.Join(reads, ",") != "Missing,X" && strings.Join(reads, ",") != "X,Missing" {
		t.Errorf("slotReads = %v (ok=%v), want X and Missing", reads, ok)
	}
	if _, ok := js.slotReads(mustAd(t, `[ R = MY.G ]`).AST().Attributes[0].Value); ok {
		t.Error("slotReads through eval() must report unknown reads")
	}
}
