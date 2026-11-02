package vm

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkJoinCondition measures the two ways a join can evaluate its condition over a
// candidate PAIR, which is the unit that matters: the pair count is the product of the
// sides, not the sum, so this cost is multiplied by everything.
//
//   - "composite" is what a join does today: build a ClassAd holding both sides under their
//     aliases, then evaluate the ordinary compiled condition against it.
//   - "composite-reused" is that with the row allocated once and its members swapped per
//     candidate -- the allocation already removed from the client-side join, included here
//     so the aliased arm is not credited with a saving that was already banked.
//   - "aliased" resolves each side directly, building nothing.
//
// width is the attribute count per side, because the composite arms' cost should depend on
// it (an ad is built over the sides) while the aliased arm's should not (it reads the two
// attributes the condition names and never touches the rest).
func BenchmarkJoinCondition(b *testing.B) {
	const cond = `j.Owner == h.Owner && j.ClusterId > h.ClusterId`
	for _, width := range []int{4, 16, 48} {
		left, right := benchSide("alice", 42, width), benchSide("alice", 7, width)

		plain, err := Parse(cond)
		if err != nil {
			b.Fatal(err)
		}
		aliased, err := ParseAliases(cond, []string{"j", "h"})
		if err != nil {
			b.Fatal(err)
		}
		if !aliased.AliasNative() {
			b.Fatal("fixture condition is not AliasNative; the aliased arm would decline")
		}

		b.Run(fmt.Sprintf("width=%d/composite", width), func(b *testing.B) {
			m := plain.Matcher()
			for i := 0; i < b.N; i++ {
				row := classad.New()
				row.InsertAttrClassAd("j", left)
				row.InsertAttrClassAd("h", right)
				if v := m.Eval(row); !isTrueBench(v) {
					b.Fatal("fixture pair does not match")
				}
			}
		})

		b.Run(fmt.Sprintf("width=%d/composite-reused", width), func(b *testing.B) {
			m := plain.Matcher()
			row := classad.New()
			row.InsertAttrClassAd("j", left)
			for i := 0; i < b.N; i++ {
				row.InsertAttrClassAd("h", right)
				if v := m.Eval(row); !isTrueBench(v) {
					b.Fatal("fixture pair does not match")
				}
			}
		})

		b.Run(fmt.Sprintf("width=%d/aliased", width), func(b *testing.B) {
			m := aliased.Matcher()
			resolve := func(alias int, name string) classad.Value {
				if alias == 0 {
					return left.EvaluateAttr(name)
				}
				return right.EvaluateAttr(name)
			}
			for i := 0; i < b.N; i++ {
				v, ok := m.EvalAliased(resolve, nil)
				if !ok || !isTrueBench(v) {
					b.Fatal("fixture pair does not match")
				}
			}
		})
	}
}

func benchSide(owner string, cluster int64, width int) *classad.ClassAd {
	ad := classad.New()
	ad.InsertAttrString("Owner", owner)
	ad.InsertAttr("ClusterId", cluster)
	for i := 0; i < width-2; i++ {
		ad.InsertAttrString(fmt.Sprintf("Pad%02d", i), "padding value")
	}
	return ad
}

func isTrueBench(v classad.Value) bool {
	b, err := v.BoolValue()
	return err == nil && b
}
