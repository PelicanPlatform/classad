package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// evalProfileFixture is a collection whose records hold the shape this measurement exists for:
// one attribute written as an expression over another in every record (Mem), and a literal of
// the same kind beside it (Rss) so a difference between the two is about the expression.
//
// extra is appended to every record verbatim, which is how the privacy case adds a private
// attribute and a reference to it without a second fixture.
func evalProfileFixture(t testing.TB, n int, extra string) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := range n {
		text := fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; Rss=%d; Mem=(Rss+1023)/1024; Cmd="/bin/x"%s ]`,
			i, i%10, i%7, 1+i%6, 1024*(1+i%37), extra)
		ad, perr := classad.Parse(text)
		if perr != nil {
			t.Fatal(perr)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func profileNamed(t *testing.T, profs []AttrTypeProfile, name string) AttrTypeProfile {
	t.Helper()
	for _, p := range profs {
		if strings.EqualFold(p.Name, name) {
			return p
		}
	}
	t.Fatalf("no profile for %s in %d profiles", name, len(profs))
	return AttrTypeProfile{}
}

func classTotal(cs map[string]int) int {
	n := 0
	for _, v := range cs {
		n += v
	}
	return n
}

// TestEvaluatedProfileTypesAnExpressionAttribute is the whole point of the option: an attribute
// stored as an expression in every record has no declarable type from the stored classes alone,
// and a uniform integer one from the evaluated ones.
//
// The literal control is the other half of it. Rss is the same kind in the same records, and its
// two measurements must agree exactly -- if they do not, the evaluated pass is measuring
// something other than the same cells.
func TestEvaluatedProfileTypesAnExpressionAttribute(t *testing.T) {
	c := evalProfileFixture(t, 2000, "")
	profs, sampled := c.AttrTypeProfilesWith(AttrTypeOptions{SampleMax: 2000, Evaluate: true})
	if sampled == 0 {
		t.Fatal("nothing was sampled")
	}

	mem := profileNamed(t, profs, "Mem")
	if mem.Dominant != "expr" || len(mem.ValueClasses()) != 1 {
		t.Fatalf("Mem stored classes are %v; the fixture must store it as an expression in every record", mem.Classes)
	}
	if mem.Evaluated == nil {
		t.Fatal("Mem has no evaluated counts although Evaluate was asked for")
	}
	if !mem.Evaluated.Uniform() || mem.Evaluated.Dominant != "int" {
		t.Fatalf("Mem evaluates to %v; it is (Rss+1023)/1024 in every record, so it is uniformly an integer",
			mem.Evaluated.Classes)
	}
	// Rss runs 1024..1024*37, so Mem = (Rss+1023)/1024 runs 1..37 with integer division.
	if mem.Evaluated.IntMin != 1 || mem.Evaluated.IntMax != 37 {
		t.Fatalf("Mem evaluated bounds are [%d,%d], want [1,37]", mem.Evaluated.IntMin, mem.Evaluated.IntMax)
	}

	rss := profileNamed(t, profs, "Rss")
	if rss.Evaluated == nil {
		t.Fatal("Rss has no evaluated counts")
	}
	if rss.Evaluated.Ints != rss.Ints || rss.Evaluated.IntMin != rss.IntMin || rss.Evaluated.IntMax != rss.IntMax {
		t.Fatalf("the literal control disagrees with itself: stored %d ints in [%d,%d], evaluated %d in [%d,%d]",
			rss.Ints, rss.IntMin, rss.IntMax, rss.Evaluated.Ints, rss.Evaluated.IntMin, rss.Evaluated.IntMax)
	}
}

// TestEvaluatedCountsCoverEveryCell pins the invariant a consumer divides by: the evaluated
// classes account for exactly the cells the stored ones do. A pass that quietly skipped the
// cells it could not evaluate would leave a profile whose shares sum to less than Present, and
// every rate computed from it would be wrong in a direction nothing reports.
func TestEvaluatedCountsCoverEveryCell(t *testing.T) {
	c := evalProfileFixture(t, 1000, "")
	profs, _ := c.AttrTypeProfilesWith(AttrTypeOptions{SampleMax: 1000, Evaluate: true})
	if len(profs) == 0 {
		t.Fatal("no profiles")
	}
	for _, p := range profs {
		if p.Evaluated == nil {
			t.Fatalf("%s has no evaluated counts", p.Name)
		}
		if got := classTotal(p.Evaluated.Classes); got != p.Present {
			t.Fatalf("%s: evaluated classes sum to %d, present in %d records", p.Name, got, p.Present)
		}
		if got := classTotal(p.Classes); got != p.Present {
			t.Fatalf("%s: stored classes sum to %d, present in %d records", p.Name, got, p.Present)
		}
	}
}

// TestEvaluatedProfileWithholdsPrivateReferences is the privacy rule, and the attack it stops is
// a quiet one: the filter that keeps private attributes out of a served report works by NAME, so
// an ordinary attribute whose expression reads a private one would carry its bounds straight
// through a filter with no reason to suspect it.
//
// Derived = ClaimId * 2 over a numeric ClaimId is that shape. The evaluated profile for Derived
// must contain no integer bounds at all; it is counted as the error or undefined the missing
// reference produces.
func TestEvaluatedProfileWithholdsPrivateReferences(t *testing.T) {
	c := evalProfileFixture(t, 500, `; ClaimId=4242; Derived=ClaimId*2`)
	profs, _ := c.AttrTypeProfilesWith(AttrTypeOptions{SampleMax: 500, Evaluate: true})

	claim := profileNamed(t, profs, "ClaimId")
	if claim.Ints == 0 {
		t.Fatalf("the fixture must store ClaimId as a plain integer; stored classes are %v", claim.Classes)
	}

	d := profileNamed(t, profs, "Derived")
	if d.Evaluated == nil {
		t.Fatal("Derived has no evaluated counts")
	}
	if d.Evaluated.Ints != 0 {
		t.Fatalf("Derived evaluated to %d integers in [%d,%d]: an expression over a private attribute "+
			"carried its bounds into a profile that is not filtered by name",
			d.Evaluated.Ints, d.Evaluated.IntMin, d.Evaluated.IntMax)
	}
	if len(d.Evaluated.ValueClasses()) != 0 {
		t.Fatalf("Derived evaluated to %v, want nothing that carries a type", d.Evaluated.Classes)
	}
}

// TestEvaluatedProfileIsNotTakenUnlessAsked keeps the expensive measurement off the default
// path: every existing caller of AttrTypeProfiles pays for the stored classes only.
func TestEvaluatedProfileIsNotTakenUnlessAsked(t *testing.T) {
	c := evalProfileFixture(t, 200, "")
	profs, _ := c.AttrTypeProfiles(200)
	if len(profs) == 0 {
		t.Fatal("no profiles")
	}
	for _, p := range profs {
		if p.Evaluated != nil {
			t.Fatalf("%s carries evaluated counts although they were not asked for", p.Name)
		}
	}
}

// BenchmarkAttrTypeProfiles measures what the evaluated option costs, which is the number that
// decides whether a caller can simply always ask for it. The ad width matters here for the same
// reason it matters to a read: evaluating is one decode of the WHOLE record, so its cost scales
// with attributes the measurement never evaluates.
func BenchmarkAttrTypeProfiles(b *testing.B) {
	for _, wide := range []bool{false, true} {
		name := "narrow"
		extra := ""
		if wide {
			name = "wide"
			var sb strings.Builder
			for f := range 38 {
				if f%3 == 0 {
					fmt.Fprintf(&sb, `; Pad%d="v%d"`, f, f)
				} else {
					fmt.Fprintf(&sb, `; Pad%d=%d`, f, f*7)
				}
			}
			extra = sb.String()
		}
		b.Run(name, func(b *testing.B) {
			c := evalProfileFixture(b, 20000, extra)
			const sample = 2000
			for _, eval := range []bool{false, true} {
				arm := "stored"
				if eval {
					arm = "evaluated"
				}
				b.Run(arm, func(b *testing.B) {
					b.ReportAllocs()
					var sampled int
					for b.Loop() {
						_, sampled = c.AttrTypeProfilesWith(AttrTypeOptions{SampleMax: sample, Evaluate: eval})
					}
					if sampled == 0 {
						b.Fatal("nothing was sampled")
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*sampled), "ns/record")
				})
			}
		})
	}
}
