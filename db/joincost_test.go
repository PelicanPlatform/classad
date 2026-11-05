package db

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// costFixture is a history archive with the skew a real one has: a few owners hold most of
// the jobs, most jobs completed, memory requests cluster on a few round numbers. Uniform
// data would make every selectivity estimate right for the wrong reason.
func costFixture(tb testing.TB, n int, reindex bool) (*Catalog, *ArchiveTable) {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:      1 << 16,
		CategoricalAttrs: []string{"Owner", "JobStatus"},
		ValueAttrs:       []string{"RequestMemory", "ClusterId", "CompletionDate"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	owners := []string{"alice", "bob", "carol", "dave", "erin"}
	pick := func(cuts ...float64) int {
		r := rng.Float64()
		for i, c := range cuts {
			if r < c {
				return i
			}
		}
		return len(cuts)
	}
	mems := []int{1024, 2048, 4096, 8192, 16384}
	for i := 0; i < n; i++ {
		oi := pick(0.50, 0.75, 0.88, 0.95)
		mi := pick(0.55, 0.80, 0.93, 0.98)
		status := 4
		if rng.Float64() < 0.12 {
			status = 3
		}
		ad, err := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"%s\"\nCmd = \"/home/%s/run.sh\"\nCompletionDate = %d",
			i, i%10, status, mems[mi], 1+i%8, owners[oi], owners[oi], 1700000000+i))
		if err != nil {
			tb.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			tb.Fatal(err)
		}
	}
	if reindex {
		a.Reindex()
	}
	return cat, a
}

// costConstraints spans the access paths readVisits has to price: an index probe at several
// selectivities, an index union, a plan whose extra conjunct no index covers, and -- the
// ones that matter most -- constraints no index covers at all, which scan.
var costConstraints = []string{
	`Owner == "erin"`,
	`Owner == "alice"`,
	`RequestMemory >= 16384`,
	`RequestMemory >= 2048`,
	`Owner == "erin" && JobStatus == 3`,
	`Owner == "alice" && RequestMemory >= 8192`,
	`ClusterId < 1000`,
	`ClusterId >= 19000`,
	`ClusterId >= 100000`, // matches nothing
	`Owner == "erin" || Owner == "dave"`,
	`Owner == "alice" && RequestCpus >= 4`,
	`RequestCpus >= 4 && ClusterId < 1000`,
	`JobStatus == 3`,
	`JobStatus == 4`,
	`RequestCpus >= 4`,
	`Cmd == "/home/erin/run.sh"`,
}

// TestEstVisitedTracksTheRead is the cost model's foundation: every join cost is denominated
// in records touched, so an estimate that does not track what a read actually touches makes
// every decision built on it arbitrary.
//
// The bound is deliberately loose. The estimate is for ORDERING work and choosing an access
// path, never for a result, and the thing it has to be right about is the ORDER of the
// number. What a tolerance this wide still catches is the failure that matters: EstCandidates
// is zero for a plan that scans, so reading it off that one field prices the most expensive
// side a join can have at nothing.
func TestEstVisitedTracksTheRead(t *testing.T) {
	for _, reindex := range []bool{true, false} {
		t.Run(fmt.Sprintf("reindex=%v", reindex), func(t *testing.T) {
			cat, a := costFixture(t, 20000, reindex)
			defer cat.Close()
			total := a.Count()
			for _, c := range costConstraints {
				ex, err := a.Explain(c)
				if err != nil {
					t.Fatalf("%s: %v", c, err)
				}
				if ex.Plan == "" {
					t.Errorf("%s: no plan was reported", c)
					continue
				}
				est := int(ex.EstVisited)
				var st collections.ScanStats
				seq, err := a.QueryRawWireStats(c, nil, false, &st)
				if err != nil {
					t.Fatalf("%s: %v", c, err)
				}
				for range seq {
				}
				actual := st.RecordsVisited
				t.Logf("%-44s estVisited %6d  visited %6d  (%s)", c, est, actual, ex.Plan)
				// A ratio bound plus an absolute allowance. The allowance is for the
				// open segment, which a read walks in full and no explain reports (see
				// readVisits), so the shortfall is additive and swamps a ratio when the
				// read is small. One percent of the table is far above one open segment
				// here and far below the failure this is for: a scan priced at zero.
				slack := total / 100
				if lo, hi := actual/2-slack, actual*5/2+slack; est < lo || est > hi {
					t.Errorf("%s: priced at %d records, but the read visits %d (want %d..%d, plan %q)",
						c, est, actual, lo, hi, ex.Plan)
				}
			}
		})
	}
}

