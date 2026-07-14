package collections

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
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
	// Encode-time closure layout: hot set = the match closure, read via the hot
	// header (O(closure), no scan, no BFS). The closure walk is paid once at encode.
	hotWires := encodeHotClosure(b, c, ads, wires)
	b.Run("HotClosureDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = c.hotClosureDecode(wire.Ad(hotWires[i%len(hotWires)]))
		}
	})
	b.Run("HotClosureDecode+Eval", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ad := c.hotClosureDecode(wire.Ad(hotWires[i%len(hotWires)]))
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
		// Hot-closure layout: re-encode with the closure as the hot set, read via header.
		hw := wire.EncodeWithHot(nil, c.mustDecode(t, w).AST(), c.intern, idSet(want))
		hot := c.hotClosureDecode(wire.Ad(hw))
		mHot := classad.NewMatchClassAd(job, nil)
		mHot.ReplaceRightAd(hot)
		vf := mFull.EvaluateAttrRight("Requirements")
		vp := mPart.EvaluateAttrRight("Requirements")
		vt := mTwo.EvaluateAttrRight("Requirements")
		vh := mHot.EvaluateAttrRight("Requirements")
		full.SetTarget(nil)
		part.SetTarget(nil)
		two.SetTarget(nil)
		hot.SetTarget(nil)
		checked++
		if vf.String() != vp.String() || vf.String() != vt.String() || vf.String() != vh.String() {
			mismatch++
			if mismatch <= 5 {
				t.Errorf("eval mismatch: full=%q single=%q two=%q hot=%q", vf.String(), vp.String(), vt.String(), vh.String())
			}
		}
	}
	t.Logf("checked %d ads, %d mismatches", checked, mismatch)
	if mismatch > 0 {
		t.Fatalf("%d/%d ads: single-pass closure eval != full eval", mismatch, checked)
	}
}

// BenchmarkOSPoolMatchSorted is the integrated end-to-end measurement: real OSPool
// slot ads in a collection, MatchSorted(job, 1) -- the negotiator pick-best shape --
// with closure decode on (threshold 64, wide ads trigger it) vs off (threshold huge,
// forcing full FromAST per candidate).
func BenchmarkOSPoolMatchSorted(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	load := func(roots []string) *Collection {
		c := New(Options{Shards: 8, MatchClosureRoots: roots})
		for i, ad := range ads {
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), ad); err != nil {
				b.Fatal(err)
			}
		}
		return c
	}
	plain := load(nil)                    // full decode / two-pass
	hot := load([]string{"Requirements"}) // closure in the hot header
	// A simple wire-native job Requirements (so the deferred survivor path engages) and
	// a Rank, matching most slots.
	job, err := classad.ParseOld(`ProjectName = "OSGSpike"
RequestCpus = 1
RequestMemory = 1024
RequestDisk = 1048576
RequestGPUs = 0
Requirements = TARGET.Cpus >= 1
Rank = TARGET.Cpus`)
	if err != nil {
		b.Fatal(err)
	}
	run := func(b *testing.B, c *Collection) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.MatchSorted(job, 1)
		}
	}
	saved := closureDecodeMinAttrs
	defer func() { closureDecodeMinAttrs = saved }()
	b.Run("FullDecode", func(b *testing.B) { closureDecodeMinAttrs = 1 << 30; run(b, plain) })
	b.Run("TwoPassClosure", func(b *testing.B) { closureDecodeMinAttrs = 64; run(b, plain) })
	b.Run("HotClosure", func(b *testing.B) { closureDecodeMinAttrs = 64; run(b, hot) })
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

// TestOSPoolHotHeaderCost measures the storage cost of putting the match closure in
// the hot header: compressed record size with the default (frequency) hot set vs the
// closure hot set. If the delta is noise against the 26 KB ad, a columnar/delta hot
// header is premature; if material, it motivates that layout.
func TestOSPoolHotHeaderCost(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	c, wires := encodeOSPool(t, ads)
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	var rawBase, rawHot, zBase, zHot int
	for i, ad := range ads {
		base := c.encodeAd(ad.AST()) // default hot set
		hot := wire.EncodeWithHot(nil, ad.AST(), c.intern, idSet(c.closureIDs(wire.Ad(wires[i]))))
		rawBase += len(base)
		rawHot += len(hot)
		zBase += len(codec.Compress(nil, base))
		zHot += len(codec.Compress(nil, hot))
	}
	n := len(ads)
	t.Logf("avg/ad  raw: base=%d closure-hot=%d (+%d B)  zstd: base=%d closure-hot=%d (+%d B, %.2f%%)",
		rawBase/n, rawHot/n, (rawHot-rawBase)/n, zBase/n, zHot/n, (zHot-zBase)/n,
		100*float64(zHot-zBase)/float64(zBase))
}

