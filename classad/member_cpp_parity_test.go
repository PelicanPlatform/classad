package classad

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestMemberCppParity pins member() and identicalMember() to the reference engine, over the cross
// product of first and second arguments in testdata/member_cpp.tsv -- each line's expected value is
// what libclassad returned for it.
//
// The rules the table holds the evaluator to, which the planner and the compiled forms of member
// rely on: member is UNDEFINED for an UNDEFINED first argument (or list), ERROR for an ERROR, list
// or ad first argument (or a non-list second), and otherwise TRUE exactly when some element compares
// == TRUE -- an element whose comparison is UNDEFINED or ERROR is skipped, so the list's contents
// never make the result UNDEFINED or ERROR. == coerces int/real/bool numerically and folds ASCII
// case. identicalMember uses =?= instead: no coercion, case-sensitive, and an UNDEFINED or ERROR
// first argument is looked for like any other value.
func TestMemberCppParity(t *testing.T) {
	f, err := os.Open("testdata/member_cpp.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		want, expr, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed line %q", line)
		}
		ad, err := Parse(`[ A = 5; R = 5.0; B = true; S = "abc"; L = {1, 5}; N = [a = 5]; E = error; U = undefined; x = ` + expr + ` ]`)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		if got := memberTag(ad.EvaluateAttr("x")); got != want {
			t.Errorf("%s = %s, reference engine gives %s", expr, got, want)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 2000 {
		t.Fatalf("only %d cases read; the table is truncated", n)
	}
}

func memberTag(v Value) string {
	switch {
	case v.IsUndefined():
		return "U"
	case v.IsError():
		return "E"
	case v.IsBool():
		if b, _ := v.BoolValue(); b {
			return "B:true"
		}
		return "B:false"
	}
	return "?:" + v.String()
}
