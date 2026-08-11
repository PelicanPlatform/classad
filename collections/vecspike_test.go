package collections

import (
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// SPIKE: what would vectorized evaluation be worth?
//
// colScope evaluates a query per RECORD: one interpreter dispatch per operator per record, each
// producing a boxed classad.Value. Vectorized execution inverts that loop -- for each OPERATOR, apply
// it to a whole batch of values: load a column as a contiguous vector, compare it into a bitmap, AND
// the bitmaps, popcount. Dispatch is amortized over the batch and the inner loops are contiguous and
// branch-predictable.
//
// The reason to believe it applies here is that the hand-written multi-field scan already IS one, for a
// single fixed expression shape: its narrowing passes are column-at-a-time bitmap ANDs, and it is
// roughly 10x colScope. Vectorized evaluation would generalize that to arbitrary expression trees.
//
// This spike does NOT build a compiler. It hand-writes the vectorized plan for three shapes and
// measures them against colScope and the row scan on identical data, to size the prize before anyone
// writes an executor. If the hand-vectorized version is not decisively faster than colScope, there is
// nothing to build.

// loadVec reads one numeric column of a block into a float64 vector. This is the operation the whole
// idea rests on: for a HOT column it is a strided walk of uncompressed memory with no decode, and for
// a cold one it is one decompression per block shared by every record.
func loadVec(blk *columnarBlock, fieldIdx int, bc *blockCache, out []float64, valid []bool) ([]float64, []bool, bool) {
	f := blk.schema.fields[fieldIdx]
	if !numericKind(f.kind) {
		return nil, nil, false
	}
	if cap(out) < blk.n {
		out = make([]float64, blk.n)
		valid = make([]bool, blk.n)
	}
	out, valid = out[:blk.n], valid[:blk.n]
	isReal := f.kind == akReal
	var coldNum []byte
	hotOff, hot := blk.hotFieldOff[fieldIdx]
	if !hot {
		start, ok := blk.coldFieldStart[fieldIdx]
		if !ok {
			return nil, nil, false
		}
		raw, err := bc.stream(blk, kindColdNum)
		if err != nil {
			return nil, nil, false
		}
		coldNum = raw[start:]
	}
	esc := blk.schema.escBytes
	for k := 0; k < blk.n; k++ {
		base := k * blk.hotStride
		if testBit(blk.hot[base:base+esc], fieldIdx) {
			valid[k] = false // escaped: a real executor would gather these separately
			continue
		}
		var raw int64
		if hot {
			raw = readIntLE(blk.hot[base+hotOff:], f.width, f.unsigned)
		} else {
			raw = readIntLE(coldNum[k*f.width:], f.width, f.unsigned)
		}
		if isReal {
			out[k] = math.Float64frombits(uint64(raw))
		} else {
			out[k] = float64(raw)
		}
		valid[k] = true
	}
	return out, valid, true
}

// vecCountMemGtCpusTimes512 is the hand-written vectorized plan for
// `RequestMemory > RequestCpus * 512`: load two columns, one fused multiply-and-compare pass, count.
func (c *Collection) vecCountMemGtCpusTimes512(memName, cpusName string, factor float64, fallback *vm.Matcher) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return 0, false
	}
	memID, ok1 := c.intern.LookupID(memName)
	cpusID, ok2 := c.intern.LookupID(cpusName)
	if !ok1 || !ok2 {
		return 0, false
	}
	count := 0
	var memV, cpuV []float64
	var memOK, cpuOK []bool
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			sch := cs.schema()
			if sch == nil {
				// The active segment has no block. Count it the ordinary way instead of abandoning
				// the query: a spike that declines on any live table measures nothing.
				count += c.rowEvalWindow(w, s0, fallback)
				continue
			}
			mi, ok1 := sch.byID[memID]
			ci, ok2 := sch.byID[cpusID]
			if !ok1 || !ok2 {
				releaseWindows(wins)
				return 0, false
			}
			base := 0
			for _, blk := range cs.blocks {
				var ok bool
				memV, memOK, ok = loadVec(blk, mi, st.cache, memV, memOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				cpuV, cpuOK, ok = loadVec(blk, ci, st.cache, cpuV, cpuOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				for k := 0; k < blk.n; k++ {
					gk := base + k
					if gk >= len(cs.offs) {
						break
					}
					o := cs.offs[gk]
					if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
						continue
					}
					if memOK[k] && cpuOK[k] && memV[k] > cpuV[k]*factor {
						count++
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}

// vecCountTwoRanges is `A > a && B >= b`: two loads, two compares, an AND, a count. The shape #174
// already hand-vectorizes, included to check the spike reproduces its speed rather than inventing it.
func (c *Collection) vecCountTwoRanges(aName string, aMin float64, bName string, bMin float64, fallback *vm.Matcher) (int, bool) {
	st := c.schemaScan.Load()
	if st == nil {
		return 0, false
	}
	aID, ok1 := c.intern.LookupID(aName)
	bID, ok2 := c.intern.LookupID(bName)
	if !ok1 || !ok2 {
		return 0, false
	}
	count := 0
	var aV, bV []float64
	var aOK, bOK []bool
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			sch := cs.schema()
			if sch == nil {
				count += c.rowEvalWindow(w, s0, fallback)
				continue
			}
			ai, ok1 := sch.byID[aID]
			bi, ok2 := sch.byID[bID]
			if !ok1 || !ok2 {
				releaseWindows(wins)
				return 0, false
			}
			base := 0
			for _, blk := range cs.blocks {
				var ok bool
				aV, aOK, ok = loadVec(blk, ai, st.cache, aV, aOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				bV, bOK, ok = loadVec(blk, bi, st.cache, bV, bOK)
				if !ok {
					releaseWindows(wins)
					return 0, false
				}
				for k := 0; k < blk.n; k++ {
					gk := base + k
					if gk >= len(cs.offs) {
						break
					}
					o := cs.offs[gk]
					if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
						continue
					}
					if aOK[k] && bOK[k] && aV[k] > aMin && bV[k] >= bMin {
						count++
					}
				}
				base += blk.n
			}
		}
		releaseWindows(wins)
	}
	return count, true
}

// TestVecSpikeAgrees is the precondition for believing any of the timings: the hand-written
// vectorized plans must produce the row path's answer.
func TestVecSpikeAgrees(t *testing.T) {
	c := scopeFixtureCodec(t, 20000)
	defer c.Close()

	aq, err := vm.Parse("RequestMemory > RequestCpus * 512")
	if err != nil {
		t.Fatal(err)
	}
	tq, err := vm.Parse("RequestMemory > 4096 && RequestCpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	arithM, twoM := aq.Matcher(), tq.Matcher()
	got, ok := c.vecCountMemGtCpusTimes512("RequestMemory", "RequestCpus", 512, arithM)
	if !ok {
		t.Fatal("vectorized plan declined")
	}
	if want := rowTruth(t, c, "RequestMemory > RequestCpus * 512"); got != want {
		t.Errorf("vectorized %d != row %d", got, want)
	}
	got2, ok := c.vecCountTwoRanges("RequestMemory", 4096, "RequestCpus", 4, twoM)
	if !ok {
		t.Fatal("vectorized plan declined")
	}
	if want := rowTruth(t, c, "RequestMemory > 4096 && RequestCpus >= 4"); got2 != want {
		t.Errorf("vectorized %d != row %d", got2, want)
	}
	t.Logf("arithmetic=%d  tworanges=%d (both match the row path)", got, got2)
}

// BenchmarkVecSpike sizes the prize: vectorized against colScope (per-record interpretation), the
// hand-written multi-field scan where it applies, and the row scan.
func BenchmarkVecSpike(b *testing.B) {
	c := scopeFixtureCodec(b, 60000)
	defer c.Close()

	// Shape 1: attribute-to-attribute arithmetic. Only colScope serves this today.
	arith, err := vm.Parse("RequestMemory > RequestCpus * 512")
	if err != nil {
		b.Fatal(err)
	}
	arithM := arith.Matcher()
	b.Run("arith/vectorized", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecCountMemGtCpusTimes512("RequestMemory", "RequestCpus", 512, arithM)
		}
	})
	b.Run("arith/colScope", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.ColumnarEvalCount(arith)
		}
	})
	b.Run("arith/rowScan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := 0
			for range c.Query(arith) {
				n++
			}
		}
	})

	// Shape 2: two ranges -- what #174 hand-vectorizes. The spike should land near it, not beyond it.
	two, err := vm.Parse("RequestMemory > 4096 && RequestCpus >= 4")
	if err != nil {
		b.Fatal(err)
	}
	twoM := two.Matcher()
	b.Run("tworanges/vectorized", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.vecCountTwoRanges("RequestMemory", 4096, "RequestCpus", 4, twoM)
		}
	})
	b.Run("tworanges/handwritten174", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.CountQuery(two)
		}
	})
	b.Run("tworanges/colScope", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.ColumnarEvalCount(two)
		}
	})
}
