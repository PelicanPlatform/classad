package collections

import "testing"

// TestClosureKeepsTheColumnarPathForLiterals pins the condition that makes the chasing wire
// scan affordable: a projection of LITERALS must stay on the columnar fast path, and only a
// projection whose expressions reach outside itself may fall back to reassembly.
//
// This is a performance contract enforced as a correctness one, because the cost is invisible
// from the rows -- both paths return identical bytes, and the slow one simply reassembles
// every record. Before the plan was made closure-aware, every chasing query took the slow
// path: measured on 20k real job ads, 266ms against 93ms for a three-attribute projection.
// RecordsClosureReassembled is the counter that tells the two apart.
func TestClosureKeepsTheColumnarPathForLiterals(t *testing.T) {
	t.Parallel()
	c, _ := refFixture(t, 20000, true)
	q := mustParseQ(t, "ProcId < 4")

	for _, tc := range []struct {
		name      string
		proj      []string
		wantFalls bool
	}{
		{"literals only", []string{"ClusterId", "ProcId", "JobStatus"}, false},
		{"wider, still literals", []string{"ClusterId", "ProcId", "JobStatus", "Owner",
			"RequestMemory", "RequestDisk", "ResidentSetSize", "DiskUsage", "CgroupFloor"}, false},
		// Requirements refers to siblings the projection does not name, so these records
		// genuinely cannot be served from the projected columns alone.
		{"expression reaching outside", []string{"ClusterId", "Requirements"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st ScanStats
			rows := 0
			for range c.QueryRawWireRefsStats(q, tc.proj, false, &st) {
				rows++
			}
			if rows == 0 {
				t.Fatal("no rows: the fixture or constraint stopped matching, so this proves nothing")
			}
			if st.RecordsColumnDecided == 0 {
				t.Fatal("no record was decided from columns: the fixture is not exercising the " +
					"columnar path at all, so the counter below cannot mean anything")
			}
			switch {
			case tc.wantFalls && st.RecordsClosureReassembled == 0:
				t.Errorf("a projection naming an expression over non-projected siblings never fell "+
					"back (%d rows): the self-contained check is not discriminating, which means "+
					"rows may be shipping without their closure", rows)
			case !tc.wantFalls && st.RecordsClosureReassembled != 0:
				t.Errorf("a projection of literals fell back %d time(s) of %d visited: literals refer "+
					"to nothing, so every record should have been served from its columns",
					st.RecordsClosureReassembled, st.RecordsVisited)
			}
		})
	}
}
