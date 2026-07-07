package collections

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A corpus spanning the coercion-sensitive cases: string values in mixed case,
// numeric values as int/real/bool, expression-valued and absent indexed attrs.
var (
	fuzzOnce sync.Once
	fuzzColl *Collection
	fuzzSrc  map[int]*classad.ClassAd
)

func fuzzCorpus() (*Collection, map[int]*classad.ClassAd) {
	fuzzOnce.Do(func() {
		c := New(Options{
			Shards:           4,
			CategoricalAttrs: []string{"Arch", "State"},
			ValueAttrs:       []string{"Cpus", "Load"},
		})
		src := map[int]*classad.ClassAd{}
		// Arch mixes case; Cpus/Load mix int/real/bool; some exceptions/absents.
		arch := []string{"X86_64", "x86_64", "AArch64", "aarch64", "ppc64le"}
		cpus := []string{"1", "2", "3", "4", "1.0", "2.5", "true", "false"}
		load := []string{"0.5", "1", "2.75", "0", "3.0"}
		state := []string{"Unclaimed", "unclaimed", "Claimed", "Idle"}
		i := 0
		add := func(text string) {
			ad, err := classad.Parse(text)
			if err != nil {
				panic(fmt.Sprintf("parse %q: %v", text, err))
			}
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
				panic(err)
			}
			src[i] = ad
			i++
		}
		for _, a := range arch {
			for _, cp := range cpus {
				for _, ld := range load {
					add(fmt.Sprintf(`[ ID=%d; Arch="%s"; State="%s"; Cpus=%s; Load=%s ]`,
						i, a, state[i%len(state)], cp, ld))
				}
			}
		}
		// Exceptions and absents.
		add(fmt.Sprintf(`[ ID=%d; Arch="X86_64"; Cpus=Base+1; Load="high" ]`, i)) // expr + wrong-type
		add(fmt.Sprintf(`[ ID=%d; State="Claimed"; Cpus=4 ]`, i))                 // Arch/Load absent
		add(fmt.Sprintf(`[ ID=%d; Arch=42; State=7; Cpus=3; Load=1.5 ]`, i))      // wrong-type cat
		c.Reindex()
		fuzzColl, fuzzSrc = c, src
	})
	return fuzzColl, fuzzSrc
}

func sortedIDs(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// FuzzIndexMatchesFullScan asserts, for a fuzzed query over a fixed diverse
// corpus, that the index path returns exactly the brute-force match set. It is the
// backstop for value coercions (int/real/bool -> float64, ClassAd's coercing == and
// range) and the negation/operand-flip transforms.
func FuzzIndexMatchesFullScan(f *testing.F) {
	for _, s := range []string{
		`Arch == "x86_64"`, `Arch != "aarch64"`, `State == "unclaimed"`,
		`Cpus == 2`, `Cpus == 2.0`, `Cpus == true`, `Cpus >= 2`, `Cpus < 3`,
		`Load > 1.0`, `Load <= 2.75`, `Load != 0`,
		`Arch == "X86_64" || Arch == "aarch64"`,
		`Cpus >= 2 && Arch == "x86_64"`, `!(State == "Idle")`,
		`Load > 1 && Cpus <= 2 && Arch != "ppc64le"`, `Cpus == 1+1`,
	} {
		f.Add(s)
	}
	c, src := fuzzCorpus()
	f.Fuzz(func(t *testing.T, qs string) {
		q, err := vm.Parse(qs)
		if err != nil {
			return
		}
		got := map[int]bool{}
		for ad := range c.Query(q) {
			v, ok := ad.EvaluateAttrInt("ID")
			if !ok {
				t.Fatalf("yielded ad without ID for %q", qs)
			}
			id := int(v)
			if got[id] {
				t.Fatalf("query %q yielded ID %d twice", qs, id)
			}
			got[id] = true
		}
		want := map[int]bool{}
		for id, ad := range src {
			if q.Matches(ad) {
				want[id] = true
			}
		}
		if len(got) != len(want) {
			t.Fatalf("query %q: index %d matches, brute %d\n got=%v\n want=%v",
				qs, len(got), len(want), sortedIDs(got), sortedIDs(want))
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("query %q: index missed ID %d (brute matched)", qs, id)
			}
		}
	})
}
