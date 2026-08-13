package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// Redaction covered the values an unprivileged connection is SERVED. It did nothing about what a
// constraint could READ, and evaluation has no notion of privacy -- so a read-only client could learn a
// claim capability from whether a row came back. These vectors all worked before the gate:
//
//	ClaimId == "guess"                 an equality oracle
//	regexp("^abc", ClaimId)            a prefix oracle: linear in the secret's length, not exponential
//	substr(ClaimId,0,6) == "secret"    likewise
//	size(ClaimId) == 13                its length
//	Capability =!= undefined           its existence
//	MY.ClaimId == "guess"              and this one evaded a ReadAttrs-based check entirely
//	count(*) where ClaimId == "guess"  the same oracle through the aggregate
//
// Nothing here asserts an error MESSAGE; each asserts that the request is refused rather than answered,
// because "answered" is the leak.

const privateSecret = "secret-abc123"

func privateGatePair(t *testing.T, readOnly bool) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	tx.NewClassAdOld("j1", "Cpus = 4\nJobStatus = 2\nClaimId = \""+privateSecret+"\"\nCapability = \"cap-xyz\"")
	tx.NewClassAdOld("j2", "Cpus = 8\nJobStatus = 4\nClaimId = \"other-def456\"")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	opts := ServeOptions{ReadOnly: readOnly}
	if !readOnly {
		opts = ServeOptions{IncludePrivate: true}
	}
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

// privateConstraints are the expressions an unprivileged connection must not be allowed to evaluate.
var privateConstraints = []string{
	`ClaimId == "` + privateSecret + `"`,
	`regexp("^secret", ClaimId)`,
	`substr(ClaimId,0,6) == "secret"`,
	`size(ClaimId) == 13`,
	`Capability =!= undefined`,
	`MY.ClaimId == "` + privateSecret + `"`,
	`TARGET.ClaimId == "` + privateSecret + `"`,
	`eval("ClaimId") == "` + privateSecret + `"`,
	`Cpus == 4 && ClaimId == "` + privateSecret + `"`,
	`Cpus == 4 ? ClaimId : "z"`,
	`{1, ClaimId}[1] == "` + privateSecret + `"`,
	`(ClaimId ?: "z") == "` + privateSecret + `"`,
}

// TestPrivateConstraintRefusedEveryReadOp walks every read op that accepts a constraint. A new op that
// forgets the gate reopens the oracle, and the only thing that catches that is enumerating them here.
func TestPrivateConstraintRefusedEveryReadOp(t *testing.T) {
	c, cleanup := privateGatePair(t, true)
	defer cleanup()
	ctx := context.Background()

	// Each entry runs one op with the given constraint and reports whether the server refused.
	ops := []struct {
		name    string
		run     func(constraint string) error
		answers bool // true if this op returns data we must not have received
	}{
		{"Query", func(q string) error { _, err := c.Query(ctx, q); return err }, true},
		{"QueryLimit", func(q string) error { _, err := c.QueryLimit(ctx, q, 10); return err }, true},
		{"QueryRaw", func(q string) error { _, err := c.QueryRaw(ctx, q); return err }, true},
		{"Aggregate/count", func(q string) error {
			_, err := c.Aggregate(ctx, q, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
			return err
		}, true},
		{"Aggregate/grouped", func(q string) error {
			_, err := c.Aggregate(ctx, q, []string{"JobStatus"}, []AggSpec{{Func: AggCount, Arg: "*"}})
			return err
		}, true},
		{"AggregateBucketed", func(q string) error {
			_, err := c.AggregateBucketed(ctx, q, []GroupCol{{Attr: "JobStatus"}},
				[]AggSpec{{Func: AggCount, Arg: "*"}})
			return err
		}, true},
		{"Explain", func(q string) error { _, err := c.Explain(ctx, q); return err }, true},
	}
	for _, op := range ops {
		for _, cst := range privateConstraints {
			err := op.run(cst)
			if err == nil {
				t.Errorf("%s(%q): answered; an unprivileged connection must not evaluate a private "+
					"attribute reference", op.name, cst)
				continue
			}
			if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "dynamic") {
				t.Errorf("%s(%q): refused for the wrong reason: %v", op.name, cst, err)
			}
		}
	}
}

