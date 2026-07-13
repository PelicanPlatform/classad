package collections

import (
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Phase-0 spike (HTCONDOR match perf): does partial-decode (decode only the slot
// Requirements' transitive closure) beat full FromAST on real OSPool slot ads,
// which carry ~568 attributes but reference ~25 in Requirements? Measures the
// decode:eval split so the go/no-go for wire-native slot eval is data-backed.
//
// Needs testdata/ospool_slots.ldif (condor_status -pool cm-1.ospool.osg-htc.org
// -l); the benchmark skips when it is absent, so CI is unaffected.

func loadOSPoolAds(tb testing.TB) ([]*classad.ClassAd, *classad.ClassAd) {
	tb.Helper()
	data, err := os.ReadFile("testdata/ospool_slots.ldif")
	if err != nil {
		tb.Skip("no OSPool testdata (testdata/ospool_slots.ldif): " + err.Error())
	}
	var ads []*classad.ClassAd
	for _, blk := range strings.Split(string(data), "\n\n") {
		if strings.TrimSpace(blk) == "" {
			continue
		}
		ad, err := classad.ParseOld(blk)
		if err != nil {
			continue
		}
		hasReq := false
		for _, a := range ad.AST().Attributes {
			if strings.EqualFold(a.Name, "Requirements") {
				hasReq = true
				break
			}
		}
		if hasReq {
			ads = append(ads, ad)
		}
	}
	if len(ads) == 0 {
		tb.Skip("no parseable OSPool slot ads with Requirements")
	}
	// A representative job supplying the TARGET.* attributes START / WithinResourceLimits read.
	job, err := classad.ParseOld(`ProjectName = "OSGSpike"
RequestCpus = 1
RequestMemory = 1024
RequestDisk = 1048576
RequestGPUs = 0
JobDurationCategory = "Medium"
Requirements = true
Rank = 0`)
	if err != nil {
		tb.Fatalf("job parse: %v", err)
	}
	return ads, job
}

// encodeOSPool encodes every ad to the collection's wire form (interned), returning
// the per-ad wire bytes plus the collection to use as the wire context.
func encodeOSPool(tb testing.TB, ads []*classad.ClassAd) (*Collection, [][]byte) {
	tb.Helper()
	c := New(Options{Shards: 1})
	wires := make([][]byte, len(ads))
	var attrs int
	for i, ad := range ads {
		wires[i] = c.encodeAd(ad.AST())
		attrs += len(ad.AST().Attributes)
	}
	tb.Logf("OSPool spike: %d ads, avg %d attrs/ad", len(ads), attrs/len(ads))
	return c, wires
}

func BenchmarkOSPoolSlotDecode(b *testing.B) {
	ads, job := loadOSPoolAds(b)
	c, wires := encodeOSPool(b, ads)
	seeds := []string{"Requirements"} // partialDecodeWire expands the transitive closure
	m := classad.NewMatchClassAd(job, nil)

	// Report the closure size vs the full ad, once, so the ratio is on the record.
	full0 := c.mustDecode(b, wires[0])
	part0 := partialDecodeWire(c, wires[0], seeds)
	b.Logf("ad[0]: full=%d attrs, Requirements closure=%d attrs",
		len(full0.AST().Attributes), len(part0.AST().Attributes))

	b.Run("FullDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			w := wires[i%len(wires)]
			node, err := c.decodeWire(w)
			if err != nil {
				b.Fatal(err)
			}
			_ = classad.FromAST(node)
		}
	})
	b.Run("PartialDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = partialDecodeWire(c, wires[i%len(wires)], seeds)
		}
	})
	b.Run("FullDecode+Eval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			node, err := c.decodeWire(wires[i%len(wires)])
			if err != nil {
				b.Fatal(err)
			}
			ad := classad.FromAST(node)
			m.ReplaceRightAd(ad)
			_ = m.EvaluateAttrRight("Requirements")
			ad.SetTarget(nil)
		}
	})
	b.Run("PartialDecode+Eval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ad := partialDecodeWire(c, wires[i%len(wires)], seeds)
			m.ReplaceRightAd(ad)
			_ = m.EvaluateAttrRight("Requirements")
			ad.SetTarget(nil)
		}
	})
	// Single-pass closure decode: the informed design. Closure id-set computed once
	// (cached per Requirements in the real impl); per-candidate is one sequential
	// ForEach pass decoding only the closure, skipping the rest.
	want := c.closureIDs(wire.Ad(wires[0]))
	b.Logf("closure id-set size: %d", len(want))
	b.Run("SinglePassDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = c.singlePassClosureDecode(wire.Ad(wires[i%len(wires)]), want)
		}
	})
	b.Run("SinglePassDecode+Eval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ad := c.singlePassClosureDecode(wire.Ad(wires[i%len(wires)]), want)
			m.ReplaceRightAd(ad)
			_ = m.EvaluateAttrRight("Requirements")
			ad.SetTarget(nil)
		}
	})
	b.Run("TwoPassDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = c.twoPassClosureDecode(wire.Ad(wires[i%len(wires)]))
		}
	})
	b.Run("TwoPassDecode+Eval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ad := c.twoPassClosureDecode(wire.Ad(wires[i%len(wires)]))
			m.ReplaceRightAd(ad)
			_ = m.EvaluateAttrRight("Requirements")
			ad.SetTarget(nil)
		}
	})
	// Worker-reused id->bytes map: the map backing is allocated once and cleared per
	// candidate, removing the per-candidate map allocation while staying cache-free.
	b.Run("TwoPassReuse+Eval", func(b *testing.B) {
		b.ReportAllocs()
		nodes := make(map[uint32][]byte, 600)
		for i := 0; i < b.N; i++ {
			ad := c.twoPassClosureDecodeReuse(wire.Ad(wires[i%len(wires)]), nodes)
			m.ReplaceRightAd(ad)
			_ = m.EvaluateAttrRight("Requirements")
			ad.SetTarget(nil)
		}
	})
}

