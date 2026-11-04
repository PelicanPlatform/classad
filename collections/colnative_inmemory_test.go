package collections

import (
	"fmt"
	"iter"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// An in-memory collection columnarizes its sealed segments exactly as a persistent one does.
//
// It did not, for a reason that sounded structural and was not: the rewrite stages its output under
// a temporary file name and commits it with a marker/rename protocol, so it refused a shard with no
// directory to stage into. But staging and renaming are the crash-recovery half of the commit, and a
// RAM shard has no crash to recover from -- its segments do not outlive the process. The half that
// is actually about the shard, the swap under the shard lock, is identical for both backings.
//
// What the refusal cost was not the column scan: a RAM collection still covered its sealed segments
// with SIDECAR blocks and served columns from those. It cost the dedup. A covered segment holds every
// schema'd value twice, once in the record and once in the block; a columnarized one holds it only in
// the block. So the backing where memory is the whole budget was the one carrying the second copy.
//
// It also left a divergence that a test cannot see from its answers, which is the dangerous kind.

// inmemDiffAds is the fixture both backings are built from: enough ads to seal several segments at
// the small segment size below, with a unique Seq so rows can be compared across two shardings.
func inmemDiffAds(t *testing.T, c *Collection, n int) {
	t.Helper()
	for i := range n {
		text := fmt.Sprintf(`[ Seq=%d; ClusterId=%d; ProcId=%d; JobStatus=%d; RequestCpus=%d;
			Owner="user%03d"; Cmd="/bin/thing%d"; ImageSize=%d; Frac=%.3f; Flag=%t ]`,
			i, i/10, i%10, i%5+1, i%8+1, i%17, i%7, 100000+i*37, float64(i)/8, i%3 == 0)
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.%d", i/10, i%10)), ad); err != nil {
			t.Fatal(err)
		}
	}
}

// colRow is one produced record, copied out of the borrowed batch (Batch's strings alias the block
// they were read from, so comparing them after the scan requires a copy).
type colRow struct {
	kinds []ValueKind
	nums  []int64
	strs  []string
}

// drainColumns runs a column scan and returns its rows sorted by the first projected attribute,
// which the fixture makes unique. Sorting is what lets two different shardings be compared: the
// two backings seal different numbers of segments for the same ads, so scan ORDER is not the thing
// under test and must not be mistaken for it.
func drainColumns(t *testing.T, c *Collection, expr string, attrs []string) ([]colRow, ColumnScanStats) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	var rows []colRow
	st, ok := c.ScanColumns(q, attrs, func(b *Batch) bool {
		for i := range b.N {
			r := colRow{
				kinds: make([]ValueKind, len(b.Cols)),
				nums:  make([]int64, len(b.Cols)),
				strs:  make([]string, len(b.Cols)),
			}
			for j := range b.Cols {
				r.kinds[j] = b.Cols[j].Kind[i]
				r.nums[j] = b.Cols[j].Num[i]
				r.strs[j] = strings.Clone(b.Cols[j].Str[i])
			}
			rows = append(rows, r)
		}
		return true
	})
	if !ok {
		t.Fatalf("ScanColumns declined %q over %v", expr, attrs)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].nums[0] < rows[j].nums[0] })
	return rows, st
}

func (r colRow) String() string {
	var b strings.Builder
	for i := range r.kinds {
		if i > 0 {
			b.WriteString(" ")
		}
		switch r.kinds[i] {
		case ValueString:
			fmt.Fprintf(&b, "%q", r.strs[i])
		case ValueReal:
			fmt.Fprintf(&b, "%g", math.Float64frombits(uint64(r.nums[i])))
		case ValueUndefined:
			b.WriteString("undefined")
		case ValueError:
			b.WriteString("error")
		default:
			fmt.Fprintf(&b, "%d", r.nums[i])
		}
	}
	return b.String()
}