// TestPublicConstraintsStillWork is the other half: the gate must not break ordinary queries, and it must
// not fire on an attribute that merely looks private-adjacent.
func TestPublicConstraintsStillWork(t *testing.T) {
	c, cleanup := privateGatePair(t, true)
	defer cleanup()
	ctx := context.Background()
	for _, cst := range []string{
		`Cpus == 4`,
		`Cpus > 2 && JobStatus == 2`,
		`MY.Cpus == 4`,
		`regexp("^4$", string(Cpus))`,
		`eval("Cpus") == 4`,                  // a constant eval of a PUBLIC attribute stays allowed
		`ClaimIdentifier == "not-private"`,   // not in the private set: a prefix of a private name
		`TransferKeyring == "not-private"`,   // ditto
		`Capabilities == "not-private-name"`, // plural: not the private "Capability"
	} {
		if _, err := c.Query(ctx, cst); err != nil {
			t.Errorf("public constraint %q was refused: %v", cst, err)
		}
	}
}

// TestPrivilegedConstraintsAllowed checks the gate is about the CONNECTION, not the data: a connection
// authorized for private attributes may still ask.
func TestPrivilegedConstraintsAllowed(t *testing.T) {
	c, cleanup := privateGatePair(t, false)
	defer cleanup()
	ctx := context.Background()
	rows, err := c.Query(ctx, `ClaimId == "`+privateSecret+`"`)
	if err != nil {
		t.Fatalf("privileged connection refused a private constraint: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("privileged Query matched %d rows, want 1", len(rows))
	}
	if !strings.Contains(rows[0], privateSecret) {
		t.Errorf("privileged connection did not see the secret: %q", rows[0])
	}
}

// TestPrivateFilterScopedRefRefused covers the per-aggregate FILTER gate specifically. It existed before,
// but was built on ReadAttrs, which reports nothing for a scoped reference -- so a filter of
// `MY.ClaimId == …` passed it.
func TestPrivateFilterScopedRefRefused(t *testing.T) {
	c, cleanup := privateGatePair(t, true)
	defer cleanup()
	ctx := context.Background()
	for _, filter := range []string{
		`ClaimId == "` + privateSecret + `"`,
		`MY.ClaimId == "` + privateSecret + `"`,
		`eval("ClaimId") == "` + privateSecret + `"`,
	} {
		_, err := c.Aggregate(ctx, "true", []string{"JobStatus"},
			[]AggSpec{{Func: AggCount, Arg: "*", Filter: filter}})
		if err == nil {
			t.Errorf("FILTER %q: answered; a filtered count leaks the predicate", filter)
		}
	}
}

// TestPrivateConstraintGateIsStatic documents what the gate does NOT cover, so nobody mistakes it for
// complete. An ad may store a PUBLIC attribute whose value is an expression over a private one; a
// constraint on that public name references nothing private, so no static check can see it. Closing it
// needs the private value to be absent at evaluation time for an unprivileged reader.
func TestPrivateConstraintGateIsStatic(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	tx.NewClassAdOld("j1", "Cpus = 4\nClaimId = \""+privateSecret+"\"\nLeak = ClaimId")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	// The alias is not a private name, so the gate lets it through by design.
	if attr, _ := db.PrivateConstraintRef(`Leak == "x"`); attr != "" {
		t.Fatalf("the walker reported %q for a constraint that names no private attribute", attr)
	}
	rows, err := c.Query(context.Background(), `Leak == "`+privateSecret+`"`)
	if err != nil {
		t.Logf("laundered constraint refused: %v (the residue this documents is closed)", err)
		return
	}
	t.Logf("KNOWN RESIDUE: a constraint on a public alias of a private attribute still matches "+
		"(%d row(s)). A static check cannot see it; evaluation-time redaction is what closes it.", len(rows))
	// The rendered ad must still not contain the secret -- redaction is what holds here.
	for _, r := range rows {
		if strings.Contains(r, privateSecret) {
			t.Errorf("the secret VALUE leaked into a rendered ad: %q", r)
		}
	}
}