// TestOSPoolSinglePassMatchesFull is the correctness gate for the spike: for every
// real OSPool slot ad, evaluating Requirements on the single-pass closure decode
// must equal evaluating it on the full decode. A mismatch means SelfRefs under-
// captured the closure (a ref the fast path would miss) -- the risk the design
// must clear before it is worth building.
func TestOSPoolSinglePassMatchesFull(t *testing.T) {
	ads, job := loadOSPoolAds(t)
	c, wires := encodeOSPool(t, ads)
	mFull := classad.NewMatchClassAd(job, nil)
	mPart := classad.NewMatchClassAd(job, nil)
	mismatch, checked := 0, 0
	for _, w := range wires {
		want := c.closureIDs(wire.Ad(w)) // per-ad closure (real impl caches by Requirements)
		full := c.mustDecode(t, w)
		part := c.singlePassClosureDecode(wire.Ad(w), want)
		mFull.ReplaceRightAd(full)
		mPart.ReplaceRightAd(part)
		two := c.twoPassClosureDecode(wire.Ad(w)) // cache-free per-ad decode
		mTwo := classad.NewMatchClassAd(job, nil)
		mTwo.ReplaceRightAd(two)
		vf := mFull.EvaluateAttrRight("Requirements")
		vp := mPart.EvaluateAttrRight("Requirements")
		vt := mTwo.EvaluateAttrRight("Requirements")
		full.SetTarget(nil)
		part.SetTarget(nil)
		two.SetTarget(nil)
		checked++
		if vf.String() != vp.String() || vf.String() != vt.String() {
			mismatch++
			if mismatch <= 5 {
				t.Errorf("eval mismatch: full=%q single=%q two=%q", vf.String(), vp.String(), vt.String())
			}
		}
	}
	t.Logf("checked %d ads, %d mismatches", checked, mismatch)
	if mismatch > 0 {
		t.Fatalf("%d/%d ads: single-pass closure eval != full eval", mismatch, checked)
	}
}

