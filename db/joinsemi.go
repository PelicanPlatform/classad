package db

import (
	"fmt"
	"strings"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A SEMI OR ANTI JOIN'S SIDE IS NOT IN THE ROW.
//
// Both kinds answer a question about a side rather than folding it in: `EXISTS` and `NOT
// EXISTS`. The row they emit has no member for that side, which the walk arranges by leaving
// it nil -- the same state the unmatched half of a LEFT join is in, spliced the same way.
//
// That makes two things true of every spec that uses one, and this file enforces both:
//
//   - Nothing downstream may read the side. A later step's ON term, or the residual WHERE,
//     naming it would read UNDEFINED and quietly stop holding, which is a wrong answer with
//     no symptom.
//   - Nothing of the side needs to be read but what its own condition tests. Its output
//     projection is empty by construction rather than by the caller's choice, so a caller
//     asking for attributes of it is asking for something no row can carry.

// refuseSemiSideRefs rejects a spec where something downstream of a semi or anti step reads
// the side that step tests.
//
// A term the alias compiler will not take whole is passed over rather than refused, which is
// the same line refuseUnscopedSides draws beside it and for the same reason: refusing on a
// reference that could not be seen would refuse valid plans, and such a term is evaluated
// against a composite of the row where an absent side reads undefined either way.
func refuseSemiSideRefs(sides []JoinSide, steps []JoinStep, post, aliases []string) error {
	for i := range steps {
		if steps[i].Kind.carriesSide() {
			continue
		}
		side := i + 1
		if side >= len(sides) {
			continue
		}
		what := "a semi join"
		if steps[i].Kind == JoinAnti {
			what = "an anti join"
		}
		if sides[side].Project != nil {
			return fmt.Errorf("%s contributes no attributes, so %s may not be projected "+
				"from; drop its Project", what, aliases[side])
		}
		if err := refuseSideRead("WHERE", post, aliases, side, what); err != nil {
			return err
		}
		for j := i + 1; j < len(steps); j++ {
			if err := refuseSideRead("ON", steps[j].Cond, aliases, side, what); err != nil {
				return err
			}
			for _, e := range steps[j].Equi {
				if err := refuseSideRead("join equality", []string{e.Left}, aliases,
					side, what); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// refuseSideRead rejects a term that reads one named side.
func refuseSideRead(what string, srcs, aliases []string, side int, kind string) error {
	for _, src := range srcs {
		if strings.TrimSpace(src) == "" {
			continue
		}
		q, err := vm.ParseAliases(src, aliases)
		if err != nil || !q.AliasNative() {
			continue
		}
		for _, r := range q.AliasRefs() {
			if r.Alias == side {
				return fmt.Errorf("%s term %q reads %s, which %s tests but does not "+
					"fold in; its attributes are in no row", what, src, aliases[side], kind)
			}
		}
	}
	return nil
}