// inmemDiffCollections returns the in-memory and persistent halves of the comparison, built from
// the same ads with the same options, each with its sealed segments columnarized.
func inmemDiffCollections(t *testing.T) (mem, disk *Collection) {
	t.Helper()
	// The budget is raised past anything the fixture can produce so ONE enable pass columnarizes
	// every sealed segment. The default (64) converges over several maintenance passes, which is
	// right in production and would make "all of them, now" a coin flip here.
	opts := Options{Shards: 4, SegmentSize: 1 << 16, GroupSchemaCount: -1, ColumnarSegmentBudget: 4096}
	mem = New(opts)
	t.Cleanup(func() { mem.Close() })
	dopts := opts
	dopts.Dir = t.TempDir()
	disk, err := Open(dopts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	for _, c := range []*Collection{mem, disk} {
		inmemDiffAds(t, c, 24000)
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("schema scan did not enable")
		}
	}
	return mem, disk
}

// TestInMemoryColumnarMatchesPersistent is the differential: the same ads and the same queries
// through both backings must produce identical rows AND must both have read them from the
// COLUMNS. The second half is the point. Two record scans also agree with each other, so
// comparing answers alone would pass just as happily with the columnar path switched off on
// either side -- which is exactly the shape of green that hid this divergence.
func TestInMemoryColumnarMatchesPersistent(t *testing.T) {
	mem, disk := inmemDiffCollections(t)

	for _, side := range []struct {
		name string
		c    *Collection
	}{{"in-memory", mem}, {"persistent", disk}} {
		info := side.c.SchemaScanInfo()
		if info.SealedSegments == 0 {
			t.Fatalf("%s: no sealed segments, so nothing could be columnarized (fixture too small)", side.name)
		}
		// COLUMNAR-NATIVE, not merely covered: a sidecar block would also serve the scan, and
		// before this change that is all the in-memory side had. The two are counted separately
		// for exactly this reason (SchemaScanInfo.CoveredSegments vs ColumnarSegments).
		if info.ColumnarSegments != info.SealedSegments {
			t.Fatalf("%s: columnarized %d of %d sealed segments; the rewrite did not run on all of them",
				side.name, info.ColumnarSegments, info.SealedSegments)
		}
	}

	attrs := []string{"Seq", "ClusterId", "Owner", "Cmd", "Frac", "Flag", "ImageSize"}
	for _, expr := range []string{
		"true",
		"JobStatus == 2",
		"RequestCpus > 4 && JobStatus < 4",
		`Owner == "user003"`,
		"ImageSize > 150000",
		"Seq % 97 == 3",
	} {
		t.Run(expr, func(t *testing.T) {
			memRows, memSt := drainColumns(t, mem, expr, attrs)
			diskRows, diskSt := drainColumns(t, disk, expr, attrs)

			if len(memRows) != len(diskRows) {
				t.Fatalf("row count: in-memory %d, persistent %d", len(memRows), len(diskRows))
			}
			if len(memRows) == 0 {
				t.Fatal("query matched nothing: the comparison would be vacuous")
			}
			for i := range memRows {
				if memRows[i].String() != diskRows[i].String() {
					t.Fatalf("row %d differs:\n in-memory: %s\npersistent: %s",
						i, memRows[i], diskRows[i])
				}
			}

			// Both sides must have read the values from the columns. Asserted as a RATIO rather
			// than an absolute, because the two backings seal different numbers of segments for the
			// same ads: a RAM record is interned and a persistent one spells its names inline, so
			// the same ads fill a different number of segments and leave different remainders in the
			// ACTIVE segment, which is never columnarized on either backing. That remainder is the
			// only legitimate source of record cells here, and the fixture is sized so it is a few
			// percent; a scan that fell back to the records entirely would read 0% from columns.
			for _, s := range []struct {
				name string
				st   ColumnScanStats
			}{{"in-memory", memSt}, {"persistent", diskSt}} {
				cells := s.st.ColumnCells + s.st.RecordCells
				if cells == 0 {
					t.Fatalf("%s: no cells produced at all", s.name)
				}
				if s.st.ColumnCells*10 < cells*9 {
					t.Errorf("%s: only %d of %d cells came from columns (%d record reads) -- "+
						"this scan fell back to the record path",
						s.name, s.st.ColumnCells, cells, s.st.RecordReads)
				}
				if s.st.RecordReads*10 > s.st.Rows {
					t.Errorf("%s: materialized %d records for %d rows -- the columns did not answer",
						s.name, s.st.RecordReads, s.st.Rows)
				}
				if s.st.VectorBlocks == 0 {
					t.Errorf("%s: no block was evaluated as a vector (stats %+v)", s.name, s.st)
				}
			}
			t.Logf("rows=%d in-memory{col=%d rec=%d reads=%d blocks=%d} persistent{col=%d rec=%d reads=%d blocks=%d}",
				len(memRows),
				memSt.ColumnCells, memSt.RecordCells, memSt.RecordReads, memSt.VectorBlocks,
				diskSt.ColumnCells, diskSt.RecordCells, diskSt.RecordReads, diskSt.VectorBlocks)
		})
	}
}

