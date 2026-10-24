package collections

import (
	"bytes"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// PROVING THAT NO RECORD DEFINES CurrentTime.
//
// A query that reads CurrentTime unscoped or as MY.CurrentTime pins one time (vm.Compile). An ad
// that does not define an attribute named CurrentTime reads that time; one that does reads its own
// value, undefined included. So the query with CurrentTime folded to the pinned time
// (vm.Query.Folded) is exact over every record that lacks the attribute, and only there. The
// folded form is what the fast paths need: `CompletionDate > <literal>` is a probe, a column
// comparison, a range a zone map can decide; `CompletionDate > CurrentTime - 3600` is none of
// those.
//
// So each segment carries a proof that none of its records names CurrentTime, and a scan uses the
// folded query only in a window whose segment is proven, the original everywhere else. The proof
// comes from wherever it is cheapest:
//
//   - an interned segment: its attribute dictionary. Every name a record of the segment can carry
//     -- remnant, cold tail, base and group columns -- is keyed through it (a reassembled field the
//     dictionary lacks fails the read), so a name the dictionary lacks is a name no record has. The
//     dictionary also holds the names records only REFER to, and job ads refer to CurrentTime in
//     their expressions, so a dictionary that has the name sends the segment to a walk;
//   - a segment this process wrote to: the write path checks every ad it appends and marks the
//     segment when one names CurrentTime (track, defines);
//   - a sealed segment's sidecar: its key index carries the proof (kidxCTChecked/kidxCTDefines),
//     written at seal from the in-memory state;
//   - otherwise a walk of the records (proveSegCT), run lazily the first time a query needs it.
//
// The proof is per segment, but a record's ad is not always its own bytes. A delta record merges
// over a base in an older segment, and a chained child reads its parent's attributes. Both stay
// within one shard. So in a mutable or chained collection a window counts as proven only when every
// segment of its shard is (see buildWindowsLocked); an append-only collection, which has neither,
// decides per segment.

// The CurrentTime proof bits in a key index's flags field (keyindex.go). A build that predates them
// wrote zero there and ignores the field, so the bits are optional in both directions: zero means
// "not checked", never "clean".
const (
	kidxCTChecked uint16 = 1 << 0 // every record in [0, upto) was checked for an attribute named CurrentTime
	kidxCTDefines uint16 = 1 << 1 // and at least one names it
)

// ctQueryProofBudget bounds how many segment bytes one query walks to prove segments
// (ensureCTProofs). A store opened from sidecars that predate the proof has every segment to walk,
// and a columnarized one is reassembled record by record to do it; doing it all in the first query
// would put the whole store's decompression in front of it (measured at 9.5 s on a 400k-record
// archive). Segments past the budget stay unproven, which is correct and only slower, and the next
// query, or Reindex (ctReindexProofBudget), continues. One segment is always walked, however large.
const ctQueryProofBudget = 32 << 20

// ctReindexProofBudget is ctQueryProofBudget for a Reindex pass, which runs off the query path.
const ctReindexProofBudget = 1 << 30

// segCT is a segment's CurrentTime proof state. A segment is proven over [0, used) when no record
// names CurrentTime and every record below used was checked: below provedTo by a walk, the
// dictionary or the sidecar, and from track-1 on by the write path.
type segCT struct {
	// defines is set when some record of the segment names CurrentTime. Sticky.
	defines atomic.Bool
	// provedTo is the extent [0, provedTo) known to be checked. Only grows.
	provedTo atomic.Int64
	// track is 1 + the offset from which the write path checked every record it appended; 0 when
	// this process never wrote to the segment. Set once, under the shard write lock, by the first
	// write (writeRecord), so it covers every later append.
	track atomic.Int64
	// persisted is set once the segment's sidecar records the proof, so Reindex need not write it.
	persisted atomic.Bool
	// failed is set when a walk could not read a record, so later queries do not retry it.
	failed atomic.Bool
	// opened marks a segment Open loaded rather than one this process wrote or rewrote. Reindex
	// leaves an unproven one to the queries (see persistCTProofs).
	opened atomic.Bool
	// mu serializes walks of the segment.
	mu sync.Mutex
}

// ctProven reports whether no record in [0, used) of the segment names CurrentTime.
//
// provedTo and track are read before defines: a walk that finds CurrentTime sets defines and never
// advances provedTo, so a reader that sees the advanced extent also sees every defines set before it.
func (s *segment) ctProven(used int) bool {
	p := s.ct.provedTo.Load()
	ok := p >= int64(used)
	if !ok {
		if t := s.ct.track.Load(); t > 0 && p >= t-1 {
			ok = true
		}
	}
	if !ok && !s.ct.defines.Load() {
		ok = s.ctDictProof(used)
	}
	return ok && !s.ct.defines.Load()
}

// ctDictProof proves an interned segment from its dictionary when the dictionary lacks the name,
// recording the answer. A dictionary segment is sealed (it never takes appends), so its dictionary
// covers every record. A dictionary that HAS the name proves nothing either way: it also holds the
// names records only refer to, and job ads commonly refer to CurrentTime in their expressions
// (PeriodicRemove, for one). Such a segment is walked (proveSegCT).
func (s *segment) ctDictProof(used int) bool {
	d := s.dict.Load()
	if d == nil || s.dictUnusable.Load() {
		return false
	}
	if _, has := d.lookup(currentTimeName); has {
		return false
	}
	s.ctAdvance(used)
	return true
}

// ctAdvance records that [0, to) was checked and found clean.
func (s *segment) ctAdvance(to int) {
	for {
		p := s.ct.provedTo.Load()
		if p >= int64(to) || s.ct.provedTo.CompareAndSwap(p, int64(to)) {
			return
		}
	}
}

// ctKnown reports whether the segment's state over [0, used) is settled either way.
func (s *segment) ctKnown(used int) bool {
	return s.ct.defines.Load() || s.ctProven(used)
}

// ctKIDXFlags is the proof to record in the key index of a sidecar written over [0, used).
func (s *segment) ctKIDXFlags(used int) uint16 {
	if s.ct.defines.Load() {
		return kidxCTChecked | kidxCTDefines
	}
	if s.ctProven(used) {
		return kidxCTChecked
	}
	return 0
}

// ctAdoptKIDX takes the proof a sidecar's key index carries. The index was adopted for exactly this
// segment's extent (upto == used), so the proof covers every record.
func (s *segment) ctAdoptKIDX(ki *mmapKeyIndex) {
	if ki == nil || ki.flags&kidxCTChecked == 0 {
		return
	}
	if ki.flags&kidxCTDefines != 0 {
		s.ct.defines.Store(true)
	} else {
		s.ctAdvance(int(ki.upto))
	}
	s.ct.persisted.Store(true)
}

// ctNoteWrite is the write path's half of the proof: called under the shard write lock for every ad
// appended to the active segment, BEFORE it is appended, with whether the ad names CurrentTime.
func (s *segment) ctNoteWrite(names bool) {
	if s.ct.track.Load() == 0 {
		s.ct.track.Store(int64(s.used) + 1)
	}
	if names {
		s.ct.defines.Store(true)
	}
}

// currentTimeName is the magic attribute's name, for lookups that fold case.
const currentTimeName = "CurrentTime"

// namesCurrentTime reports whether a record's wire ad has a top-level attribute named CurrentTime,
// in any case. Inline names are compared directly; a globally interned record (an in-memory
// collection) holds an id, which exists only if some ad named the attribute when it was encoded.
// A record of an interned segment is checked by namesCurrentTimeIn.
//
// It runs on every write, so the inline lookup -- a walk over every entry -- is gated by a byte
// scan: an inline name is stored as its length (one byte, 11) followed by its bytes, so an ad whose
// bytes nowhere hold that byte followed by the name, in any case, cannot have the attribute. On a
// 164-attribute job ad that is a small fraction of the walk it saves.
func (c *Collection) namesCurrentTime(w []byte) bool {
	a := wire.Ad(w)
	if mayHoldInlineName(w, currentTimeName) {
		if _, ok := a.LookupByName(currentTimeName); ok {
			return true
		}
	}
	if c.intern != nil {
		if id, ok := c.intern.LookupID(currentTimeName); ok {
			if _, ok := a.Lookup(id); ok {
				return true
			}
		}
	}
	return false
}

// namesCurrentTimeIn is namesCurrentTime for a record of a segment with dictionary d, whose
// records key attributes by d's local ids (a delta record keeps inline names).
func namesCurrentTimeIn(w []byte, d *segDictHandle) bool {
	a := wire.Ad(w)
	if mayHoldInlineName(w, currentTimeName) {
		if _, ok := a.LookupByName(currentTimeName); ok {
			return true
		}
	}
	lid, ok := d.lookup(currentTimeName)
	if !ok {
		return false
	}
	_, has := a.Lookup(lid)
	return has
}

// mayHoldInlineName reports whether w could hold name as an inline attribute name: the name's length
// as a one-byte uvarint, then the name, compared without case. False is definite.
func mayHoldInlineName(w []byte, name string) bool {
	n := len(name)
	for i := 0; ; {
		j := bytes.IndexByte(w[i:], byte(n))
		if j < 0 {
			return false
		}
		i += j + 1
		if i+n <= len(w) && foldEqualASCII(w[i:i+n], name) {
			return true
		}
	}
}

// foldEqualASCII compares b and s ignoring ASCII case, as attribute names compare.
func foldEqualASCII(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range len(b) {
		x, y := b[i], s[i]
		if x|0x20 != y|0x20 || (x|0x20) < 'a' || (x|0x20) > 'z' {
			if x != y {
				return false
			}
		}
	}
	return true
}

// proveSegCT walks [0, upto) of a segment and records whether any record names CurrentTime. It
// reads full records (recordWireIn reassembles a columnarized one), skips damaged ranges and
// markers, and gives up -- recording nothing but the failure -- on a record it cannot read. The
// caller pins the segment.
func (c *Collection) proveSegCT(seg *segment, upto int) {
	if seg.ctDictProof(upto) {
		return
	}
	d := seg.dict.Load()
	var buf []byte
	dm := seg.damaged()
	for off := 0; off < upto; {
		o := uint32(off)
		if end, ok := dm.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		off += int(total)
		if recIsMarker(seg.data, o) {
			continue
		}
		w, err := c.recordWireIn(seg, seg.data, o, buf)
		if err != nil {
			seg.ct.failed.Store(true)
			return
		}
		buf = w
		names := false
		if d != nil {
			names = namesCurrentTimeIn(w, d)
		} else {
			names = c.namesCurrentTime(w)
		}
		if names {
			seg.ct.defines.Store(true)
			return
		}
	}
	seg.ctAdvance(upto)
}

// ensureCTProofs proves, within budget bytes, the segments a CurrentTime query would find
// unproven: a sealed segment read from a sidecar that predates the proof, the segment recovery made
// the append target (its records before this process's first write), and a rewrite's output that is
// not interned. Segments are pinned while walked, since a compaction may retire them meanwhile.
//
// opened includes segments Open loaded (see persistCTProofs).
func (c *Collection) ensureCTProofs(opened bool, budget int) {
	for _, sh := range c.shards {
		if budget <= 0 {
			return
		}
		type target struct {
			seg  *segment
			upto int
		}
		var tgts []target
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 || seg.ct.failed.Load() || seg.ctKnown(seg.used) ||
				(!opened && seg.ct.opened.Load()) {
				continue
			}
			// The write path checks everything from track-1 on; only the prefix needs a walk.
			upto := seg.used
			if t := seg.ct.track.Load(); t > 0 {
				upto = int(t - 1)
			}
			if budget <= 0 {
				break
			}
			budget -= upto
			seg.pin()
			tgts = append(tgts, target{seg, upto})
		}
		sh.mu.RUnlock()
		for _, t := range tgts {
			t.seg.ct.mu.Lock()
			if !t.seg.ct.failed.Load() && !t.seg.ctKnown(t.upto) {
				c.proveSegCT(t.seg, t.upto)
			}
			t.seg.ct.mu.Unlock()
			t.seg.unpin()
		}
	}
}

