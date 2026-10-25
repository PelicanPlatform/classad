package collections

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestIndexEstimateCountsLiveVersions: a segment index posts every version a mutable table has
// written, so summing its counts estimates the versions, not the ads. A queue mirror patches each
// job's status many times, and each patch is a record that posts JobStatus; the estimate has to
// stay near the number of ads that match, and never exceed the table.
func TestIndexEstimateCountsLiveVersions(t *testing.T) {
	t.Parallel()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 15, DeltaMax: 8,
		ValueAttrs: []string{"JobStatus"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 1000
	for i := 0; i < n; i++ {
		src := fmt.Sprintf(`[ClusterId=%d; JobStatus=1; Cmd="/bin/sleep"; Iwd="/home/user%d"]`, i, i%13)
		if perr := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, src)); perr != nil {
			t.Fatal(perr)
		}
	}
	rng := rand.New(rand.NewSource(1))
	for u := 0; u < 8*n; u++ {
		patch := classad.New()
		patch.InsertAttr("JobStatus", int64(1+rng.Intn(4)))
		tx := c.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", rng.Intn(n))), patch, nil)
		tx.Commit()
		if u%2000 == 0 {
			c.Reindex()
		}
	}
	c.Reindex()
	q, err := vm.Parse(`JobStatus == 2`)
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for range c.Query(q) {
		matches++
	}
	ex := c.ExplainQuery(q)
	if len(ex.Probes) != 1 || !ex.Probes[0].HasSelectivity {
		t.Fatalf("no index estimate for the probe: %+v", ex.Probes)
	}
	pe := ex.Probes[0]
	t.Logf("%d ads, %d match, probe estimate %d, plan estimate %d", ex.TotalAds, matches, pe.EstCandidates, ex.EstCandidates)
	for name, est := range map[string]int64{"probe": pe.EstCandidates, "plan": ex.EstCandidates} {
		if est > int64(ex.TotalAds) {
			t.Errorf("%s estimate %d exceeds the %d ads in the table", name, est, ex.TotalAds)
		}
		if est > 2*int64(matches) || 2*est < int64(matches) {
			t.Errorf("%s estimate %d is not within 2x of the %d live matches", name, est, matches)
		}
	}
}
