package dbrpc

import (
	"github.com/PelicanPlatform/classad/db"
)

// Refusing a constraint that reads a secret.
//
// Redaction protects the VALUES an unprivileged connection is served; it does nothing about what a
// constraint may READ. Evaluation has no notion of privacy, so before this a read-only client could ask
// `ClaimId == "guess"` and learn the answer from whether the row came back -- and with `regexp("^abc",
// ClaimId)` or `substr(ClaimId,0,6) == "secret"` recover a claim capability in a number of queries linear
// in its length, rather than exponential. `count(*) where ClaimId == …` did the same through the
// aggregate, whose gate checked group columns, aggregate arguments and per-aggregate filters but never
// the WHERE clause.
//
// So an unprivileged connection may not reference a private attribute in a constraint at all: it is an
// error, not an empty result. An error says what happened, and matching-nothing would still be a signal
// if it were ever distinguishable from a genuine no-match.
//
// The check runs once per query, on the parsed expression, and costs nothing per record: a public
// constraint takes exactly the path it took before.

// refusePrivateConstraint reports whether a constraint must be refused for this connection, writing the
// error frame if so. A privileged (IncludePrivate) connection is never refused.
//
// It refuses two things: a reference to a private attribute anywhere in the expression, at any scope; and
// an expression that names attributes dynamically (eval of a non-constant), because a reference this
// cannot see is a reference it cannot check. eval("ClaimId") IS seen -- the constant is checked as if it
// had been written as a reference.
func refusePrivateConstraint(reqID uint64, constraint string, includePrivate bool, write func([]byte)) bool {
	if includePrivate {
		return false
	}
	attr, dynamic := db.PrivateConstraintRef(constraint)
	if attr != "" {
		write(respErr(reqID, "cannot reference private attribute "+attr+" in a constraint"))
		return true
	}
	if dynamic {
		write(respErr(reqID, "cannot use a dynamic attribute reference in a constraint: "+
			"its target cannot be checked against the private-attribute rules"))
		return true
	}
	return false
}