// foldFor returns q's CurrentTime-folded form (vm.Query.Folded), or nil when q has none, after
// proving what can be proven so the folded form has windows to run in. Every path that uses the
// folded form must use it only where segWindow.ctFree holds, and q everywhere else.
func (c *Collection) foldFor(q *vm.Query) *vm.Query {
	qf := q.Folded()
	if qf != nil {
		c.ctQueried.Store(true)
		c.ensureCTProofs(true, ctQueryProofBudget)
	}
	return qf
}

// ctAllProven reports whether every segment the shards hold is proven. It is a hint for the paths
// that answer only when every window is proven: checking first spares a scan that would be thrown
// away. The scan still checks each window it reads, since a write may land in between.
func (c *Collection) ctAllProven() bool {
	for _, sh := range c.shards {
		sh.mu.RLock()
		ok := sh.ctShardProvenLocked()
		sh.mu.RUnlock()
		if !ok {
			return false
		}
	}
	return true
}

// ctShardProvenLocked reports whether every segment of the shard is proven. Caller holds the shard
// lock.
func (sh *shard) ctShardProvenLocked() bool {
	for _, seg := range sh.segs {
		if seg != nil && seg.used > 0 && !seg.ctProven(seg.used) {
			return false
		}
	}
	return true
}

// ctPerSegment reports whether a window's proof may be decided from its own segment alone: an
// append-only, unchained shard has no delta records and no parents, so a record's ad is its own
// bytes. Caller holds the shard lock.
func (sh *shard) ctPerSegment() bool {
	return sh.appendOnly && sh.childParentHash == nil
}

