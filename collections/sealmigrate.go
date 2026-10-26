package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Migrating a store written before private attributes were sealed.
//
// Sealing is applied at ENCODE time, so turning it on protects new writes and leaves everything already
// on disk exactly as it was: a ClaimId written last week is still a plaintext string literal in its
// segment. Nothing reports that -- reads work, queries work -- so without this pass a store would sit
// half-protected indefinitely, and the operator would have no way to tell.
//
// The pass rewrites each segment that still holds an unsealed value through the ordinary reseal path,
// which re-encodes every record under the current codec and hot set, and therefore under the current
// sealer.
//
// Three properties, in the order they matter:
//
//   - IDEMPOTENT, because the candidate test is the data itself rather than bookkeeping. A segment is a
//     candidate only if it actually contains a value that should be sealed and is not, so a re-run after
//     a crash re-examines everything and rewrites only what is left. There is no state to disagree with
//     the store.
//   - CRASH-SAFE, because each segment is replaced whole: the reseal writes a new file and the swap
//     installs it under the shard lock, so an interrupted run leaves every segment either fully migrated
//     or untouched. A partly-written replacement is never installed.
//   - RESUMABLE follows from the first two. The marker below is only an optimization -- deleting it
//     costs a scan, not correctness.
//
// A trained compression dictionary is part of what has to be migrated. Its content is literally a
// concatenation of sampled records, so one trained while values were in the clear holds those values,
// and resealing the records under it -- or leaving it in dicts/attic, where pruning normally keeps a
// retired one -- keeps them on disk. A pass that runs therefore retrains from sealed samples, rewrites
// every segment still on an older dictionary, and deletes the older ones outright. Which dictionaries
// saw plaintext cannot be told from their bytes, so every one that predates the pass is treated as if
// it had.

// sealMigrationMarker records that a full pass found nothing left to migrate. Its absence costs one scan
// of the sealed segments at open; its presence skips that. It is written only after a clean pass, so a
// crash mid-pass simply leaves it absent.
//
// Its content names what the pass established (see sealMigrationStamp), and a marker whose content
// differs counts as absent. A marker that only existed would vouch for a policy it never checked: an
// attribute added to the encrypted set after the pass ran, or a pass whose detection was later found
// wrong, would both be skipped for good.
const sealMigrationMarker = "sealed-attrs.done"

// sealMigrateHook is a test seam, run with no lock held at the two points where a concurrent commit
// changes what the pass has to do: "collapsed", between the collapse and the shard loop, where a commit
// can leave a live delta; and "resealed", between a shard's rewrite and its swap, where a commit can
// supersede a record that has already been copied. Production leaves it nil.
var sealMigrateHook func(stage string)

// sealMigrationVersion is bumped whenever a store marked by an earlier build may still hold values this
// one would seal -- a detection fix, or a change to what classad.IsPrivateAttribute covers -- so that
// every such store runs the pass once more on upgrade.
//
// 2: version 1 treated any non-literal value as sealed, so an expression in an encrypted attribute was
// left in the clear, as was every value in a segment holding a delta record or columnar payload; and it
// resealed under a dictionary that could hold plaintext.
const sealMigrationVersion = 2

