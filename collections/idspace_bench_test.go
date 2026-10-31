package collections

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

var (
	idspaceBenchOnce sync.Once
	idspaceBenchC    *Collection
)

// idspaceBenchStore: an interned, columnarized store reopened so intern ids and stored ids differ.
func idspaceBenchStore(b *testing.B) *Collection {
	idspaceBenchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "idspace-bench")
		if err != nil {
			b.Fatal(err)
		}
		opts := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1}
		c, err := Open(opts)
		if err != nil {
			b.Fatal(err)
		}
		for i := range 20000 {
			cid := fmt.Sprintf("%d", i)
			if i%10 == 1 {
				cid = `"text"`
			}
			extra := ""
			if i%3 == 0 {
				extra = fmt.Sprintf("; X%d=%d", i%8, i)
			}
			ad, _ := classad.Parse(fmt.Sprintf(`[ ClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d%s ]`, cid, i%10, i%7, i%6, extra))
			if err = c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				b.Fatal(err)
			}
		}
		if _, err = c.RetrainDict(4096); err != nil {
			b.Fatal(err)
		}
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			b.Fatal("no schema")
		}
		c.Close()
		if idspaceBenchC, err = Open(opts); err != nil {
			b.Fatal(err)
		}
	})
	return idspaceBenchC
}

func idspaceBlocks(c *Collection) []*columnarBlock {
	var out []*columnarBlock
	for _, seg := range c.shards[0].segs {
		if seg == nil {
			continue
		}
		if cs := seg.colblk.Load(); cs != nil {
			out = append(out, cs.blocks...)
		}
	}
	return out
}

// BenchmarkIdspaceColdTail reads an escaped schema field and a non-schema attribute out of every
// record's cold tail through the block's id translation.
func BenchmarkIdspaceColdTail(b *testing.B) {
	c := idspaceBenchStore(b)
	blocks := idspaceBlocks(c)
	if len(blocks) == 0 {
		b.Fatal("no blocks")
	}
	cid, _ := c.intern.LookupID("ClusterId")
	x3, _ := c.intern.LookupID("X3")
	bc := c.sharedColCache()
	found := 0
	for b.Loop() {
		for _, blk := range blocks {
			for k := 0; k < blk.n; k++ {
				if _, cell := blk.escapedNumCell(k, globalID(cid), bc); cell != cellAbsent {
					found++
				}
				if _, ok, _ := blk.escapedNode(k, globalID(x3), bc); ok {
					found++
				}
				if _, ok := blk.remap.storedID(globalID(cid)); ok {
					found++
				}
			}
		}
	}
	if found == 0 {
		b.Fatal("found nothing")
	}
}

// BenchmarkIdspaceProjection projects an escaped field and a non-schema attribute from the columns.
func BenchmarkIdspaceProjection(b *testing.B) {
	c := idspaceBenchStore(b)
	proj := []string{"ClusterId", "X3"}
	for b.Loop() {
		rows, _ := seqPage(c, nil, proj, SeqCursor{}, 0)
		if len(rows) == 0 {
			b.Fatal("no rows")
		}
	}
}