// segKeyIndex builds a sealed segment's key index for its sidecar, carrying the segment's
// CurrentTime proof as far as it is known. It never walks the records for it: an unknown proof is
// written as unknown and proven later (ensureCTProofs, then Reindex persists it).
func (c *Collection) segKeyIndex(seg *segment) []byte {
	flags := seg.ctKIDXFlags(seg.used)
	return buildKeyIndex(seg.data, seg.used, c.h, seg.damaged(), flags)
}

// persistCTProofs walks the segments a CurrentTime query would find unproven (ensureCTProofs, within
// ctReindexProofBudget) and writes into each sealed segment's sidecar a proof known in memory but not
// on disk, so the next open adopts it instead of walking the segment again. Reindex runs it, and
// compaction, merges and maintenance call Reindex after rewriting segments, so a query rarely finds
// a rewrite's output to walk. A segment whose dictionary lacks the name is not written: the
// dictionary proves it again at no cost. The rewrite keeps every other section as it is; this
// process's mapping of the old file stays valid, since the proof is already in memory. Caller holds
// reindexMu, which serializes sidecar rewrites.
//
// The segments Open loaded without a proof (sidecars that predate it) are walked only once this
// process has run a CurrentTime query. Open itself runs Reindex, and walking the whole store there
// would put its decompression in front of every restart after an upgrade, for stores that may never
// be asked about CurrentTime.
func (c *Collection) persistCTProofs() {
	c.ensureCTProofs(c.ctQueried.Load(), ctReindexProofBudget)
	for _, sh := range c.shards {
		segs := sh.pinSealed(func(seg *segment) bool {
			if !seg.persistent || seg.keyIdx.Load() == nil || seg.ct.persisted.Load() || !seg.ctKnown(seg.used) {
				return false
			}
			d := seg.dict.Load()
			if d == nil {
				return true
			}
			_, has := d.lookup(currentTimeName)
			return has
		})
		for _, seg := range segs {
			if c.maintStop.Load() {
				break
			}
			c.persistCTProof(seg)
		}
		unpinAll(segs)
	}
}

