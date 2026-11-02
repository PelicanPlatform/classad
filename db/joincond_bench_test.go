package db

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkJoinCondPath measures the whole join, not just the condition, so the condition
// saving is seen at the scale it actually runs at: once per candidate PAIR.
//
// Two shapes, because they weight the pair loop differently:
//
//   - "equi" pairs on an equality, so the hash does the work and few pairs are formed. The
//     condition is a small part of the total and this arm is the check that nothing regressed.
//   - "band" has no equality at all, so every left row is tried against every right row and
//     the pair loop IS the join. This is where the condition cost shows.
//
// width is the attributes per stored ad: the composite path builds a row over the sides, so
// it should cost more as the ads widen, and the aliased path should not.
func BenchmarkJoinCondPath(b *testing.B) {
	for _, shape := range []string{"equi", "band"} {
		for _, width := range []int{4, 24} {
			n := 400
			if shape == "band" {
				n = 150 // the band is quadratic; keep the pair count comparable
			}
			cat := benchJoinFixture(b, n, width)
			spec := JoinSpec{
				Kind:  JoinInner,
				Left:  JoinSide{Table: "jobs", Alias: "j"},
				Right: JoinSide{Table: "hist", Alias: "h"},
			}
			if shape == "equi" {
				spec.Cond = []string{`j.ClusterId == h.ClusterId`, `j.Cpus >= h.ExitCode`}
				spec.Equi = []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}}
			} else {
				spec.Cond = []string{`j.Cpus > h.ExitCode`}
			}

			for _, arm := range []string{"composite", "aliased"} {
				b.Run(fmt.Sprintf("%s/width=%d/%s", shape, width, arm), func(b *testing.B) {
					forceCompositeCond = arm == "composite"
					defer func() { forceCompositeCond = false }()
					var rows int
					for i := 0; i < b.N; i++ {
						var st JoinStats
						seq, err := cat.Join(spec, JoinLimits{}, &st)
						if err != nil {
							b.Fatal(err)
						}
						rows = 0
						for range seq {
							rows++
						}
					}
					if rows == 0 {
						b.Fatal("the join produced no rows; the arm is measuring rejection only")
					}
					b.ReportMetric(float64(rows), "rows")
				})
			}
		}
	}
}

func benchJoinFixture(b *testing.B, n, width int) *Catalog {
	b.Helper()
	cat, err := OpenCatalog(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { cat.Close() })
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		b.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		b.Fatal(err)
	}
	pad := ""
	for i := 0; i < width-3; i++ {
		pad += fmt.Sprintf("; Pad%02d = \"padding value\"", i)
	}
	for i := 0; i < n; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; Owner = "alice"; Cpus = %d%s ]`, i, i%16, pad))
		if perr != nil {
			b.Fatal(perr)
		}
		if werr := jobs.Put(fmt.Sprintf("j%d", i), ad); werr != nil {
			b.Fatal(werr)
		}
		had, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; ExitCode = %d%s ]`, i, i%8, pad))
		if perr != nil {
			b.Fatal(perr)
		}
		if werr := hist.Put(fmt.Sprintf("h%d", i), had); werr != nil {
			b.Fatal(werr)
		}
	}
	return cat
}
