package collections

import (
	"encoding/binary"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// integrityArchiveOptions is a small identity-codec archive, so a test can find and damage a
// record's stored bytes directly. internAtSeal selects interned (segment-dictionary) sealed
// segments rather than inline-keyed ones; the columnar projection reads the two differently.
func integrityArchiveOptions(dir string, internAtSeal bool) ArchiveOptions {
	return ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, InternAtSeal: internAtSeal,
		ZoneAttrs: []string{"ID"}}
}

func integrityAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ MyType="Job"; TargetType="Machine"; ID=%d; ProcId=%d; JobStatus=%d; Owner="user%d"`,
		i, i%10, 1+i%5, i%7)
	fmt.Fprintf(&b, `; RequestMemory=%d; Cmd="/bin/run-%d"`, 1024*(1+i%4), i%13)
	for f := 0; f < 6; f++ {
		fmt.Fprintf(&b, `; Filler%02d="filler %d-%d"`, f, i, f)
	}
	b.WriteString(" ]")
	return b.String()
}

// buildIntegrityArchive writes a columnarized archive with an unsealed tail and closes it,
// returning its directory.
func buildIntegrityArchive(t *testing.T, internAtSeal bool) string {
	t.Helper()
	dir := t.TempDir()
	a, err := CreateArchive(integrityArchiveOptions(dir, internAtSeal))
	if err != nil {
		t.Fatal(err)
	}
	app := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := a.Append(mustAd(t, integrityAd(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	app(0, 1500)
	if !a.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	app(1500, 1600)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openIntegrityArchive(t *testing.T, dir string, internAtSeal bool) *Collection {
	t.Helper()
	a, err := OpenArchive(integrityArchiveOptions(dir, internAtSeal))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if !a.c.VerifyingReads() {
		t.Fatal("read verification is off: the checksum arm proves nothing")
	}
	if columnarSegmentsForTest(a.c) == 0 {
		t.Fatal("no columnarized segment after reopen")
	}
	return a.c
}

// corruptRecordCRCs flips one bit of the stored checksum of the first n keyed records of every
// segment file under dir. The ad bytes are untouched, so anything that reads them without
// verifying decodes a perfectly good ad -- which is exactly what makes serving it wrong.
func corruptRecordCRCs(t *testing.T, dir string, n int) int {
	t.Helper()
	flipped := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".dat") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		done := 0
		for _, off := range recordOffsets(t, path) {
			if done == n {
				break
			}
			if recIsMarker(b, uint32(off)) {
				continue // a dictionary or columnar payload record, not an ad
			}
			kl := int(binary.LittleEndian.Uint32(b[off+recKeyLenOff:]) & keyLenMask)
			adLenOff := off + recKeyOff + kl
			crcOff := adLenOff + 4 + int(binary.LittleEndian.Uint32(b[adLenOff:]))
			flipBit(t, path, crcOff)
			done++
		}
		flipped += done
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return flipped
}

var integrityProj = []string{"ID", "ProcId", "Owner"}

// rawIDs collects the ID of every RawAd.
func rawIDs(t *testing.T, seq iter.Seq[RawAd]) []int {
	t.Helper()
	var out []int
	for ra := range seq {
		out = append(out, idOfExprs(t, ra.Exprs))
	}
	slices.Sort(out)
	return out
}

// wireIDs collects the ID of every wire-form row.
func wireIDs(t *testing.T, seq iter.Seq[[]byte]) []int {
	t.Helper()
	var out []int
	for row := range seq {
		buf, offs, _, _, ok := RenderRawAdInline(row, nil, nil)
		if !ok {
			t.Fatal("RenderRawAdInline refused a wire row")
		}
		exprs := make([][]byte, 0, len(offs))
		for i := 0; i+1 < len(offs); i++ {
			exprs = append(exprs, buf[offs[i]:offs[i+1]])
		}
		out = append(out, idOfExprs(t, exprs))
	}
	slices.Sort(out)
	return out
}

func idOfExprs(t *testing.T, exprs [][]byte) int {
	t.Helper()
	for _, e := range exprs {
		if v, ok := strings.CutPrefix(string(e), "ID = "); ok {
			id, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("ID %q: %v", v, err)
			}
			return id
		}
	}
	t.Fatalf("row without an ID: %q", exprs)
	return 0
}

// totalReadSkips is every record-read refusal counted so far, across all reasons.
func totalReadSkips() int64 {
	var n int64
	for _, v := range RecordReadSkips() {
		n += v
	}
	return n
}

// integrityConsumers are the reads that hand a projection plan to the scan, and so can reach the
// columnar projection. Each returns the IDs it served.
func integrityConsumers(t *testing.T, c *Collection, q *vm.Query) map[string]func() []int {
	return map[string]func() []int{
		"QueryRawWire":     func() []int { return wireIDs(t, c.QueryRawWire(q, integrityProj, false)) },
		"QueryRawWireRefs": func() []int { return wireIDs(t, c.QueryRawWireRefs(q, integrityProj, false)) },
		"QueryRawFromSeq": func() []int {
			seq, _ := c.QueryRawFromSeq(q, integrityProj, SeqCursor{}, 0)
			return rawIDs(t, seq)
		},
		"QueryRawProjected":      func() []int { return rawIDs(t, c.QueryRawProjected(q, integrityProj, false, false)) },
		"QueryRawProjected/refs": func() []int { return rawIDs(t, c.QueryRawProjected(q, integrityProj, true, false)) },
	}
}

// integrityReference is the read with no projection plan: every matched record goes through
// wireAt, whose integrity gate is the behaviour the columnar projection must reproduce.
func integrityReference(t *testing.T, c *Collection, q *vm.Query) []int {
	return rawIDs(t, func(yield func(RawAd) bool) {
		c.scanQuery(c.newQueryPlan(q, nil, nil), c.yieldRawProjected(yield, c.newRawProjector(integrityProj, false, false)))
	})
}

// checkIntegrityParity runs every consumer against the plan-less reference: the same IDs, and the
// same number of refusals counted.
func checkIntegrityParity(t *testing.T, c *Collection, wantRefused bool) {
	t.Helper()
	for _, cons := range []string{"true", "ProcId < 5"} {
		q := mustParseQ(t, cons)
		before := totalReadSkips()
		want := integrityReference(t, c, q)
		refSkips := totalReadSkips() - before
		if len(want) == 0 {
			t.Fatalf("%s: the reference matched nothing", cons)
		}
		if wantRefused && cons == "true" && refSkips == 0 {
			t.Fatalf("%s: the reference refused nothing: the damage did not reach a read", cons)
		}
		for name, run := range integrityConsumers(t, c, q) {
			before := totalReadSkips()
			got := run()
			skips := totalReadSkips() - before
			if !slices.Equal(got, want) {
				extra := 0
				for _, id := range got {
					if _, found := slices.BinarySearch(want, id); !found {
						extra++
					}
				}
				t.Errorf("%s %s: %d rows (%d the plain scan refuses), want %d", name, cons, len(got), extra, len(want))
			}
			if skips != refSkips {
				t.Errorf("%s %s: %d read refusals counted, the plain scan counts %d", name, cons, skips, refSkips)
			}
		}
	}
}

// TestColumnarProjectionRefusesBadChecksum: a record whose stored checksum fails is refused by
// every read that reassembles it. The columnar projection serves a record partly from its own
// bytes, so it must refuse the same records and count them the same way -- otherwise a projected
// read returns rows a whole-ad read of the same table does not.
func TestColumnarProjectionRefusesBadChecksum(t *testing.T) {
	for _, interned := range []bool{false, true} {
		t.Run(fmt.Sprintf("interned=%v", interned), func(t *testing.T) {
			dir := buildIntegrityArchive(t, interned)
			{
				// The fixture must actually take the columnar projection, or parity is vacuous.
				c := openIntegrityArchive(t, dir, interned)
				var st ScanStats
				for range c.QueryRawWireStats(mustParseQ(t, "ProcId < 5"), integrityProj, false, &st) {
				}
				if st.RowsMatched == 0 || st.RecordsReassembled*2 > st.RowsMatched {
					t.Fatalf("columnar projection not taken: %d matched, %d reassembled",
						st.RowsMatched, st.RecordsReassembled)
				}
				c.Close()
			}
			if n := corruptRecordCRCs(t, dir, 3); n == 0 {
				t.Fatal("no record damaged")
			}
			before := VerifyReadSkips()
			checkIntegrityParity(t, openIntegrityArchive(t, dir, interned), true)
			if VerifyReadSkips() == before {
				t.Error("no checksum failure was counted")
			}
		})
	}
}

// TestColumnarProjectionRefusesUnusableDictionary: records in a segment whose dictionary is
// unusable are refused by the reassembling read; the columnar projection must refuse them too.
//
// Recovery never publishes a columnar payload for such a segment, so from disk this state does
// not reach the projection today. It is constructed here by marking a columnarized segment after
// open, which is the state the gate exists to keep safe should those two ever come apart.
func TestColumnarProjectionRefusesUnusableDictionary(t *testing.T) {
	dir := buildIntegrityArchive(t, true)
	c := openIntegrityArchive(t, dir, true)
	marked := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.colNative.Load() != nil && seg.dict.Load() != nil && marked < 2 {
				seg.dictUnusable.Store(true)
				marked++
			}
		}
		sh.mu.RUnlock()
	}
	if marked == 0 {
		t.Fatal("no interned columnarized segment to mark")
	}
	checkIntegrityParity(t, c, true)
}