// persistCTProof rewrites one segment's sidecar with its proof (see persistCTProofs).
func (c *Collection) persistCTProof(seg *segment) {
	path := snapshotPath(seg)
	if path == "" {
		return
	}
	data, closer, err := mapFile(path)
	if err != nil {
		return
	}
	attr, key, col, zone, ok := splitSegmentSidecar(data)
	if !ok {
		_ = closer()
		return
	}
	if ki, err := parseKeyIndex(key); err != nil || int(ki.upto) != seg.used {
		_ = closer() // not this segment's index: leave it for a reindex to rebuild
		return
	}
	container := buildSegmentSidecar(append([]byte(nil), attr...), c.segKeyIndex(seg), append([]byte(nil), col...),
		append([]byte(nil), zone...), seg.used, seg.dictRecOff(), encodeDamage(seg.damaged()))
	_ = closer()
	if writeFileAtomic(path, container) == nil {
		seg.ct.persisted.Store(true)
	}
}

// evalSide is what a column scan needs to evaluate one query in a window: the query, a matcher for
// the per-record column resolver, the row fallback, and the block pruning derived from its probes.
type evalSide struct {
	q    *vm.Query
	m    *vm.Matcher
	rm   *rowMatch
	plan prunePlan
	// prune is false when q's probes fold CurrentTime: they hold only for records without it, and
	// this side runs in windows not proven free of it.
	prune bool
}

// evalSides pairs a query with its CurrentTime-folded form for a column scan; at picks one per window.
type evalSides struct {
	orig evalSide
	fold *evalSide // nil when the query has no folded form
}

func (c *Collection) newEvalSides(q *vm.Query) *evalSides {
	s := &evalSides{orig: evalSide{q: q, m: q.Matcher(), rm: c.newRowMatch(q), prune: !q.ReadsCurrentTime()}}
	if qf := c.foldFor(q); qf != nil && qf.Native() {
		s.fold = &evalSide{q: qf, m: qf.Matcher(), rm: c.newRowMatch(qf), prune: true}
	}
	return s
}

// at returns the side to evaluate in w: the folded form where w is proven free of CurrentTime.
func (s *evalSides) at(w segWindow) *evalSide {
	if s.fold != nil && w.ctFree {
		return s.fold
	}
	return &s.orig
}

// tests is prunePlan.tests for this side, or nothing when its probes may not prune.
func (sd *evalSide) tests(c *Collection, sch *adSchema) ([]int, []fieldPred) {
	if !sd.prune {
		return nil, nil
	}
	return sd.plan.tests(c, sd.q, sch)
}

// stringEq is prunePlan.stringEq for this side, or nothing when its probes may not prune.
func (sd *evalSide) stringEq(c *Collection, sch *adSchema) []strProbe {
	if !sd.prune {
		return nil
	}
	return sd.plan.stringEq(c, sd.q, sch)
}

// ctUnprovenSegments counts the segments a scan would find unproven right now (see
// buildWindowsLocked), for ExplainQuery. It proves nothing.
func (c *Collection) ctUnprovenSegments() int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		shardFree := sh.ctPerSegment() || sh.ctShardProvenLocked()
		for _, seg := range sh.segs {
			if seg != nil && seg.used > 0 && (!shardFree || !seg.ctProven(seg.used)) {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}