// MigrateSealedAttrs rewrites every sealed segment that still holds a value this collection would seal,
// so a store written before sealing was turned on stops carrying private attributes in the clear.
// Returns the number of segments rewritten.
//
// Safe to call on a store that needs nothing: the scan finds no candidate and it returns 0. Safe to call
// twice, and safe to interrupt (see the file comment). A collection with no sealer, or no persistence,
// has nothing to migrate.
//
// workers bounds the concurrent rewrites; <= 0 takes a default derived from GOMAXPROCS. Rewriting is
// decompress + re-encode + compress per record, so it is CPU-bound and worth parallelizing on a store
// with many segments -- but each rewrite also holds a whole new segment in memory, which is why it is
// bounded rather than one goroutine per segment.
func (c *Collection) MigrateSealedAttrs(workers int) int {
	if c.sealer == nil || c.dir == "" {
		return 0 // nothing to seal with, or nothing durable to rewrite
	}
	if c.sealMigrationDone() {
		return 0
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()

	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
		if workers > 4 {
			workers = 4 // each worker holds a segment's worth of rewritten records
		}
	}
	// Collapse first, for the same reason every other rewrite does: this pass retires the active
	// segment below, and a live delta caught in it would be sealed somewhere no collapse looks.
	// Worse here than elsewhere -- resealSegmentsAs REFUSES a segment holding a live delta, so the
	// segment is not migrated either, and the pass reports success having both stranded the
	// fragment and left the legacy bytes it exists to remove.
	c.collapseBeforeRewrite()
	if sealMigrateHook != nil {
		sealMigrateHook("collapsed")
	}
	codec, retiredBelow, rotated := c.rotateDictForSeal()
	// complete stays true only if every candidate was rewritten and installed. Anything less leaves
	// plaintext behind, and the marker must not say otherwise: the pass is one-shot per store.
	complete := true
	migrated := 0
	for _, sh := range c.shards {
		sh.mu.Lock()
		// Refuse to retire an active segment that still holds a live delta: the collapse above
		// runs off the shard lock, so a commit can land between it and here. Skipping the shard
		// leaves it for the next run; retiring it strands the fragment permanently.
		//
		// The same test covers every sealed segment of the shard, because the reseal DROPS
		// superseded delta records: with no live delta anywhere in the shard, each current version
		// is a whole record and nothing reads a superseded fragment, but a live delta in one segment
		// still needs the superseded ones before it, wherever they sit.
		if shardHasLiveDelta(sh) {
			sh.mu.Unlock()
			migrateSkippedLiveDelta.Add(1)
			complete = false
			continue
		}
		// Retire the active segment from the write path so it is migrated too, exactly as the dict
		// reseal does. On reopen the last segment becomes the write target again, and its records are
		// the LEGACY ones -- skipping it left a store that reported a successful migration with
		// plaintext still in it, which is the failure this pass exists to prevent. writeRecord
		// allocates a fresh segment when act is nil, so the next write is unaffected.
		sh.act = nil
		var srcs []*segment
		for _, s := range sh.segs {
			if s != nil && s.used > 0 {
				srcs = append(srcs, s)
			}
		}
		sh.mu.Unlock()

		// Scan and rewrite off the shard lock; only the swap takes it.
		type result struct {
			src, dst *segment
		}
		var (
			wg      sync.WaitGroup
			sem     = make(chan struct{}, workers)
			mu      sync.Mutex
			results []result
		)
		for _, src := range srcs {
			src := src
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if !c.onRetiredDict(src, rotated) && !c.segmentHasUnsealedValue(src) {
					return
				}
				dst := c.resealOneSegment(sh, src, codec)
				mu.Lock()
				if dst != nil {
					results = append(results, result{src: src, dst: dst})
				} else {
					complete = false
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		if sealMigrateHook != nil {
			sealMigrateHook("resealed")
		}

		var toReap []*segment
		for _, r := range results {
			sh.mu.Lock()
			// Swap only if the source still occupies its slot and has not become the write target again
			// -- the same defence the dict transcode takes against a concurrent reseal or rotate.
			if int(r.src.id) < len(sh.segs) && sh.segs[r.src.id] == r.src && sh.act != r.src {
				carrySupersedes(r.src, r.dst)
				sh.segs[r.src.id] = r.dst
				migrated++
				if r.src.retire() {
					toReap = append(toReap, r.src)
				}
			} else {
				r.dst.retire()
				r.dst.reapAndHook() // slot moved under us: drop the fresh file rather than leak it
				complete = false
			}
			sh.mu.Unlock()
		}
		for _, seg := range toReap {
			seg.reapAndHook() // munmap + unlink the superseded file, off-lock
		}
	}
	if migrated > 0 {
		// EVERY STRUCTURE THAT NAMES A (segment, offset) IS NOW STALE. A reseal re-encodes each record
		// and sizes the new segment exactly, so the same key sits at a different offset -- and the old
		// offset may be past the end of the new, smaller file.
		//
		// Leaving them stale did not read as corruption. A SCAN walks segments directly and was
		// perfectly fine, which is what the first version of this pass verified. A GET goes through the
		// directory and the bucket chain, so it read a header where there was none: usually a key
		// mismatch that ends the chain and reports the key MISSING (1998 of 2000, in the test below),
		// occasionally an offset past the mapping, which is a panic in a running daemon:
		//
		//	panic: runtime error: slice bounds out of range [:67269717] with capacity 148816
		//
		// Reindex rebuilds each segment's key sidecar from its actual contents and evicts that
		// segment's directory entries, so both lookup paths point at the new layout. This is the same
		// reconciliation the other two swap-a-rewritten-segment paths do (compaction, InternSealed);
		// this pass simply did not, which is the whole defect.
		c.reindexAfterCompaction()
	}
	if migrated > 0 || rotated {
		c.pruneDicts()
	}
	if rotated && complete {
		// Only once nothing references them: an incomplete pass leaves segments on an older
		// dictionary, and those still need it to be read. The next pass deletes it.
		c.deleteDictsBelow(retiredBelow)
	}
	if complete {
		c.markSealMigrationDone()
	}
	return migrated
}

// carrySupersedes marks superseded in dst every record that is superseded in src.
//
// The reseal appends every record as current. A mutable segment holds superseded versions beside
// current ones, so installing dst as built brought each of them back: every Get of an updated key
// returned its OLDEST copy, and a scan returned every version of it. Reading the flags here, under the
// shard lock, rather than when the records were copied also covers a commit that superseded a record
// after its copy was made -- the copy ran off the lock -- and the lock keeps any further stamp from
// landing in between.
func carrySupersedes(src, dst *segment) {
	type recID struct {
		seq uint64
		key string
	}
	at := make(map[recID]uint32)
	for off := uint32(0); off < uint32(dst.used); {
		tl := recTotalLen(dst.data, off)
		if tl == 0 {
			break
		}
		if !recIsMarker(dst.data, off) && recSuperseded(dst.data, off) == seqMax {
			at[recID{recSeq(dst.data, off), string(recKey(dst.data, off))}] = off
		}
		off += tl
	}
	for off := uint32(0); off < uint32(src.used); {
		if end, ok := src.damaged().skip(off); ok {
			off = end
			continue
		}
		tl := recTotalLen(src.data, off)
		if tl == 0 {
			break
		}
		if !recIsMarker(src.data, off) {
			if sup := recSuperseded(src.data, off); sup != seqMax {
				if d, ok := at[recID{recSeq(src.data, off), string(recKey(src.data, off))}]; ok {
					dst.supersedeRec(d, sup)
				}
			}
		}
		off += tl
	}
}

// shardHasLiveDelta reports whether any segment of sh holds a live delta record. Caller holds sh.mu.
func shardHasLiveDelta(sh *shard) bool {
	if segHasLiveDelta(sh.act) {
		return true
	}
	for _, s := range sh.segs {
		if s != nil && s != sh.act && segHasLiveDelta(s) {
			return true
		}
	}
	return false
}

// rotateDictForSeal replaces the write codec's trained dictionary with one trained from SEALED samples,
// when any trained dictionary is in use, and returns the codec the pass should reseal under. retiredBelow
// is the lowest dictionary id the pass did not create: every registered id below it predates the pass.
// rotated reports whether a replacement happened, which makes every segment on an older dictionary a
// candidate (see onRetiredDict).
//
// Training failing is not a reason to keep the old dictionary: the pass falls back to the base codec,
// which has none, and compression returns at the next retrain.
func (c *Collection) rotateDictForSeal() (codec Codec, retiredBelow uint32, rotated bool) {
	codec = c.currentCodec()
	if !c.trainedDictInUse() {
		return codec, 0, false
	}
	c.dicts.mu.Lock()
	retiredBelow = c.dicts.next
	c.dicts.mu.Unlock()
	if dict, err := TrainDict(c.sealedSamples(sealRotateSamples)); err == nil {
		if fresh, err := NewZSTDCodec(dict); err == nil {
			if _, err := c.dicts.register(fresh, dict); err == nil {
				c.codec.Store(&codecHolder{fresh})
				c.lastDictBytes.Store(int64(len(dict)))
				c.lastRetrainUnix.Store(time.Now().UnixNano())
				return fresh, retiredBelow, true
			}
		}
	}
	base, _ := c.dicts.codecFor(0)
	c.codec.Store(&codecHolder{base})
	return base, retiredBelow, true
}

// sealRotateSamples is how many records the replacement dictionary is trained from: the default sample
// size of a scheduled retrain.
const sealRotateSamples = 4096

// trainedDictInUse reports whether the write codec or any segment uses a trained dictionary (any
// registered id other than the base codec's 0).
func (c *Collection) trainedDictInUse() bool {
	trained := func(cd Codec) bool {
		id, ok := c.dicts.idFor(cd)
		return ok && id != 0
	}
	if trained(c.currentCodec()) {
		return true
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s != nil && s.used > 0 && trained(s.codec) {
				sh.mu.RUnlock()
				return true
			}
		}
		sh.mu.RUnlock()
	}
	return false
}

// onRetiredDict reports whether seg is compressed under a trained dictionary the pass is replacing.
func (c *Collection) onRetiredDict(seg *segment, rotated bool) bool {
	if !rotated || seg.codec == c.currentCodec() {
		return false
	}
	id, ok := c.dicts.idFor(seg.codec)
	return ok && id != 0
}

// sealedSamples returns up to n recent records in inline wire form with every value this collection
// seals sealed. The sampler hands back an inline record's bytes as stored, which for a legacy record are
// plaintext -- and a dictionary's content IS its samples -- so each one is decoded and encoded afresh.
func (c *Collection) sealedSamples(n int) [][]byte {
	raw := c.CollectSamplesRecent(n, 0, 0)
	out := raw[:0]
	for _, w := range raw {
		ad, err := c.decodeWire(w)
		if err != nil {
			continue // a sample that does not decode is left out, never kept as it was
		}
		out = append(out, c.encodeAd(ad))
	}
	return out
}

// deleteDictsBelow unlinks every dictionary file with an id below id that no registered codec still
// uses, from dicts/ and from dicts/attic/.
//
// This is the one place a dictionary is deleted rather than retired to the attic. The attic keeps a
// dictionary so a segment whose file name was mangled can still be decoded once repaired; here the
// dictionary may itself hold the plaintext the pass exists to remove, and keeping it would keep that.
func (c *Collection) deleteDictsBelow(id uint32) {
	c.dicts.mu.Lock()
	dir := c.dicts.dir
	live := make(map[uint32]bool, len(c.dicts.byID))
	for k := range c.dicts.byID {
		live[k] = true
	}
	c.dicts.mu.Unlock()
	if dir == "" {
		return
	}
	for _, d := range []string{dir, filepath.Join(dir, "attic")} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			var n uint32
			if _, err := fmt.Sscanf(e.Name(), "%d.zst", &n); err != nil || n == 0 || n >= id || live[n] {
				continue
			}
			_ = os.Remove(filepath.Join(d, e.Name()))
		}
	}
}