// A constraint no index covers reads every record. EstCandidates is 0 for that plan, so a
// cost model that trusted it would price a full scan as free -- and then narrow it, or
// choose it as the side to hash, on the strength of a number that means the opposite.
func TestEstVisitedPricesAScanAtTheWholeTable(t *testing.T) {
	cat, a := costFixture(t, 20000, true)
	defer cat.Close()
	total := a.Count()
	ex, err := a.Explain(`RequestCpus >= 4`) // no index covers RequestCpus
	if err != nil {
		t.Fatal(err)
	}
	if ex.EstCandidates != 0 {
		t.Fatalf("the fixture no longer produces the scan this is about: EstCandidates = %d", ex.EstCandidates)
	}
	est := int(ex.EstVisited)
	if est != total {
		t.Errorf("priced a full scan at %d records, want the whole table (%d)", est, total)
	}
}

// The error paths. Each returns a reason rather than a number, because the caller's next
// move is to decide something with whatever comes back, and a decision made on an invented
// size is indistinguishable in everything it reports from an informed one.
func TestSideRowsDeclinesWhatItCannotPrice(t *testing.T) {
	cat, a := costFixture(t, 1000, true)
	defer cat.Close()
	total := a.Count()

	for _, tc := range []struct {
		name string
		side JoinSide
		want string
	}{
		{"no such table", JoinSide{Table: "nope", Alias: "n", Constraint: `Owner == "x"`}, "could not be planned"},
		{"bad constraint", JoinSide{Table: "history", Alias: "h", Constraint: "Owner =="}, "could not be planned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, why := cat.sideRows(tc.side, total)
			if why == "" {
				t.Fatalf("priced it at %d records instead of declining", rows)
			}
			if !strings.Contains(why, tc.want) {
				t.Errorf("reason %q does not contain %q", why, tc.want)
			}
			if rows != 0 {
				t.Errorf("declined but returned %d rows", rows)
			}
		})
	}

	// A MUTABLE table prices the same way. It is a separate branch of explainSide and the
	// one build-side selection will lean on, so it is not left to the archive to cover.
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nOwner = \"user%d\"", i, i%5))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr = jobs.Put(fmt.Sprintf("%d.0", i), ad); perr != nil {
			t.Fatal(perr)
		}
	}
	side := JoinSide{Table: "jobs", Alias: "j", Constraint: `Owner == "user0"`}
	rows, why := cat.sideRows(side, jobs.Len())
	if why != "" {
		t.Fatalf("a mutable table could not be priced: %s", why)
	}
	if rows <= 0 || rows > jobs.Len() {
		t.Errorf("priced a mutable side at %d records, outside 1..%d", rows, jobs.Len())
	}

	// A side with no constraint of its own reads the whole table, and that needs no plan.
	if rows, why := cat.sideRows(JoinSide{Table: "history", Alias: "h"}, total); why != "" || rows != total {
		t.Errorf("an unconstrained side priced at (%d, %q), want (%d, \"\")", rows, why, total)
	}
}

func TestAndConstraints(t *testing.T) {
	for _, tc := range []struct{ own, cons, want string }{
		{"", "(a == 1)", "(a == 1)"},
		{"b == 2", "", "b == 2"},
		{"", "", ""},
		{"b == 2", "(a == 1)", "(b == 2) && (a == 1)"},
		// The side's own text is parenthesized because it may be a bare disjunction,
		// which would otherwise bind looser than the && and widen the side.
		{"b == 2 || c == 3", "(a == 1)", "(b == 2 || c == 3) && (a == 1)"},
	} {
		if got := andConstraints(tc.own, tc.cons); got != tc.want {
			t.Errorf("andConstraints(%q, %q) = %q, want %q", tc.own, tc.cons, got, tc.want)
		}
	}
}
