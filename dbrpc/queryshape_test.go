package dbrpc

import (
	"strconv"
	"strings"
	"testing"
)

// TestQueryShapeMerges lists the constraint pairs that MUST land in one bucket. Each
// pair is a way the same query reaches the server written two different ways.
func TestQueryShapeMerges(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{
			// The case the whole feature turns on: a rolling time window.
			"time literal",
			"CompletionDate >= 1700000000",
			"CompletionDate >= 1699000000",
		},
		{
			// ClassAd attribute names are case-insensitive, so these are one attribute.
			"attribute case",
			`owner == "alice"`,
			`Owner == "bob"`,
		},
		{
			// The engine constant-folds before planning: these are the same query.
			"constant folding",
			"Memory > 2048*1024",
			"Memory > 2097152",
		},
		{
			// Whitespace and parenthesization vary with whichever client composed it.
			"whitespace and parens",
			`JobStatus==2&&Owner=="alice"`,
			`(JobStatus == 2) && (Owner == "bob")`,
		},
		{
			"conjunct order",
			`JobStatus == 2 && Owner == "alice"`,
			`Owner == "bob" && JobStatus == 5`,
		},
		{
			// A membership test must not get a bucket per list length.
			"OR list length",
			`Owner == "a" || Owner == "b" || Owner == "c"`,
			`Owner == "x" || Owner == "y"`,
		},
		{
			"list literal length",
			`member(Owner, {"a", "b", "c"})`,
			`member(Owner, {"x"})`,
		},
		{
			"comparison direction",
			"500 < Memory",
			"Memory > 2000",
		},
		{
			"function name case",
			`RegExp("^a", Owner)`,
			`regexp("^b", Owner)`,
		},
		{
			"empty and true",
			"",
			"TRUE",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ga, gb := queryShape(c.a), queryShape(c.b)
			if ga != gb {
				t.Errorf("shapes differ:\n  %q -> %q\n  %q -> %q", c.a, ga, c.b, gb)
			}
		})
	}
}

// TestQueryShapeSeparates lists the pairs that MUST NOT merge: queries that differ in
// what they ask the store to do, not merely in how they were written.
func TestQueryShapeSeparates(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"different attribute", `Owner == "a"`, `User == "a"`},
		{"different operator", "CompletionDate >= 1700000000", "CompletionDate == 1700000000"},
		{"extra conjunct", `Owner == "a"`, `Owner == "a" && JobStatus == 2`},
		{"and vs or", `Owner == "a" && JobStatus == 2`, `Owner == "a" || JobStatus == 2`},
		{"probe vs residue", "ProcId >= 5", "ProcId >= ClusterId"},
		{"different function", `regexp("^a", Owner)`, `stringListMember(Owner, Owners)`},
		{"scoped vs unscoped", "TARGET.Memory > 500", "Memory > 500"},
		{"match-all vs predicate", "true", `Owner == "a"`},
		{"negation", `Owner == "a"`, `!(Owner == "a")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ga, gb := queryShape(c.a), queryShape(c.b)
			if ga == gb {
				t.Errorf("shapes collided: %q and %q both -> %q", c.a, c.b, ga)
			}
		})
	}
}

// TestQueryShapeDropsLiterals confirms no value survives into a fingerprint: a shape is
// returned to any peer that may read, so it must carry nothing about the data.
func TestQueryShapeDropsLiterals(t *testing.T) {
	const secretish = `Owner == "alice@cern.ch" && ClusterId == 918273 && Rate > 1.5`
	got := queryShape(secretish)
	for _, lit := range []string{"alice", "cern", "918273", "1.5"} {
		if strings.Contains(got, lit) {
			t.Errorf("shape %q leaked the literal %q", got, lit)
		}
	}
}

// TestQueryShapeMalformed buckets the constraints the server could not parse together,
// rather than letting each malformed string claim a shape of its own.
func TestQueryShapeMalformed(t *testing.T) {
	if got := queryShape("Owner =="); got != shapeUnparsed {
		t.Errorf("queryShape(bad) = %q, want %q", got, shapeUnparsed)
	}
	if queryShape("Owner ==") != queryShape("&& ||") {
		t.Error("two malformed constraints should share one bucket")
	}
}

// TestQueryShapeBounded caps a pathological constraint so the shape map cannot be grown
// by length as well as by count.
func TestQueryShapeBounded(t *testing.T) {
	var terms []string
	for i := 0; i < 400; i++ {
		terms = append(terms, "Attr"+string(rune('A'+i%26))+strconv.Itoa(i)+" == 1")
	}
	got := queryShape(strings.Join(terms, " && "))
	if len(got) > shapeMaxLen+len(shapeTruncated) {
		t.Errorf("shape length %d exceeds the cap %d", len(got), shapeMaxLen)
	}
}
