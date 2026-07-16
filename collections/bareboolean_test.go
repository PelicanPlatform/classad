package collections

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestBareBooleanCategoricalIndexed: a bare boolean attribute used as a truthiness
// constraint (`HAS_CVMFS`, `!HAS_CVMFS`) is answered by its categorical index, and the
// indexed result equals a full scan even when some ads carry the attribute as a number
// or an expression (which the exception-set superset + re-verify must handle).
func TestBareBooleanCategoricalIndexed(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"categorical", "value"} {
		t.Run(kind, func(t *testing.T) { testBareBoolean(t, kind) })
	}
}

func testBareBoolean(t *testing.T, kind string) {
	build := func(indexed bool) *Collection {
		opts := Options{Shards: 4}
		if indexed {
			if kind == "categorical" {
				opts.CategoricalAttrs = []string{"Flag"}
			} else {
				opts.ValueAttrs = []string{"Flag"}
			}
		}
		c := New(opts)
		for i := 0; i < 5000; i++ {
			var text string
			switch {
			case i%500 == 0: // numeric: truthy iff non-zero
				text = fmt.Sprintf(`[ Id=%d; Flag = %d ]`, i, i%3)
			case i%501 == 0: // expression -> exception set
				text = fmt.Sprintf(`[ Id=%d; Flag = Base > 0 ]`, i)
			case i%7 == 0: // absent
				text = fmt.Sprintf(`[ Id=%d ]`, i)
			default:
				v := "true"
				if i%100 < 29 {
					v = "false"
				}
				text = fmt.Sprintf(`[ Id=%d; Flag = %s ]`, i, v)
			}
			c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, text))
		}
		if indexed {
			c.Reindex()
		}
		return c
	}
	plain, idx := build(false), build(true)

	// The bare-boolean constraint is planned as an indexed probe.
	q, _ := vm.Parse(`Flag`)
	if ex := idx.ExplainQuery(q); ex.Plan != "indexed" || ex.IndexUsable != 1 {
		t.Fatalf("bare `Flag` plan=%s usable=%d, want indexed/1", ex.Plan, ex.IndexUsable)
	}

	ids := func(c *Collection, qs string) []int {
		qq, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for ad := range c.Query(qq) {
			id, _ := ad.EvaluateAttrInt("Id")
			out = append(out, int(id))
		}
		sort.Ints(out)
		return out
	}
	for _, qs := range []string{`Flag`, `!Flag`, `Flag && Id < 1000`} {
		got, want := ids(idx, qs), ids(plain, qs)
		if !equalInts(got, want) {
			t.Errorf("query %q: indexed %d != full scan %d", qs, len(got), len(want))
		}
		if len(want) == 0 || len(want) == 5000 {
			t.Errorf("query %q: expected a partial set, got %d", qs, len(want))
		}
	}
}