// TestOSPoolHotHeaderLayout compares hot-header encodings for the closure across all
// ads: (A) interleaved (id, absOffset) pairs [current], (B) columnar [ids][absOffsets],
// (C) columnar delta [ids][node-size skips] sorted by (skip,id). Compressed per-record
// and as one dictionary-free concatenated stream (proxy for a shared-dictionary store),
// to quantify how much delta+columnar+sort claws back the offset entropy.
func TestOSPoolHotHeaderLayout(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	c, wires := encodeOSPool(t, ads)
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		id   uint32
		off  uint32 // synthetic contiguous offset (prefix sum of preceding sizes)
		size uint32
	}
	var A, B, Cc [][]byte // per-ad header bytes in each layout
	var catA, catB, catC []byte
	uv := binary.AppendUvarint
	for i := range ads {
		a := wire.Ad(wires[i])
		var es []entry
		var off uint32
		for id := range c.closureIDs(a) {
			node, ok := a.Lookup(id)
			if !ok {
				continue
			}
			es = append(es, entry{id, off, uint32(len(node))})
			off += uint32(len(node))
		}
		sort.Slice(es, func(i, j int) bool { return es[i].id < es[j].id })
		var a1 []byte
		for _, e := range es {
			a1 = uv(uv(a1, uint64(e.id)), uint64(e.off))
		}
		var b1 []byte
		for _, e := range es {
			b1 = uv(b1, uint64(e.id))
		}
		for _, e := range es {
			b1 = uv(b1, uint64(e.off))
		}
		sort.Slice(es, func(i, j int) bool {
			if es[i].size != es[j].size {
				return es[i].size < es[j].size
			}
			return es[i].id < es[j].id
		})
		var c1 []byte
		for _, e := range es {
			c1 = uv(c1, uint64(e.id))
		}
		for _, e := range es {
			c1 = uv(c1, uint64(e.size)) // node-size skip; prefix-sum reconstructs offsets
		}
		A, B, Cc = append(A, a1), append(B, b1), append(Cc, c1)
		catA, catB, catC = append(catA, a1...), append(catB, b1...), append(catC, c1...)
	}
	perRec := func(hs [][]byte) int {
		n := 0
		for _, h := range hs {
			n += len(codec.Compress(nil, h))
		}
		return n
	}
	raw := func(hs [][]byte) int {
		n := 0
		for _, h := range hs {
			n += len(h)
		}
		return n
	}
	n := len(ads)
	t.Logf("per-record zstd avg/ad:  A(interleaved)=%d  B(columnar)=%d  C(columnar+delta+sort)=%d  (raw A=%d)",
		perRec(A)/n, perRec(B)/n, perRec(Cc)/n, raw(A)/n)
	t.Logf("concatenated zstd total: A=%d  B=%d  C=%d  (raw A=%d) -- proxy for shared-dictionary store",
		len(codec.Compress(nil, catA)), len(codec.Compress(nil, catB)), len(codec.Compress(nil, catC)), len(catA))
}

// TestOSPoolPrefixLayoutStorage measures the storage effect of the flagHotClosure
// prefix layout (Phase 1c): the closure as a sorted contiguous prefix with no offset
// pairs, vs the plain layout. Compressed per-record and concatenated (a shared-
// dictionary proxy), since the prefix layout's payoff -- dropping ad-specific offsets
// and stabilizing the byte order across homogeneous ads -- shows up under a dictionary.
// uvlen returns the encoded length of x as a uvarint.
func uvlen(x uint64) int {
	var b [binary.MaxVarintLen64]byte
	return binary.PutUvarint(b[:], x)
}

