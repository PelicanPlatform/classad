package collections

import (
	"strings"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Building a columnarized segment.
//
// Modelled on the interning reseal, which already rewrites a sealed segment in place: a new file is
// built holding every record re-encoded, sized exactly to its contents, and swapped in under the
// shard lock with seq, key and time markers carried across so watch cursors stay valid. The only
// new part is what "re-encoded" means -- each record loses the attributes the schema covers, and
// one columnar record at the end gains them.
//
// A record's remnant is what the schema does NOT carry. That is deliberately the SAME split the
// columnar block already makes: the block's fixed slots and string region hold the schema'd values,
// its cold tail holds the rest. Today the cold tail duplicates the remnant; here the remnant IS the
// cold tail's content and the block stops carrying it.

// columnarizeSegment rewrites one sealed segment into columnar form and returns the replacement, or
// nil if it cannot be built (in which case the caller keeps the original -- this is an optimization,
// never a correctness dependency).
//
// The source must be sealed (immutable), so it is read without the shard lock; the returned segment
// is staged on disk and not yet part of the shard -- see columnarizeSealedSegment.
func (c *Collection) columnarizeSegment(sh *shard, src *segment, s *adSchema, hot []int) (*segment, []uint32, []uint32) {
	if src == nil || src.used == 0 || s == nil || len(s.fields) == 0 {
		return nil, nil, nil
	}
	// recol re-columnarizes a segment that ALREADY holds a columnar payload, to bring its group set
	// current after the committed groups changed (see refreshGroupSchemas). Its records are remnants,
	// so its full ads are reconstructed (remnant spliced with the existing columns) before being
	// re-split under the new group set. A colDamaged segment cannot be reconstructed -- its records
	// are short of every attribute the unreadable payload held -- so it is refused rather than rewritten
	// into ads missing half their content.
	recol := src.columnarized()
	if src.colDamaged.Load() {
		return nil, nil, nil
	}
	var recolOffs []uint32
	var recolFulls [][]byte
	if recol {
		var ok bool
		recolOffs, recolFulls, ok = c.reconstructFullRecords(src)
		if !ok || len(recolOffs) == 0 {
			return nil, nil, nil
		}
	}

	d := src.dict.Load()

	// Whether the schema carries an attribute, by whichever key the record uses. Built once:
	// resolving a name through the intern table per attribute per record would dominate.
	byName := make(map[string]struct{}, len(s.fields))
	for _, f := range s.fields {
		if n, ok := c.intern.Name(f.id); ok {
			byName[strings.ToLower(n)] = struct{}{}
		}
	}
	// Everything the block can store moves into it, whether or not the segment is interned.
	//
	// This was restricted to interned segments while a cold tail could only be keyed durably by a
	// segment dictionary -- doing it without one lost 1125 of 3000 records after a restart, because
	// global intern ids are renumbered at every Open. A payload with no dictionary now carries a NAME
	// for each cold-tail id in its section, so both shapes survive a restart and the restriction is
	// gone. Removing that naming fails five tests, which is what keeps this honest.

	// The GROUP schemas take part too, and they have to: they are built into the same payload, and a
	// rewrite that left them out silently retired the group accelerator for every segment it touched
	// (measured: 0 of 9). Their attributes are not base schema fields, so without them the group
	// columns simply do not exist and every group attribute stays in the record.
	var groups []*colGroup
	if st := c.schemaScan.Load(); st != nil {
		groups = st.groups
	}
	// A record's group-owned attributes, captured while the block builder walks the records so the
	// remnant pass strips exactly what the columns took. groupSkipSet admits only WHOLE membership --
	// a record holding part of a group is not in that group's column, so its values must stay in the
	// record, which is where the exception path already looks for them.
	skips := make([]map[uint32]struct{}, 0, 512)

	// The block sees only what the schema carries, and the remnant only what it does not. Without
	// that split the block's COLD TAIL would hold the non-schema attributes -- which is precisely
	// what the remnant holds -- and every such attribute would be stored twice again, which is the
	// duplication this format exists to remove. What legitimately stays in the block's cold tail is
	// a SCHEMA field whose value did not fit its slot.
	toInterned := func(dst, w []byte) ([]byte, bool) {
		iw, ok := c.recordToInternedDict(d, dst, w)
		if !ok {
			return nil, false
		}
		skip := groupSkipSet(groups, iw)
		skips = append(skips, skip)
		// EVERYTHING the block can store, not just the schema's fields. An attribute the schema does
		// not carry lands in the block's COLD TAIL, which is one compressed region per ROW GROUP -- and
		// that is the whole point of moving it: those attributes are the majority of a segment, and a
		// record's own bytes cannot see the record before them. Measured on real OSPool ads, the same
		// leftover bytes cost 17.7% of what per-record compression needs at a 128 KiB row group.
		//
		// Group-owned attributes come along too, so the builder can put them in their group's column;
		// it leaves those out of the base cold tail itself (groupSkipSet), so nothing is stored twice.
		only, ok := keepAttrs(iw, func(id uint32, name string, node []byte) bool {
			if !storableInColumn(node) {
				return false
			}
			return true
		})
		return only, ok
	}

	// Pass 1: build the columnar blocks over the segment's records, exactly as the accelerator
	// does today. This also yields the per-record arena offsets the reader maps back through. When
	// re-columnarizing, the records are fed as reconstructed full ads rather than decompressed from
	// the arena (their arena bytes are only the remnant); colBuild processes both identically.
	var blocks []*columnarBlock
	var gblocks [][]*colGroupBlock
	var offs []uint32
	if recol {
		blocks, gblocks, offs = buildColumnarFromWires(recolOffs, recolFulls,
			c.regionCodec(), s, hot, groups, c.colGrouping(), toInterned, c.segIDMapper(d))
	} else {
		blocks, gblocks, offs = buildColumnarFromSegmentGrouped(src.data, src.used, src.codec,
			c.regionCodec(), s, hot, groups, c.colGrouping(), toInterned, c.segIDMapper(d))
	}
	if len(blocks) == 0 || len(offs) == 0 {
		return nil, nil, nil
	}
	if len(skips) != len(offs) {
		// The remnant pass indexes skips by record position, so the two passes must have seen the
		// same records in the same order. Refuse rather than strip by a misaligned index, which
		// would remove one record's attributes on the strength of another's membership.
		return nil, nil, nil
	}
	cs := &colSegment{blocks: blocks, offsB: packU32s(offs), dictKeyed: d != nil}
	// Re-key the pinned groups onto this segment's selections: the schema and members are shared, the
	// per-block bitmaps are not.
	for gi, g := range groups {
		sel := &colGroup{schema: g.schema, ids: g.ids}
		for bi := range blocks {
			if gi < len(gblocks[bi]) {
				sel.blocks = append(sel.blocks, gblocks[bi][gi])
			}
		}
		if len(sel.blocks) == len(blocks) {
			cs.groups = append(cs.groups, sel)
		}
	}
	blob := marshalColSegment(cs, func(id uint32) (string, bool) { return c.intern.Name(id) })
	if blob == nil {
		return nil, nil, nil
	}

	// Pass 2: the remnants. Each record keeps only what the schema does not cover.
	type remnant struct {
		off  uint32
		body []byte
	}
	rems := make([]remnant, 0, len(offs))
	var buf []byte
	total := 0
	// When re-columnarizing, the arena holds only each record's remnant; its full ad was
	// reconstructed for pass 1 and is reused here so the new remnant is split from the same content.
	var fullByOff map[uint32][]byte
	if recol {
		fullByOff = make(map[uint32][]byte, len(recolOffs))
		for i, o := range recolOffs {
			fullByOff[o] = recolFulls[i]
		}
	}
	for _, o := range offs {
		var raw []byte
		if recol {
			raw = fullByOff[o]
			if raw == nil {
				return nil, nil, nil
			}
		} else {
			r, err := src.codec.Decompress(buf[:0], recAd(src.data, o))
			if err != nil {
				return nil, nil, nil
			}
			buf = r
			raw = r
		}
		// What the columns could not take: expressions, and nothing else. An expression node carries
		// attribute references as ids in the RECORD's space, and the block's nodes are re-encoded into
		// the global one, so moving an expression rewrites which attribute it points at -- measured,
		// `RequestMemory = ProcId * 512 + 7` came back as `Slack * 512 + 7`. So a record is now usually
		// just a header and a key.
		body, ok := stripSchemaAttrs(raw, func(id uint32, name string, node []byte) bool {
			if !storableInColumn(node) {
				return false
			}
			return true
		})
		if !ok {
			return nil, nil, nil
		}
		enc := src.codec.Compress(nil, body)
		rems = append(rems, remnant{off: o, body: enc})
		total += recordLen(len(recKey(src.data, o)), len(enc))
	}
	total += recordLen(0, len(blob)) + recHeaderSize
	if b := segDictBlob(src); b != nil {
		total += recordLen(0, len(b))
	}

	// Staged under the prefix recovery ignores: until commitSegmentRewrite renames it, a crash
	// must leave the original segment as the one the directory names.
	dst, err := sh.allocNamed(src.id, total, src.codec, mergeTmpPrefix)
	if err != nil {
		return nil, nil, nil
	}
	// Records first, in order, carrying their original seq/superseded/key so nothing about
	// visibility, ordering or watch changes.
	newOffs := make([]uint32, 0, len(rems))
	srcOffs := make([]uint32, 0, len(rems))
	for _, r := range rems {
		srcOffs = append(srcOffs, r.off)
		no, ok := dst.appendRawRecord(src.data, r.off, r.body)
		if !ok {
			dst.retire()
			dst.reapAndHook()
			return nil, nil, nil
		}
		newOffs = append(newOffs, no)
	}
	// An interned segment's records key their attributes by ids that only mean anything against
	// that segment's dictionary, and the remnants were copied verbatim -- so the dictionary has to
	// come with them. Without it the rewritten records are unreadable, which is the kind of failure
	// that shows up as every record mismatching rather than as an error.
	if d != nil {
		if blob := segDictBlob(src); blob != nil {
			if doff, ok := dst.appendDict(blob); ok {
				dst.dict.Store(&segDictHandle{data: dst.data, base: doff + recKeyOff + 4, rec: doff})
			} else {
				dst.retire()
				dst.reapAndHook()
				return nil, nil, nil
			}
		}
	}
	// The offsets moved, so the columnar payload must describe the NEW ones or a reader would map
	// a record to another record's columns.
	cs.offsB = packU32s(newOffs)
	blob = marshalColSegment(cs, func(id uint32) (string, bool) { return c.intern.Name(id) })
	if blob == nil || len(blob) == 0 {
		dst.retire()
		dst.reapAndHook()
		return nil, nil, nil
	}
	if _, ok := dst.appendCol(blob); !ok {
		dst.retire()
		dst.reapAndHook()
		return nil, nil, nil
	}
	if err := dst.syncAll(); err != nil {
		dst.retire()
		dst.reapAndHook()
		return nil, nil, nil
	}
	// The zone map is derived from the records, and the rewritten segment holds the same records
	// with the same values -- so it is carried over rather than recomputed. Dropping it would cost
	// the archive its segment pruning until the next reindex.
	dst.zones = src.zones
	publishColNative(c, dst)
	// Attach dst's key index + Bloom over its NEW offsets, from the same immutable bytes the deferred
	// reindex would use (buildKeyIndex over the whole segment; the dict/col records it also scans never
	// match a real key). Built here, before the swap, so columnarizeSealedSegment can PUBLISH it on dst
	// the instant dst becomes visible -- closing the window in which dst was reachable with keyIdx==nil,
	// where lookupSealed missed a record a scan could still see (put/del could not supersede it -> a
	// duplicate live version, and DeleteWhere never converged). dst is not yet visible, so storing it now
	// is safe; the publish/evict under the swap lock is what makes it authoritative.
	ki, kerr := parseKeyIndex(buildKeyIndex(dst.data, dst.used, c.h))
	if !dst.columnarized() || kerr != nil {
		dst.retire()
		dst.reapAndHook()
		return nil, nil, nil // the payload did not read back (or its key index would not parse): refuse
	}
	dst.keyIdx.Store(ki)
	dst.keyBloom.Store(bloomFromKeyIndex(ki))
	return dst, srcOffs, newOffs
}

// reconstructFullRecords rebuilds every live data record of an already-columnarized segment into its
// FULL wire ad (remnant spliced with the segment's columnar values), in arena order, returning each
// record's arena offset alongside its ad. It is what lets a columnarized segment be re-columnarized
// under a new group set: the source records are remnants, so they must be made whole before being
// re-split. Reads immutable sealed bytes off the shard lock, like the rest of the build. ok=false on
// any reconstruction error, so the caller keeps the original segment rather than rewriting it into
// short ads.
func (c *Collection) reconstructFullRecords(src *segment) ([]uint32, [][]byte, bool) {
	var offs []uint32
	var fulls [][]byte
	var buf []byte
	for off := 0; off < src.used; {
		o := uint32(off)
		total := recTotalLen(src.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(src.data, o) {
			full, err := c.recordWireIn(src, src.data, o, buf[:0])
			if err != nil {
				return nil, nil, false
			}
			offs = append(offs, o)
			fulls = append(fulls, append([]byte(nil), full...))
			buf = full[:0]
		}
		off += int(total)
	}
	return offs, fulls, true
}

// stripSchemaAttrs returns the ad with every attribute the schema carries removed.
//
// Removed entirely, not moved to a cold tail: the columnar record holds them now, and the whole
// point is that each value is stored once.
func stripSchemaAttrs(w []byte, covered func(id uint32, name string, node []byte) bool) ([]byte, bool) {
	hdr, _, _, inline, ok := wire.Ad(w).SplitBody()
	if !ok {
		return nil, false
	}
	var entries []byte
	kept := 0
	bad := false
	wire.Ad(w).ForEachRaw(func(id uint32, name string, node []byte) bool {
		if covered(id, name, node) {
			return true // the column holds it now
		}
		entries = wire.AppendKey(entries, inline, id, name)
		entries = append(entries, node...)
		kept++
		return true
	})
	if bad {
		return nil, false
	}
	return wire.BuildAd(nil, hdr, kept, entries), true
}

// keepAttrs is stripSchemaAttrs inverted: it returns the ad with ONLY the attributes the predicate
// accepts. Used to feed the columnar builder the schema's half of each record.
func keepAttrs(w []byte, keep func(id uint32, name string, node []byte) bool) ([]byte, bool) {
	hdr, _, _, inline, ok := wire.Ad(w).SplitBody()
	if !ok {
		return nil, false
	}
	var entries []byte
	n := 0
	wire.Ad(w).ForEachRaw(func(id uint32, name string, node []byte) bool {
		if !keep(id, name, node) {
			return true
		}
		entries = wire.AppendKey(entries, inline, id, name)
		entries = append(entries, node...)
		n++
		return true
	})
	return wire.BuildAd(nil, hdr, n, entries), true
}

// segDictBlob returns a segment's serialized attribute dictionary, or nil if it has none.
func segDictBlob(s *segment) []byte {
	for off := 0; off < s.used; {
		o := uint32(off)
		rl := recTotalLen(s.data, o)
		if rl == 0 {
			return nil
		}
		if recIsDict(s.data, o) {
			return recAd(s.data, o)
		}
		off += int(rl)
	}
	return nil
}

// colCommitStallHook is a test seam: called after the rewrite is BUILT and before it is committed.
// That window is the whole reason reconcile exists -- the build read the source off the shard lock,
// so a supersede landing here is recorded on the source and absent from the output. It lets a test
// delete a record at exactly that instant, which is otherwise a race a test cannot hit reliably.
// Production leaves it nil.
var colCommitStallHook func()

// columnarizeSealedSegment rewrites one sealed segment into columnar form and makes the replacement
// durable, returning whether it happened.
//
// Split from columnarizeSegment because the two halves have different failure meanings: building can
// fail freely and leaves nothing behind, while committing is the point where the new file becomes
// the one recovery names and the old one goes away. The commit is the same crash-safe protocol an
// archive merge uses -- stage, marker, rename, publish, unlink -- so a crash mid-transform is
// finished or discarded by finishPendingMerges without it needing to know a columnar rewrite
// happened at all.
//
// The caller must hold maintMu, as merging and reseal do, so segment rewrites never overlap.
func (c *Collection) columnarizeSealedSegment(sh *shard, src *segment, s *adSchema, hot []int) bool {
	if sh.allocNamed == nil || sh.segDir == "" {
		return false // in-memory collection: nothing to make durable, and no file to stage
	}
	dst, srcOffs, newOffs := c.columnarizeSegment(sh, src, s, hot)
	if dst == nil {
		return false
	}
	if colCommitStallHook != nil {
		colCommitStallHook()
	}
	// A sealed segment in a MUTABLE table can still be superseded in place, and the rewrite read the
	// source off the shard lock -- so a delete or update that landed mid-build is recorded on the
	// source and missing from the output. Copying the supersession sequences across under the lock,
	// just before the swap, is what keeps that record dead: without it a deleted ad comes back.
	//
	// Append-only tables never supersede, so this is a no-op there rather than a special case.
	reconcile := func() {
		for i, so := range srcOffs {
			sup := recSuperseded(src.data, so)
			if sup != recSuperseded(dst.data, newOffs[i]) {
				dst.supersedeRec(newOffs[i], sup)
			}
		}
	}
	// Reconcile by-key resolution to the new offsets in the SAME critical section as the swap. dst
	// already carries the key index/Bloom (columnarizeSegment attached them), so lookupSealed can find
	// dst's records the moment the swap makes it visible; here, still under the swap lock, evict every
	// directory entry whose current record lives in dst so findCurrent cannot return an old, now-stale
	// offset for one of dst's keys -- those keys then resolve through the key index instead. dst.id is
	// set by the swap that immediately precedes this, which evictSegKeys needs.
	postSwap := func() {
		sh.evictSegKeys(dst)
	}
	if !c.commitSegmentRewrite(sh, []*segment{src}, dst, reconcile, postSwap) {
		return false // commitSegmentRewrite already discarded the staged file
	}
	// Persist dst's sidecar (key index, and the attribute index when one is configured) now that the
	// swap has named its file. This is deliberately not left to reindexAfterCompaction: dst now carries
	// an in-RAM key index, and sealSegmentIndex/sealAndEvictShard short-circuit on a set keyIdx, so the
	// deferred path would never write the file. installSidecar STORES (not CAS) the key index, so it
	// cleanly upgrades the in-RAM index to the file-backed one; on any I/O error dst keeps its in-RAM
	// index (still correct) and is simply rebuilt on the next open. The zone map and columnar block are
	// carried over unchanged (colBlobForSeg declines for a segment that holds its own block).
	c.persistColumnarizedSidecar(sh, dst)
	columnarizedSegments.Add(1)
	columnarizedBytesSaved.Add(int64(src.used) - int64(dst.used))
	return true
}

// persistColumnarizedSidecar writes the just-swapped columnar segment's sidecar container and swaps
// the file-backed key index (plus an attribute index when the collection has a spec) in over the
// in-RAM one published at commit. Best-effort and off the shard lock (installSidecar takes it for the
// publish): on any failure dst keeps its in-RAM key index, so by-key resolution stays correct and the
// only cost is rebuilding the sidecar on the next open.
func (c *Collection) persistColumnarizedSidecar(sh *shard, dst *segment) {
	path := snapshotPath(dst)
	if path == "" {
		return
	}
	var attrBlob []byte
	if c.spec.Load().any() {
		if si := buildSegIndex(c, dst, dst.used, c.spec.Load()); si != nil {
			if b, e := buildSidecarIndex(si); e == nil {
				attrBlob = b
			}
		}
	}
	container := buildSegmentSidecar(attrBlob, buildKeyIndex(dst.data, dst.used, c.h),
		c.colBlobForSeg(dst), buildZoneBlob(dst.zones), dst.used, dst.dictRecOff())
	c.installSidecar(sh, dst, path, container)
}

// columnarizeSealed rewrites every eligible sealed segment into columnar form and returns how many
// were replaced.
//
// Modelled on internSealedLocked, which does the same thing for interning: gather candidates under
// the shard lock, build each one off-lock (the sources are sealed and immutable), commit each swap
// individually, then reindex ONCE at the end rather than per segment.
//
// The reindex is not optional. A rewritten segment arrives with no sidecar, and the sidecar carries
// the key index, the attribute indexes and the zone map -- so skipping it would trade the row copy
// for a lost index, which is a worse deal than it looks. What the rebuilt sidecar does NOT carry is
// another copy of the columns: colBlobForSeg declines for a segment that already holds its own.
//
// The caller must hold maintMu, as compaction and reseal do, so segment rewrites never overlap.
func (c *Collection) columnarizeSealed() int {
	// An encrypted collection is columnarized like any other. A sealed value is not a literal, so
	// storableInColumn already treats that CELL as an exception and leaves it in the record -- the same
	// mechanism that handles a too-wide or wrong-typed value. Nothing about a sealed cell has to change
	// the layout.
	//
	// This used to refuse the whole segment, on the reasoning that a columnar payload stores values in
	// the clear. That is true of a column, and a sealed value never becomes one. Refusing per COLLECTION
	// also stops making sense once private attributes are always sealed: every collection would then be
	// "encrypted" and none would get the format.
	//
	// Verified rather than argued -- TestColumnarNativeOverEncryptedStoresNoPlaintext columnarizes an
	// encrypted collection, reads every ad back with its sealed value intact, and scans every file on
	// disk for the plaintext.
	st := c.schemaScan.Load()
	if st == nil || st.schema == nil || len(st.schema.fields) == 0 {
		return 0 // no schema derived yet: nothing to move into columns
	}
	budget := c.columnarBudget()
	if budget <= 0 {
		return 0 // disabled by Options.ColumnarSegmentBudget
	}
	total := 0
	for _, sh := range c.shards {
		if total >= budget {
			break
		}
		if sh.allocNamed == nil || sh.segDir == "" {
			continue // in-memory shard: the format has nowhere durable to live
		}
		sh.mu.Lock()
		act := sh.act
		var srcs []*segment
		for _, seg := range sh.segs {
			if seg != nil && seg != act && seg.used > 0 && !seg.columnarized() {
				srcs = append(srcs, seg)
			}
		}
		sh.mu.Unlock()
		for _, src := range srcs {
			if total >= budget {
				break
			}
			if c.columnarizeSealedSegment(sh, src, st.schema, st.hot) {
				total++
			}
		}
	}
	if total > 0 {
		c.reindexAfterCompaction()
		// Columnarizing a sealed segment recompresses its remnants with that segment's own
		// (non-current) dictionary codec, warming an encoder the read path never uses again.
		// Drop those idle encoders so their match-finder history does not stay resident.
		c.releaseIdleEncoders()
	}
	return total
}

// recolumnarizeStaleGroups re-columnarizes the already-columnarized sealed segments whose built-in
// group set differs from next (the newly-committed set), so a group the table has just started
// committing to is actually applied to the segments that predate it. Bounded by the columnar budget,
// so a change converges over several maintenance passes rather than stalling on one, exactly as the
// initial columnarize backlog does. Returns how many segments were rewritten.
//
// Only segments whose group set is actually stale are touched -- one already carrying next is skipped
// -- so a steady group set costs a comparison per segment and no rewrites. Holds maintMu, as the
// other segment-rewriting passes do, so rewrites never overlap.
func (c *Collection) recolumnarizeStaleGroups(next []*colGroup) int {
	budget := c.columnarBudget()
	if budget <= 0 {
		return 0 // rewrite disabled: the adopted set still applies to future/non-columnarized segments
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	st := c.schemaScan.Load()
	if st == nil || st.schema == nil || len(st.schema.fields) == 0 {
		return 0
	}
	total := 0
	for _, sh := range c.shards {
		if total >= budget {
			break
		}
		if sh.allocNamed == nil || sh.segDir == "" {
			continue // in-memory shard: nothing durable to rewrite
		}
		sh.mu.Lock()
		act := sh.act
		var srcs []*segment
		for _, seg := range sh.segs {
			if seg == nil || seg == act || seg.used == 0 || !seg.columnarized() {
				continue
			}
			cn := seg.colNative.Load()
			if cn == nil || c.sameGroupSet(cn.seg.groups, next) {
				continue // already carries the committed group set
			}
			srcs = append(srcs, seg)
		}
		sh.mu.Unlock()
		for _, src := range srcs {
			if total >= budget {
				break
			}
			if c.columnarizeSealedSegment(sh, src, st.schema, st.hot) {
				total++
			}
		}
	}
	if total > 0 {
		c.reindexAfterCompaction()
		c.releaseIdleEncoders()
	}
	return total
}

// defaultColumnarBudget is how many sealed segments one maintenance pass rewrites into columnar
// form when the caller has not said.
//
// Bounded because enabling this over an existing archive means rewriting every sealed segment once,
// and a single pass doing that at history scale is hours of I/O -- the failure a retrain caused when
// it resealed a whole archive. 64 segments is a few hundred MiB of rewrite per pass at typical
// segment sizes, which converges over a handful of maintenance intervals without a stall long enough
// to notice. Each segment is rewritten at most once, so the backlog only ever shrinks.
const defaultColumnarBudget = 64

// columnarBudget resolves Options.ColumnarSegmentBudget: 0 takes the default, negative disables.
//
// Negative rather than 0 for "off" for the same reason GroupSchemaCount uses it: 0 is what a caller
// who never heard of the option passes, and that caller should get the default behaviour.
func (c *Collection) columnarBudget() int {
	if c.colBudget == 0 {
		return defaultColumnarBudget
	}
	return c.colBudget
}

// ColumnarizeSealed rewrites sealed segments into columnar-native form as a maintenance pass: each
// attribute the schema carries is stored once, in the segment's own columnar payload, and removed
// from the records. Returns how many segments were rewritten.
//
// Idempotent and bounded. An already-columnarized segment is skipped and the active write target is
// never touched, so re-calling is cheap; one pass rewrites at most ColumnarSegmentBudget segments,
// so a large existing archive converges over several passes instead of stalling on one.
//
// Requires a derived schema, which BuildAndEnableSchemaScan produces and also calls this from -- so
// a caller on the ordinary schema-review interval gets this without asking for it. Holds maintMu, so
// it serializes against Compact/RetrainDict/Rotate/Rewrite and against a merge.
func (c *Collection) ColumnarizeSealed() int {
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	return c.columnarizeSealed()
}

// storableInColumn reports whether a value may be moved out of its record into a column.
//
// Only LITERALS may. An expression node contains attribute references, and those references are ids
// in the same space as the record that holds them -- a segment-local dictionary id for an interned
// segment. The columnar payload is built from records whose ids have been resolved to the GLOBAL
// intern space, so moving an expression into it and splicing it back writes a global id into a
// locally-keyed record, where it resolves to whatever attribute happens to hold that local id.
//
// It is not a hypothetical. `RequestMemory = ProcId * 512 + 7` came back as `Slack * 512 + 7`: the
// reassembled ad was structurally perfect, every literal correct, and queries reading that attribute
// returned the wrong rows. Nothing errored, and a test comparing ads while rendering expressions as
// "<expr>" called the two forms equal.
//
// Keeping expressions in the record costs almost nothing -- they are a small minority of values, and
// a schema field holding one already ESCAPES, so the block marks it absent and reconstruct skips it
// rather than carrying a second copy. Translating ids inside arbitrary expression nodes would be the
// alternative, and it would have to be exactly right for every node shape forever.
func storableInColumn(node []byte) bool {
	_, ok := wire.LiteralValue(node)
	return ok
}

// globalIDOf resolves a record attribute key -- a segment-local dictionary id, or an inline name --
// into the global intern id the schema and group schemas are keyed by.
func (c *Collection) globalIDOf(d *segDictHandle, id uint32, name string) (uint32, bool) {
	if name == "" {
		if d == nil {
			return id, true // not interned: the record already carries global ids
		}
		nm := d.name(id)
		if nm == nil {
			return 0, false
		}
		name = string(nm)
	}
	return c.intern.LookupID(name)
}