// TestInMemoryColumnarAgreesWithRecordScan is the other half of the differential, and the one that
// would catch a columnarized RAM segment whose remnant and payload disagree: the same query answered
// by the columns and by a plain record scan of the same collection must name the same records.
//
// The column scan cannot be checked against the persistent side alone, because a rewrite bug common
// to both backings would be invisible there. A record scan reads the stored ads whole and knows
// nothing about the block, so it is an independent oracle.
func TestInMemoryColumnarAgreesWithRecordScan(t *testing.T) {
	mem, _ := inmemDiffCollections(t)
	if n := mem.SchemaScanInfo().ColumnarSegments; n == 0 {
		t.Fatal("no columnarized segment: nothing under test")
	}
	expr := "JobStatus == 3 && RequestCpus > 2"
	rows, st := drainColumns(t, mem, expr, []string{"Seq", "Owner", "Frac"})
	if st.ColumnCells == 0 {
		t.Fatalf("columns produced nothing: %+v", st)
	}

	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{}
	for ad := range mem.Query(q) {
		seq, _ := ad.EvaluateAttrInt("Seq")
		owner, _ := ad.EvaluateAttrString("Owner")
		want[seq] = owner
	}
	if len(want) != len(rows) {
		t.Fatalf("record scan matched %d records, column scan produced %d rows", len(want), len(rows))
	}
	for _, r := range rows {
		owner, ok := want[r.nums[0]]
		if !ok {
			t.Fatalf("column scan produced Seq=%d, which the record scan did not match", r.nums[0])
		}
		if owner != r.strs[1] {
			t.Fatalf("Seq=%d: column Owner %q, record Owner %q", r.nums[0], r.strs[1], owner)
		}
	}
}

