package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// TestFoldIsUndefinedLiterals checks that constant folding evaluates is/isnt (=?=/=!=)
// on literal operands -- including undefined, which the general numeric-fold path skips
// -- so undefined-guarded ifThenElse/elvis terms collapse. References are left intact
// (never assumed undefined), which keeps any downstream pushdown sound.
func TestFoldIsUndefinedLiterals(t *testing.T) {
	cases := []struct{ expr, want string }{
		{`undefined is undefined`, "true"},
		{`undefined isnt undefined`, "false"},
		{`5 is undefined`, "false"},
		{`"x" isnt undefined`, "true"},
		{`(undefined is undefined) || (undefined is undefined)`, "true"},
		{`ifThenElse((undefined is undefined) || (undefined is undefined), 0, 999)`, "0"},
		// A baked (literal) RequestDisk makes the whole modern-WithinResourceLimits disk
		// term collapse to the constant.
		{`4096 - ifThenElse(((undefined ?: "") != "Copy"), ifThenElse(((undefined is undefined) || (undefined is undefined)), 0, 999), 0)`, "4096"},
		// A reference is NOT assumed undefined -- it stays, so nothing unsound folds.
		{`SomeRef is undefined`, "(SomeRef is undefined)"},
		{`ifThenElse(SomeRef is undefined, 0, 999)`, `ifThenElse((SomeRef is undefined), 0, 999)`},
	}
	for _, tc := range cases {
		ex, err := ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		got := FoldConstants(ex.expr).(ast.Node).String()
		if got != tc.want {
			t.Errorf("fold %q => %q, want %q", tc.expr, got, tc.want)
		}
	}
}
