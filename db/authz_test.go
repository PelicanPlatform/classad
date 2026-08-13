package db

import (
	"strings"
	"testing"
)

// The authorization walker has to see references the query planner's ReadAttrs deliberately ignores.
// Each case here is a way of naming an attribute; a miss is a silent authorization hole, which is why the
// walk is reflective rather than a type switch per AST node kind.
func TestConstraintRefsSeesEveryReferenceForm(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want string // an attribute name that must appear in the result
		why  string
	}{
		{`ClaimId == "x"`, "ClaimId", "plain reference"},
		{`MY.ClaimId == "x"`, "ClaimId", "MY scope -- ReadAttrs reports nothing for this"},
		{`TARGET.ClaimId == "x"`, "ClaimId", "TARGET scope -- likewise"},
		{`regexp("^s", ClaimId)`, "ClaimId", "inside a function call"},
		{`substr(ClaimId, 0, 6) == "s"`, "ClaimId", "inside a nested function call"},
		{`size(MY.Capability) > 0`, "Capability", "scoped, inside a call"},
		{`!(ClaimId == "x")`, "ClaimId", "under a unary operator"},
		{`(ClaimId ?: "z") == "x"`, "ClaimId", "under Elvis"},
		{`Cpus > 4 ? ClaimId : "z"`, "ClaimId", "a conditional's branch"},
		{`{1, ClaimId}[1] == "x"`, "ClaimId", "inside a list literal, subscripted"},
		{`[a = ClaimId].a == "x"`, "ClaimId", "inside a record literal, selected"},
		{`eval("ClaimId") == "x"`, "ClaimId", "a constant eval names its target"},
		{`Cpus == 4 && ClaimId == "x"`, "ClaimId", "one side of a conjunction"},
	} {
		refs, _ := ConstraintRefs(tc.expr)
		found := false
		for _, r := range refs {
			if strings.EqualFold(r, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("ConstraintRefs(%q) = %v, missing %q (%s)", tc.expr, refs, tc.want, tc.why)
		}
		if attr, _ := PrivateConstraintRef(tc.expr); attr == "" {
			t.Errorf("PrivateConstraintRef(%q) found nothing private (%s)", tc.expr, tc.why)
		}
	}
}

// A dynamic reference cannot be resolved statically, so the walker must say so rather than report the
// expression clean -- a reference the caller cannot see is one it cannot check.
func TestConstraintRefsFlagsDynamicReferences(t *testing.T) {
	for _, expr := range []string{
		`eval(SomeAttr) == "x"`,
		`eval(strcat("Claim", "Id")) == "x"`,
		`eval("Cpus", "extra") == 4`,
	} {
		if _, dynamic := ConstraintRefs(expr); !dynamic {
			t.Errorf("ConstraintRefs(%q) did not flag a dynamic reference", expr)
		}
	}
	// A constant eval IS resolvable and must not be flagged, or every eval would be refused.
	if _, dynamic := ConstraintRefs(`eval("Cpus") == 4`); dynamic {
		t.Error(`eval("Cpus") was flagged dynamic; a constant target is exactly what can be checked`)
	}
}

func TestConstraintRefsPublicExpressionsAreClean(t *testing.T) {
	for _, expr := range []string{
		`Cpus == 4`,
		`Owner == "alice" && Memory > 1000`,
		`MY.Cpus == 4`,
		`ClaimIdentifier == "x"`, // a prefix of a private name, not one of them
		`Capabilities == "x"`,    // plural
		`TransferKeyring == "x"`,
	} {
		if attr, dynamic := PrivateConstraintRef(expr); attr != "" || dynamic {
			t.Errorf("PrivateConstraintRef(%q) = (%q, %v), want clean", expr, attr, dynamic)
		}
	}
}

// An unparsable constraint reports nothing rather than guessing: it is rejected downstream, where the
// parse error can be reported as itself.
func TestConstraintRefsUnparsable(t *testing.T) {
	for _, expr := range []string{``, `   `, `ClaimId ==`, `(((`} {
		refs, dynamic := ConstraintRefs(expr)
		if len(refs) != 0 || dynamic {
			t.Errorf("ConstraintRefs(%q) = (%v, %v), want empty", expr, refs, dynamic)
		}
	}
}
