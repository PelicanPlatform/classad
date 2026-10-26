package collections

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestColumnarProjectionAfterRestartNamesTheRightAttribute: a columnarized segment's cold tail is keyed
// by the ids it was written with, and reading it in a later process translates through the names the
// payload records. A name the payload does not hold used to be looked up as its own current id, which
// after a restart is as likely as not the stored id of another attribute -- so projecting an attribute
// a record lacks returned another attribute's value under the asked-for name. Each rare attribute here
// lives in one stretch of the table, so every segment's tail holds a different few, and the projection
// of each is checked against the whole record, before and after a restart, for inline segments and for
// ones keyed by a segment dictionary. The columnar counts read the same tails and are checked too.
func TestColumnarProjectionAfterRestartNamesTheRightAttribute(t *testing.T) {
	t.Parallel()
	for _, interned := range []bool{false, true} {
		t.Run(fmt.Sprintf("interned=%v", interned), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			open := func() *Collection {
				c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: -1})
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			c := open()
			const rare = 24
			for i := range 3000 {
				extra := ""
				if i%7 == 0 {
					extra = fmt.Sprintf("; X%d=%d", (i/120)%rare, i)
				}
				ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d%s ]`, i, i%10, i%7, i%6, extra))
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
					t.Fatal(err)
				}
			}
			if interned {
				if _, err := c.RetrainDict(4096); err != nil {
					t.Fatal(err)
				}
				dicts := 0
				for _, seg := range c.shards[0].segs {
					if seg != nil && seg.dict.Load() != nil {
						dicts++
					}
				}
				if dicts == 0 {
					t.Fatal("no segment was interned; the dictionary-keyed case goes untested")
				}
			}
			if !c.BuildAndEnableSchemaScan(4096, 8) {
				t.Fatal("no schema")
			}
			check := func(stage string) {
				t.Helper()
				for x := range rare {
					proj := []string{"ClusterId", fmt.Sprintf("X%d", x)}
					got, _ := seqPage(c, nil, proj, SeqCursor{}, 0)
					want, _ := refQueryRawFromSeq(c, nil, proj, SeqCursor{}, 0)
					if !slices.Equal(got, want) {
						for i := range got {
							if i < len(want) && got[i] != want[i] {
								t.Fatalf("%s, projecting %v: row %d reads %q, the whole record %q", stage, proj, i, got[i], want[i])
							}
						}
						t.Fatalf("%s, projecting %v: %d rows, the whole records give %d", stage, proj, len(got), len(want))
					}
					// The columnar count and aggregate paths read the same tails.
					for _, e := range []string{fmt.Sprintf("X%d > 0", x), fmt.Sprintf("X%d is undefined", x)} {
						q, err := vm.Parse(e)
						if err != nil {
							t.Fatal(err)
						}
						if n, served := c.CountQuery(q); served {
							if want := len(readAll(t, c, e)); n != want {
								t.Fatalf("%s, counting %s: %d from the columns, %d from the records", stage, e, n, want)
							}
						}
					}
				}
			}
			check("before the restart")
			if columnarSegmentsForTest(c) == 0 {
				t.Fatal("nothing columnarized")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c = open()
			defer c.Close()
			check("after the restart")
		})
	}
}
