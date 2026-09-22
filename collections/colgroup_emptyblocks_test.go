package collections

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// makeEveryRecordDelta sets the delta flag on every data record in a segment, which is how a
// sealed segment of pure deltas looks to the columnar builder: its skip function excludes
// delta records, so nothing is encodable and no block is built from the records.
func makeEveryRecordDelta(seg *segment) {
	for off := uint32(0); off < uint32(seg.used); {
		tl := recTotalLen(seg.data, off)
		if tl == 0 || off+tl > uint32(seg.used) {
			break
		}
		if !recIsMarker(seg.data, off) {
			f := binary.LittleEndian.Uint32(seg.data[off+recKeyLenOff:])
			binary.LittleEndian.PutUint32(seg.data[off+recKeyLenOff:], f|deltaFlag)
		}
		off += tl
	}
}

// The two returns of the columnar builder are not the same length in every case, and the one
// case where they differ is easy to miss: a segment with no encodable records still gets one
// synthetic block so the colSegment carries its schema, and no group row is built beside it.
//
// This pins that contract, because the consumers index the group rows BY BLOCK POSITION and a
// silent change here would put the bounds check in those consumers back out of step.
func TestEmptySegmentBuildsBlockWithoutGroupRow(t *testing.T) {
	c, st, seg := emptyGroupRowFixture(t)
	blocks, gblocks, _ := buildColumnarFromSegmentGrouped(seg.data, seg.used, seg.codec,
		c.regionCodec(), st.schema, st.hot, st.groups, c.colGrouping(),
		func(dst, w []byte) ([]byte, bool) { return w, true },
		func(u uint32) (uint32, bool) { return u, true },
		func(o uint32) bool { return true }) // nothing is encodable

	if len(blocks) != 1 {
		t.Fatalf("len(blocks) = %d, want the single synthetic block", len(blocks))
	}
	if len(gblocks) != 0 {
		t.Fatalf("len(gblocks) = %d, want 0: the synthetic block gets no group row", len(gblocks))
	}
}

// Building the read accelerator for a segment of pure deltas, with a group set in use, must not
// index a group row that was never built. This crashed a production maintenance pass seven
// times -- Compact -> Reindex -> sealSegmentIndex -> colBlobForSeg -> buildColSegment -- taking
// the daemon down with it each time, since a panic in a background maintenance goroutine is not
// recoverable by the request path.
func TestColSegmentSurvivesGroupsWithNoGroupRows(t *testing.T) {
	c, st, seg := emptyGroupRowFixture(t)
	makeEveryRecordDelta(seg)

	cs := c.buildColSegment(seg, st.schema, st.hot)
	// The point is that it returns rather than panicking; and with no encodable records there is
	// no group coverage to record, so the groups must be dropped rather than half-filled.
	if cs != nil && len(cs.groups) != 0 {
		t.Errorf("colSegment carries %d groups for a segment with no encodable records", len(cs.groups))
	}
}

// groupFixture returns a collection whose scan state carries a GROUP SET -- the state a table
// reaches once its stability history is long enough -- plus one sealed segment. The group set is
// installed directly because deriving one takes more history than a unit test should write, and
// its contents do not matter here: what matters is that the group loop runs at all.
func emptyGroupRowFixture(t *testing.T) (*Collection, *schemaScanState, *segment) {
	t.Helper()
	// ColumnarSegmentBudget negative disables the rewrite, so enabling the schema scan leaves the
	// sealed segments in plain form. buildColSegment returns early for an already-columnarized
	// segment, so without this the fixture finds nothing to test and both tests skip themselves.
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16, DeltaMax: 64,
		ColumnarSegmentBudget: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	for i := range 400 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ProcId", int64(0))
		ad.InsertAttr("JobStatus", int64(1))
		ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%7))
		ad.InsertAttr("RequestMemory", int64((i%16)*1024))
		for j := range 12 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		tx.Commit()
	}
	if !c.BuildAndEnableSchemaScan(2000, 32) {
		t.Fatal("schema scan did not enable")
	}
	st := c.schemaScan.Load()
	if st == nil || st.schema == nil {
		t.Fatal("no schema published")
	}
	withGroups := *st
	withGroups.groups = []*colGroup{{schema: st.schema, ids: []uint32{0, 1}}}
	c.schemaScan.Store(&withGroups)

	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, sg := range sh.segs {
		if sg != nil && sg != sh.act && sg.used > 0 && !sg.columnarized() {
			return c, &withGroups, sg
		}
	}
	t.Fatal("no plain sealed segment: the fixture wrote nothing to test")
	return nil, nil, nil
}