// TestInMemoryColumnarWholeAdsOnEveryReadPath covers the risk this change creates rather than the
// one it removes.
//
// Columnarizing rewrites a record into a REMNANT: the attributes the schema carries leave the
// record and live only in the segment's payload, and every reader that wants the whole ad has to
// splice the two back together. The splice is generic -- it keys off the record's own stripped flag,
// not off the backing -- but until now an in-memory collection could not produce a remnant, so none
// of the raw relays below had ever met one. The relays are also the paths an in-memory collection
// uses MOST: QueryRaw and the raw wire readers yield nothing for an inline (persistent) collection
// at all, so persistent coverage says nothing about them here.
//
// A missed splice does not error. It returns the remnant, which is a perfectly well-formed ad that
// is simply missing half its attributes -- so this asserts on every attribute of every ad, not on
// the ones a query happened to touch.
func TestInMemoryColumnarWholeAdsOnEveryReadPath(t *testing.T) {
	mem, _ := inmemDiffCollections(t)
	info := mem.SchemaScanInfo()
	if info.ColumnarSegments == 0 {
		t.Fatal("no columnarized segment: nothing under test")
	}
	t.Logf("in-memory: %d of %d sealed segments columnarized, %d schema fields",
		info.ColumnarSegments, info.SealedSegments, info.SchemaFields)

	// check asserts one ad is whole and self-consistent. Seq determines every other attribute (see
	// inmemDiffAds), so one read value validates the rest without the test carrying a copy of the
	// fixture.
	check := func(t *testing.T, path string, ad *classad.ClassAd) bool {
		t.Helper()
		seq, ok := ad.EvaluateAttrInt("Seq")
		if !ok {
			t.Errorf("%s: an ad came back without Seq", path)
			return false
		}
		i := seq
		type want struct {
			name string
			got  func() (string, bool)
			want string
		}
		wants := []want{
			{"ClusterId", func() (string, bool) { v, ok := ad.EvaluateAttrInt("ClusterId"); return fmt.Sprint(v), ok }, fmt.Sprint(i / 10)},
			{"ProcId", func() (string, bool) { v, ok := ad.EvaluateAttrInt("ProcId"); return fmt.Sprint(v), ok }, fmt.Sprint(i % 10)},
			{"JobStatus", func() (string, bool) { v, ok := ad.EvaluateAttrInt("JobStatus"); return fmt.Sprint(v), ok }, fmt.Sprint(i%5 + 1)},
			{"RequestCpus", func() (string, bool) { v, ok := ad.EvaluateAttrInt("RequestCpus"); return fmt.Sprint(v), ok }, fmt.Sprint(i%8 + 1)},
			{"ImageSize", func() (string, bool) { v, ok := ad.EvaluateAttrInt("ImageSize"); return fmt.Sprint(v), ok }, fmt.Sprint(100000 + i*37)},
			{"Owner", func() (string, bool) { return ad.EvaluateAttrString("Owner") }, fmt.Sprintf("user%03d", i%17)},
			{"Cmd", func() (string, bool) { return ad.EvaluateAttrString("Cmd") }, fmt.Sprintf("/bin/thing%d", i%7)},
			// EvaluateAttrNumber, not EvaluateAttrReal: a real whose value is integral renders
			// without a fractional part on the raw paths and reparses as an int, which is a
			// rendering detail and not the missing attribute this test is looking for.
			{"Frac", func() (string, bool) {
				v, ok := ad.EvaluateAttrNumber("Frac")
				return fmt.Sprintf("%.3f", v), ok
			}, fmt.Sprintf("%.3f", float64(i)/8)},
			{"Flag", func() (string, bool) {
				v, ok := ad.EvaluateAttrBool("Flag")
				return fmt.Sprint(v), ok
			}, fmt.Sprint(i%3 == 0)},
		}
		for _, w := range wants {
			got, ok := w.got()
			if !ok {
				t.Errorf("%s: Seq=%d is missing %s -- the remnant was returned without its columns", path, i, w.name)
				return false
			}
			if got != w.want {
				t.Errorf("%s: Seq=%d %s = %s, want %s", path, i, w.name, got, w.want)
				return false
			}
		}
		return true
	}

	const n = 24000
	all := func(path string, seq iter.Seq[*classad.ClassAd]) {
		t.Helper()
		got := 0
		for ad := range seq {
			got++
			if !check(t, path, ad) {
				return
			}
		}
		if got != n {
			t.Errorf("%s: returned %d ads, want %d", path, got, n)
		}
	}

	all("Scan", mem.Scan())
	all("ScanRawWire", func(yield func(*classad.ClassAd) bool) {
		for w := range mem.ScanRawWire(nil, false) {
			if !yield(wireToAd(t, mem, w)) {
				return
			}
		}
	})
	all("ScanRaw", func(yield func(*classad.ClassAd) bool) {
		for r := range mem.ScanRaw() {
			if !yield(rawToAd(t, r)) {
				return
			}
		}
	})
}
