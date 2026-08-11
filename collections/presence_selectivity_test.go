package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// presenceEstimates returns the estimated candidates for `attr is undefined` and
// `attr isnt undefined` over c, summed across segments the way the planner sees them.
func presenceEstimates(t *testing.T, c *Collection, attr string, cat bool) (absent, present float64) {
	t.Helper()
	spec := c.spec.Load()
	var id uint32
	var ok bool
	if spec.inline {
		id, ok = spec.nameToID[attr]
	} else {
		id, ok = c.intern.LookupID(attr)
	}
	if !ok {
		t.Fatalf("%s is not indexed; the estimator would not be consulted", attr)
	}
	mk := func(op string) usableProbe { return usableProbe{attrID: id, op: op, cat: cat} }
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			absent += indexEstCandidates(si, mk("absent"))
			present += indexEstCandidates(si, mk("present"))
		}
	}
	return absent, present
}

// TestPresenceEstimatesAreComplementary is the reported symptom: `attr is undefined` and
// `attr isnt undefined` are complements, so their estimates must partition the indexed
// records. Both were previously estimated at the count of records that HAVE the attribute,
// so a probe matching nothing and a probe matching everything scored the same -- and
// selectivity exists precisely to tell those apart.
func TestPresenceEstimatesAreComplementary(t *testing.T) {
	c := New(Options{Shards: 1, ValueAttrs: []string{"JobStatus"}})
	defer c.Close()
	const n = 400
	for i := range n {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; JobStatus=%d ]`, i, i%6))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex() // segment indexes (and their stats) are what the estimator reads
	absent, present := presenceEstimates(t, c, "jobstatus", false)

	// Every ad carries JobStatus, so `is undefined` matches nothing.
	if absent > float64(n)*0.01 {
		t.Errorf("`JobStatus is undefined` estimated %.0f of %d; every ad has JobStatus", absent, n)
	}
	if present < float64(n)*0.99 {
		t.Errorf("`JobStatus isnt undefined` estimated %.0f of %d; every ad has JobStatus", present, n)
	}
	// The two partition the records: neither may exceed the total, and they must not both
	// claim (nearly) all of them.
	if sum := absent + present; sum > float64(n)*1.01 {
		t.Errorf("absent(%.0f) + present(%.0f) = %.0f, exceeds %d indexed records",
			absent, present, sum, n)
	}
}

// TestPresenceEstimatesWithMixedPresence: with the attribute on only some ads, each estimate
// must track its own side rather than both reporting the same number.
func TestPresenceEstimatesWithMixedPresence(t *testing.T) {
	c := New(Options{Shards: 1, ValueAttrs: []string{"JobStatus"}})
	defer c.Close()
	const n = 400
	withAttr := 0
	for i := range n {
		text := fmt.Sprintf(`[ ClusterId=%d ]`, i)
		if i%4 == 0 { // a quarter carry JobStatus
			text = fmt.Sprintf(`[ ClusterId=%d; JobStatus=%d ]`, i, i%6)
			withAttr++
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex() // segment indexes (and their stats) are what the estimator reads
	absent, present := presenceEstimates(t, c, "jobstatus", false)

	wantAbsent, wantPresent := float64(n-withAttr), float64(withAttr)
	if absent < wantAbsent*0.9 || absent > wantAbsent*1.1 {
		t.Errorf("absent estimate %.0f, want ~%.0f", absent, wantAbsent)
	}
	if present < wantPresent*0.9 || present > wantPresent*1.1 {
		t.Errorf("present estimate %.0f, want ~%.0f", present, wantPresent)
	}
	// The bug made the absent estimate track the PRESENT count, so this is the ordering
	// that actually mattered: with a quarter of ads carrying the attribute, `absent` must
	// be the larger of the two.
	if absent <= present {
		t.Errorf("absent(%.0f) <= present(%.0f) when only %d of %d ads carry the attribute",
			absent, present, withAttr, n)
	}
}
