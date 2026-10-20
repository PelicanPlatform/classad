package classad

import (
	"time"

	"github.com/PelicanPlatform/classad/ast"
)

// CurrentTime is ClassAd's magic attribute for "now": an expression that references it
// evaluates to the current wall-clock time in Unix seconds (like the time() builtin),
// unless the ClassAd explicitly defines CurrentTime -- in which case the ad's value wins.
// This matches HTCondor, where CurrentTime is always available and history/queue
// expressions such as `CurrentTime - EnteredHistoryTime` rely on it.
//
// The magic is provided in two places, which must agree:
//   - Evaluation: evaluateAttributeReference returns the current time when an unscoped (or
//     MY-scoped) CurrentTime reference is otherwise undefined. An Evaluator can pin that
//     time (PinCurrentTime), so every evaluation of a compiled query sees one "now".
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

// currentTimeValue returns the current time as an integer Unix-seconds value, matching the
// time() builtin (NewIntValue(time.Now().Unix())).
func currentTimeValue() Value {
	return NewIntValue(time.Now().Unix())
}