// TestOSPoolHotHeaderVariants explores hot-header encodings WITHOUT touching the
// entries region (so the node-byte alignment a trained dictionary exploits is
// preserved). It compares several columnar/delta orderings, each header stream
// compressed with a dictionary trained on that variant's streams -- the realistic
// measure. The goal: an encoding whose columns REPEAT across homogeneous ads (so the
// dictionary dedups them), since absolute byte offsets are unique and drift.
func TestOSPoolHotHeaderVariants(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	c, wires := encodeOSPool(t, ads)

	type hotEnt struct{ id, off, idx, size uint32 }
	// Per ad, the hot (closure) entries with byte offset, attr index, and node size.
	perAd := make([][]hotEnt, len(ads))
	for i, w := range wires {
		closure := c.closureIDs(wire.Ad(w))
		var off, idx uint32
		var hs []hotEnt
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			nodeOff := off + uint32(uvlen(uint64(id)))
			if closure[id] {
				hs = append(hs, hotEnt{id, nodeOff, idx, uint32(len(node))})
			}
			off += uint32(uvlen(uint64(id))) + uint32(len(node))
			idx++
			return true
		})
		perAd[i] = hs
	}

	uv := binary.AppendUvarint
	// Each variant builds one ad's header bytes from its hot entries.
	variants := []struct {
		name  string
		build func(hs []hotEnt) []byte
	}{
		{"V0 interleaved (id,off)", func(hs []hotEnt) []byte {
			hs = sortBy(hs, func(a, b hotEnt) bool { return a.id < b.id })
			var o []byte
			for _, h := range hs {
				o = uv(uv(o, uint64(h.id)), uint64(h.off))
			}
			return o
		}},
		{"V1 columnar [ids][offs] id-sort", func(hs []hotEnt) []byte {
			hs = sortBy(hs, func(a, b hotEnt) bool { return a.id < b.id })
			var o []byte
			for _, h := range hs {
				o = uv(o, uint64(h.id))
			}
			for _, h := range hs {
				o = uv(o, uint64(h.off))
			}
			return o
		}},
		{"V2 columnar [ids][off-delta] off-sort", func(hs []hotEnt) []byte {
			hs = sortBy(hs, func(a, b hotEnt) bool { return a.off < b.off })
			var o []byte
			for _, h := range hs {
				o = uv(o, uint64(h.id))
			}
			var prev uint32
			for _, h := range hs {
				o = uv(o, uint64(h.off-prev))
				prev = h.off
			}
			return o
		}},
		{"V3 columnar [ids][index] idx-sort", func(hs []hotEnt) []byte {
			hs = sortBy(hs, func(a, b hotEnt) bool { return a.idx < b.idx })
			var o []byte
			for _, h := range hs {
				o = uv(o, uint64(h.id))
			}
			for _, h := range hs {
				o = uv(o, uint64(h.idx))
			}
			return o
		}},
		{"V4 columnar [ids][index-delta] idx-sort", func(hs []hotEnt) []byte {
			hs = sortBy(hs, func(a, b hotEnt) bool { return a.idx < b.idx })
			var o []byte
			for _, h := range hs {
				o = uv(o, uint64(h.id))
			}
			var prev uint32
			for _, h := range hs {
				o = uv(o, uint64(h.idx-prev))
				prev = h.idx
			}
			return o
		}},
		{"V5 [id-delta][index-delta] id-sort", func(hs []hotEnt) []byte {
			hs = sortBy(hs, func(a, b hotEnt) bool { return a.id < b.id })
			var o []byte
			var pid uint32
			for _, h := range hs {
				o = uv(o, uint64(h.id-pid))
				pid = h.id
			}
			// index in id order (not sorted): store raw indices.
			for _, h := range hs {
				o = uv(o, uint64(h.idx))
			}
			return o
		}},
	}

	for _, v := range variants {
		streams := make([][]byte, len(perAd))
		raw := 0
		for i, hs := range perAd {
			streams[i] = v.build(hs)
			raw += len(streams[i])
		}
		dict, err := TrainDict(streams)
		if err != nil {
			t.Skipf("dict training unavailable: %v", err)
		}
		codec, _ := NewZSTDCodec(dict)
		z := 0
		for _, s := range streams {
			z += len(codec.Compress(nil, s))
		}
		n := len(perAd)
		t.Logf("%-40s raw=%5.1f B/ad  dict-zstd=%5.2f B/ad", v.name, float64(raw)/float64(n), float64(z)/float64(n))
	}
}

// sortBy returns a copy of s sorted by less (small helper to keep variants terse).
func sortBy[T any](s []T, less func(a, b T) bool) []T {
	out := append([]T(nil), s...)
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

// hotClosureDecode reads a ClassAd from the hot header alone -- the design where the
// encoder has already placed the match closure in the hot set, so the reader just
// iterates the hot entries (O(hotCount)) with no scan and no BFS. The transitive
// closure walk happened once at encode time.
func (c *Collection) hotClosureDecode(a wire.Ad) *classad.ClassAd {
	out := classad.New()
	a.ForEachHot(func(id uint32, node []byte) bool {
		if expr, err := c.decodeNode(node); err == nil {
			if name, ok := c.intern.Name(id); ok {
				out.Insert(name, expr)
			}
		}
		return true
	})
	return out
}

// encodeHotClosure re-encodes each ad with its Requirements closure as the hot set,
// so hotClosureDecode reads exactly that closure. Mirrors what a closure-aware
// encoder would do at write time.
func encodeHotClosure(tb testing.TB, c *Collection, ads []*classad.ClassAd, wires [][]byte) [][]byte {
	tb.Helper()
	out := make([][]byte, len(ads))
	for i := range ads {
		want := c.closureIDs(wire.Ad(wires[i]))
		out[i] = wire.EncodeWithHot(nil, ads[i].AST(), c.intern, idSet(want))
	}
	return out
}

func idSet(m map[uint32]bool) map[uint32]struct{} {
	s := make(map[uint32]struct{}, len(m))
	for id := range m {
		s[id] = struct{}{}
	}
	return s
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
