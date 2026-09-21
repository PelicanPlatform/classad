package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A columnarized segment's records are written WITHOUT the attributes its payload holds. If that
// payload is absent -- not corrupt, absent -- every read used to return the remnant, which decodes
// cleanly into an ad simply missing its schema'd attributes. Nothing errored and nothing said so.
//
// On a production mirror that was 2,687 job rows with no ClusterId, JobStatus, Owner or QDate,
// while the high-cardinality attributes survived; the split matched the table's hot columns
// exactly. Worse, compaction copies such a record verbatim when it believes the segment is not
// columnarized, turning a recoverable read fault into permanent loss in a segment that then looks
// healthy.
//
// recIsStripped marks the remnants at write time so a reader learns it from the RECORD, not from
// segment state -- which is the thing that goes missing when this breaks.

// columnarizedFixture builds a persistent collection with at least one columnarized segment and
// returns it plus one columnarized segment.
func columnarizedFixture(t *testing.T) (*Collection, *segment) {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 12, ColumnarSegmentBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; `+
				`RequestCpus=%d; QDate=%d; RemoteHost="slot%d@host%d.example" ]`,
			i, i%10, i%37, 1+i%5, (i%16)*1024, 1+i%8, 1600000000+i, i%64, i%128))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	sch, hot, ok := c.deriveSchema(4096, 12)
	if !ok || !c.installSchemaScan(sch, hot) {
		t.Fatal("no schema derived: the fixture would not columnarize")
	}
	c.EnableSchemaScan(sch, hot)
	if n := c.ColumnarizeSealed(); n == 0 {
		t.Fatal("nothing columnarized: this test would assert nothing")
	}
	var found *segment
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.columnarized() {
				found = seg
				break
			}
		}
		sh.mu.RUnlock()
	}
	if found == nil {
		t.Fatal("no columnarized segment")
	}
	return c, found
}

// TestRemnantsAreMarked: the records of a columnarized segment must say so themselves.
func TestRemnantsAreMarked(t *testing.T) {
	c, seg := columnarizedFixture(t)
	defer c.Close()
	marked, plain := 0, 0
	for off := 0; off < seg.used; {
		o := uint32(off)
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(seg.data, o) {
			if recIsStripped(seg.data, o) {
				marked++
			} else {
				plain++
			}
		}
		off += int(total)
	}
	if marked == 0 {
		t.Errorf("no record in a columnarized segment is marked a remnant (%d unmarked)", plain)
	}
	t.Logf("%d remnants marked, %d unmarked records", marked, plain)
}

// TestMissingPayloadErrorsRatherThanServingRemnants is the regression. The payload is withheld the
// way a failed publish leaves it -- nil pointer, NOT flagged damaged, which is the combination that
// used to fall through to "return the bytes as they are".
func TestMissingPayloadErrorsRatherThanServingRemnants(t *testing.T) {
	c, seg := columnarizedFixture(t)
	defer c.Close()

	// Truth first: with the payload present, a record reads back whole.
	var off uint32
	for o := 0; o < seg.used; {
		u := uint32(o)
		total := recTotalLen(seg.data, u)
		if total == 0 {
			break
		}
		if !recIsMarker(seg.data, u) && recIsStripped(seg.data, u) {
			off = u
			break
		}
		o += int(total)
	}
	full, err := c.recordWireIn(seg, seg.data, off, nil)
	if err != nil {
		t.Fatalf("with the payload present the record did not reassemble: %v", err)
	}
	whole, err := c.decodeAd(full, identityCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := whole.EvaluateAttrInt("JobStatus"); !ok {
		t.Fatal("fixture record has no JobStatus even with the payload; nothing to protect")
	}

	// Now withhold it, as a failed publish does.
	seg.colNative.Store(nil)
	seg.colDamaged.Store(false)

	if _, err := c.recordWireIn(seg, seg.data, off, nil); err == nil {
		t.Error("a remnant was served as a whole ad with no payload to complete it")
	}
	// And through the scan path, which is what a query uses.
	if _, _, ok := segStoredOrReassembled(c, seg, off); ok {
		t.Error("segStoredOrReassembled returned a remnant instead of reporting it cannot rebuild")
	}
}

// TestCompactionRefusesRemnantsWithoutPayload: the permanence half. Compaction must not copy a
// remnant into a fresh segment as though it were a whole record.
func TestCompactionRefusesRemnantsWithoutPayload(t *testing.T) {
	c, seg := columnarizedFixture(t)
	defer c.Close()
	seg.colNative.Store(nil)
	seg.colDamaged.Store(false)

	// Compaction reads through the same gate; with the payload withheld it must not produce
	// records that decode to partial ads.
	c.Compact()

	partial, checked := 0, 0
	for i := 0; i < 3000; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i)))
		if !ok {
			continue // a record it refused to rebuild is skipped, which is the safe direction
		}
		checked++
		if _, has := ad.EvaluateAttrInt("JobStatus"); !has {
			partial++
		}
	}
	if partial > 0 {
		t.Errorf("%d of %d readable rows lost JobStatus: compaction baked in a remnant", partial, checked)
	}
	t.Logf("%d rows readable after compacting with the payload withheld, 0 partial", checked)
}