// closureIDs computes the transitive self-reference closure of "Requirements" as a
// set of interned ids (done once per distinct Requirements in the real design;
// here once for the benchmark). This is the cached seed set a single-pass decoder
// would filter on.
func (c *Collection) closureIDs(a wire.Ad) map[uint32]bool {
	ids := map[uint32]bool{}
	seen := map[string]bool{}
	work := []string{"Requirements"}
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		fold := strings.ToLower(name)
		if seen[fold] {
			continue
		}
		seen[fold] = true
		id, ok := c.intern.LookupID(name)
		if !ok {
			continue
		}
		node, ok := a.Lookup(id)
		if !ok {
			continue
		}
		ids[id] = true
		if expr, err := c.decodeNode(node); err == nil {
			work = append(work, vm.SelfRefs(expr)...)
		}
	}
	return ids
}

// singlePassClosureDecode builds a ClassAd containing only the closure attributes,
// in ONE sequential pass over the wire ad: decode the wanted nodes, skip the rest
// (ForEach advances past unwanted nodes without building them). This is the design
// the naive partialDecodeWire's scattered O(N) lookups got wrong.
func (c *Collection) singlePassClosureDecode(a wire.Ad, want map[uint32]bool) *classad.ClassAd {
	out := classad.New()
	a.ForEach(func(id uint32, node []byte) bool {
		if want[id] {
			if expr, err := c.decodeNode(node); err == nil {
				if name, ok := c.intern.Name(id); ok {
					out.Insert(name, expr)
				}
			}
		}
		return true
	})
	return out
}

// twoPassClosureDecode is the correctness-safe design that needs no cross-ad cache:
// pass 1 indexes id -> node bytes in one ForEach (no AST build), then a closure BFS
// uses O(1) map lookups (not scattered O(N) Ad.Lookup) and AST-builds only the
// closure. Correct per ad regardless of whether Start/WithinResourceLimits vary.
func (c *Collection) twoPassClosureDecode(a wire.Ad) *classad.ClassAd {
	nodes := make(map[uint32][]byte, 600)
	a.ForEach(func(id uint32, node []byte) bool {
		nodes[id] = node
		return true
	})
	out := classad.New()
	reqID, ok := c.intern.LookupID("Requirements")
	if !ok {
		return out
	}
	seen := map[uint32]bool{}
	work := []uint32{reqID}
	for len(work) > 0 {
		id := work[len(work)-1]
		work = work[:len(work)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		node, ok := nodes[id]
		if !ok {
			continue
		}
		expr, err := c.decodeNode(node)
		if err != nil {
			continue
		}
		if name, ok := c.intern.Name(id); ok {
			out.Insert(name, expr)
		}
		for _, ref := range vm.SelfRefs(expr) {
			if rid, ok := c.intern.LookupID(ref); ok && !seen[rid] {
				work = append(work, rid)
			}
		}
	}
	return out
}

// twoPassClosureDecodeReuse is twoPassClosureDecode with a caller-provided map that
// is cleared and refilled, so the map backing is allocated once per worker.
func (c *Collection) twoPassClosureDecodeReuse(a wire.Ad, nodes map[uint32][]byte) *classad.ClassAd {
	clear(nodes)
	a.ForEach(func(id uint32, node []byte) bool {
		nodes[id] = node
		return true
	})
	out := classad.New()
	reqID, ok := c.intern.LookupID("Requirements")
	if !ok {
		return out
	}
	seen := map[uint32]bool{}
	work := []uint32{reqID}
	for len(work) > 0 {
		id := work[len(work)-1]
		work = work[:len(work)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		node, ok := nodes[id]
		if !ok {
			continue
		}
		expr, err := c.decodeNode(node)
		if err != nil {
			continue
		}
		if name, ok := c.intern.Name(id); ok {
			out.Insert(name, expr)
		}
		for _, ref := range vm.SelfRefs(expr) {
			if rid, ok := c.intern.LookupID(ref); ok && !seen[rid] {
				work = append(work, rid)
			}
		}
	}
	return out
}

// mustDecode is a test helper: full-decode wire bytes to a ClassAd or fail.
func (c *Collection) mustDecode(tb testing.TB, w []byte) *classad.ClassAd {
	tb.Helper()
	node, err := c.decodeWire(w)
	if err != nil {
		tb.Fatal(err)
	}
	return classad.FromAST(node)
}
