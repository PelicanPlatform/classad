package classad

import (
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/ast"
)

// CurrentTime is ClassAd's magic attribute for "now": an expression that references it
// evaluates to the current wall-clock time in Unix seconds (like the time() builtin),
// unless the ClassAd explicitly defines CurrentTime -- in which case the ad's value wins.
// This matches HTCondor, where CurrentTime is always available and history/queue
// expressions such as `CurrentTime - EnteredHistoryTime` rely on it.
//
// This is HTCondor's old ClassAd semantics (ClassAdReconfig -> SetOldClassAdSemantics), under
// which CurrentTime is one of the special names in libclassad's ClassAd::LookupInScope
// (src/classad/classad.cpp). A lookup of CurrentTime in an ad that does not define it
// yields time() and stops there: it does not continue to the enclosing scope, and since
// it succeeds, AttributeReference::FindExpr (src/classad/attrrefs.cpp) never consults the
// match target, which it does only when the lookup comes back undefined. So:
//
//   - an ad that defines CurrentTime supplies that value, undefined or an expression
//     included -- `[CurrentTime = undefined; r = CurrentTime].r` is undefined;
//   - unscoped or MY.CurrentTime in an ad without it is the time, in matchmaking too:
//     it never reads the match target's CurrentTime;
//   - TARGET.CurrentTime is the target's CurrentTime, or the time when the target lacks
//     it; with no target it is undefined;
//   - a select (`rec.CurrentTime`) and a reference inside a nested record look in that
//     record and get the time when it lacks CurrentTime, even if an enclosing ad has it.
//
// A chained parent (SetParent) is read as part of the ad, as libclassad's Lookup reads a
// chained parent ad; an enclosing scope reached through a select is not.
//
// The magic is provided in two places, which must agree:
//   - Evaluation: evaluateCurrentTime. An Evaluator can pin the time (PinCurrentTime), so
//     every evaluation of a compiled query sees one "now".
//   - Query planning: FoldConstantsAt folds CurrentTime to a given time, so a constraint
//     like `CompletionDate > CurrentTime - 86400` becomes an indexable range probe and can
//     prune whole segments/zones. That fold is exact only for an ad that does not define
//     CurrentTime, so FoldConstants, whose result must evaluate like its input in any
//     scope, leaves the reference alone.

// currentTimeAttr is the folded (lower-case) name of the magic attribute.
const currentTimeAttr = "currenttime"

// isCurrentTimeRef reports whether ref is the magic CurrentTime attribute: an unscoped or
// MY-scoped reference named "CurrentTime" (case-insensitive). TARGET/PARENT/absolute
// references are ordinary lookups, never the magic attribute.
func isCurrentTimeRef(ref *ast.AttributeReference) bool {
	if ref == nil {
		return false
	}
	switch ref.Scope {
	case ast.NoScope, ast.MyScope:
		return ref.NormalizedName() == currentTimeAttr
	default:
		return false
	}
}

// IsCurrentTimeName reports whether name is CurrentTime (case-insensitive), for a resolver
// (SetResolver) that must tell the magic attribute apart.
func IsCurrentTimeName(name string) bool {
	return len(name) == len(currentTimeAttr) && strings.EqualFold(name, currentTimeAttr)
}

// currentTimeExpr returns the CurrentTime that c defines, and the ad that defines it: c or
// one of its chained parents. An enclosing scope reached through a select is not searched.
// It returns a nil expression when none defines it.
func (c *ClassAd) currentTimeExpr() (*ClassAd, ast.Expr) {
	for ad := c; ad != nil; ad = ad.parent {
		if expr := ad.lookupNorm(currentTimeAttr); expr != nil {
			return ad, expr
		}
		if ad.lexicalParent {
			break
		}
	}
	return nil, nil
}

// currentTimeValue returns the current time as an integer Unix-seconds value, matching the
// time() builtin (NewIntValue(time.Now().Unix())).
func currentTimeValue() Value {
	return NewIntValue(time.Now().Unix())
}
