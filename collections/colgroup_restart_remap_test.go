package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestGlobalPayloadGroupColdTailSurvivesRestart: a payload with no segment dictionary keys its cold tails
// by the writing process's intern ids and records their names, and a reopen translates through those
// names. The translation used to reach only the base blocks, so a group column value that escaped into
// the GROUP block's cold tail (AaExit is an integer column; a few records hold a string) was looked up by
// this process's id after a restart: it counted as absent, and Get returned another attribute's value
// under its name. The writer interns two names first, so every id moves across the reopen. Both places
// the payload lives are covered: inside a columnarized segment, where the group columns are the only copy
// of the value, and the sidecar copy beside an untouched record (ColumnarSegmentBudget -1).
func TestGlobalPayloadGroupColdTailSurvivesRestart(t *testing.T) {
	t.Parallel()
	adText := func(i int) string {
		if i%3 != 0 {
			return fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d ]`, i, i%10, i%7, i%6)
		}
		exit := fmt.Sprintf("%d", i%3+1)
		if i%150 == 0 {
			exit = `"aborted"`
		}
		return fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; AaStart=%d; AaEnd=%d; AaHost="h%d"; AaExit=%s ]`,
			i, i%10, i%7, i%6, i, i+10, i%50, exit)
	}
	const n, escaped = 4000, 27
	for _, tc := range []struct {
		name   string
		budget int
	}{{"columnarized", 0}, {"sidecar", -1}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			open := func() *Collection {
				c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: 4,
					GroupStabilityRuns: 1, ColumnarSegmentBudget: tc.budget})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			c := open()
			// Intern ids are first-seen and process-local; the reopened process never meets these.
			c.intern.Intern("Decoy0")
			c.intern.Intern("Decoy1")
			for i := range n {
				ad, err := classad.Parse(adText(i))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
					t.Fatal(err)
				}
			}
			// No RetrainDict: the segments stay without a dictionary, so the payload is globally keyed.
			if !c.BuildAndEnableSchemaScan(4096, 8) {
				c.Close()
				t.Fatal("schema scan did not enable")
			}
			writerExit, _ := c.intern.LookupID("AaExit")
			// The case under test must be reached: a globally-keyed payload, in the place this variant
			// covers, whose group block's cold tail holds AaExit.
			reached := false
			for _, seg := range c.shards[0].segs {
				if seg == nil {
					continue
				}
				cs := seg.colblk.Load()
				if cs == nil || cs.dictKeyed || seg.dict.Load() != nil || seg.columnarized() != (tc.budget >= 0) {
					continue
				}
				for _, g := range cs.groups {
					for _, gb := range g.blocks {
						if gb == nil || gb.blk == nil {
							continue
						}
						// Read from the tail itself: a block loaded from a payload does not carry
						// the build-time id set.
						tail, err := c.sharedColCache().stream(gb.blk, kindCold)
						if err != nil {
							t.Fatal(err)
						}
						ids := map[uint32]struct{}{}
						collectColdIDs(tail, ids)
						if _, ok := ids[writerExit]; ok {
							reached = true
						}
					}
				}
			}
			if !reached {
				c.Close()
				t.Fatal("no globally-keyed payload held AaExit in a group block's cold tail")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}

			c2 := open()
			defer c2.Close()
			if id, _ := c2.intern.LookupID("AaExit"); id == writerExit {
				t.Fatalf("AaExit kept id %d across the reopen, so the two id spaces never diverged", id)
			}
			q, err := vm.Parse(`AaExit == "aborted"`)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := c2.CountQuery(q); !ok || got != escaped {
				t.Errorf("CountQuery after restart = %d (served %v), want %d", got, ok, escaped)
			}
			if got, ok := c2.ColumnarEvalCount(q); !ok || got != escaped {
				t.Errorf("ColumnarEvalCount after restart = %d (served %v), want %d", got, ok, escaped)
			}
			bad := 0
			for i := 0; i < n; i += 150 {
				ad, ok := c2.Get([]byte(fmt.Sprintf("%d.0", i)))
				if !ok {
					t.Fatalf("record %d missing after restart", i)
				}
				if s, ok := ad.EvaluateAttrString("AaExit"); !ok || s != "aborted" {
					if bad < 3 {
						t.Errorf("record %d after restart: %s", i, ad)
					}
					bad++
				}
			}
			if bad > 0 {
				t.Errorf("%d of %d escaped AaExit values read wrong after restart", bad, escaped)
			}
		})
	}
}