// segmentHasUnsealedValue reports whether seg holds any attribute this collection would seal, stored
// other than sealed -- which is exactly "was this record written before sealing was on for that
// attribute".
//
// It reads the segment's records directly and stops at the first hit: a store that needs migrating pays
// almost nothing to discover it, and a store that does not pays one pass.
func (c *Collection) segmentHasUnsealedValue(seg *segment) bool {
	var buf []byte
	found := false
	for off := 0; off < seg.used && !found; {
		o := uint32(off)
		if end, ok := seg.damaged().skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		// The FULL record: a columnarized segment keeps the values its schema covers in the
		// segment's columnar payload, not in the record, and reading only the stored bytes would
		// never see them.
		if !recIsMarker(seg.data, o) {
			w, err := c.recordWireIn(seg, seg.data, o, buf[:0])
			if err == nil {
				buf = w
				found = c.recordHasUnsealedValue(seg.dict.Load(), w)
			}
		}
		off += int(total)
	}
	return found
}

// recordHasUnsealedValue reports whether a record's STORED form holds a value this collection would seal
// in any form other than the sealed one.
//
// Sealed means the nEncrypted node and nothing else. The encoder seals whatever value an attribute
// holds, expressions included, so an unsealed value can be any node at all: a string, a list, a nested
// ad, a reference, or the unary minus over a literal that older builds stored for a negative number.
// Testing "is not a literal" instead reported every one of those as already sealed.
//
// It reads the raw wire deliberately. Converting the record first -- wireToInline, say -- would decode
// with the key and RE-SEAL on the way out, so a legacy plaintext value would come back sealed and this
// would report every segment clean: the check would mask exactly what it exists to find.
//
// Name resolution has to follow the record's own encoding: a segment-local dictionary resolves its ids,
// an inline record carries its names, and anything else resolves against the collection's global table.
// Getting that wrong is not a wrong answer, it is a nil dereference -- which is how the first version
// announced itself.
func (c *Collection) recordHasUnsealedValue(dict *segDictHandle, w []byte) bool {
	sealedAlready := func(name string, node []byte) bool {
		if !c.shouldEncrypt(name) {
			return true // not ours to seal
		}
		return wire.IsEncryptedNode(node)
	}
	found := false
	switch {
	case dict != nil:
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			if nm := dict.name(id); nm != nil && !sealedAlready(string(nm), node) {
				found = true
				return false
			}
			return true
		})
	case c.inline:
		wire.Ad(w).ForEachNamed(nil, func(name string, node []byte) bool {
			if !sealedAlready(name, node) {
				found = true
				return false
			}
			return true
		})
	default:
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			if name, ok := c.intern.Name(id); ok && !sealedAlready(name, node) {
				found = true
				return false
			}
			return true
		})
	}
	return found
}

func (c *Collection) sealMigrationPath() string { return filepath.Join(c.dir, sealMigrationMarker) }

// sealMigrationStamp is the marker content for the current policy: the pass version and the explicit
// encrypted-attribute set. The private attributes are compiled in, so the version covers them.
func (c *Collection) sealMigrationStamp() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sealed v%d\n", sealMigrationVersion)
	for _, name := range c.EncryptedAttrNames() { // case-folded and sorted, so the stamp is canonical
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return b.String()
}

func (c *Collection) sealMigrationDone() bool {
	if c.dir == "" {
		return false
	}
	got, err := os.ReadFile(c.sealMigrationPath())
	return err == nil && string(got) == c.sealMigrationStamp()
}

// markSealMigrationDone records a clean pass. A failure to write it costs a rescan next time, which is
// why it is not reported: the store is correct either way.
func (c *Collection) markSealMigrationDone() {
	if c.dir == "" {
		return
	}
	path := c.sealMigrationPath()
	stamp := c.sealMigrationStamp()
	if got, err := os.ReadFile(path); err == nil && string(got) == stamp {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(stamp), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
