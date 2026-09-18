package collections

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Delta records store only the attributes one write CHANGED, instead of the whole ad.
//
// The motivating measurement: replaying a real OSPool AP job_queue.log, 117,796 ad-touches
// in 20 MB of log each rewrote a ~7.3 KB ad, handing the encoder and compressor 861 MB --
// a 43x amplification of the log they came from. The attributes one schedd transaction
// actually changes average ~150 bytes (median 70). Storing just those is ~49x less work per
// write.
//
// The cost is read amplification: a point read must walk back along the key's version chain
// collecting deltas until it reaches a full record, then merge them oldest-first. That is
// bounded by writing a fresh full record once a key has accumulated DeltaMax deltas. The
// bound is ADVISORY -- correctness needs only that walking back eventually reaches a full
// record, never that the chain be short -- so an approximate depth counter is sound, and a
// racing second writer can only make a chain longer than intended, never break it.
//
// The hard invariant is elsewhere: anything that can DROP an older version must first
// materialize the deltas that depend on it. Compaction retains superseded versions only
// above a retain floor, so delta records and time travel are refused together (see Open).
//
// COLUMNARIZATION is where this is still unfinished, and the constraint is a DENSITY one rather
// than the correctness one it looks like. Reads are fine: the seal-collapse invariant leaves only
// SUPERSEDED deltas in a sealed segment, nothing reads those, and TestDeltaVsColumnarization
// confirms every attribute survives the rewrite. But the columnar builder filters markers and
// nothing else, so each dead fragment still takes a row whose schema fields are almost entirely
// absent. Measured over a delta-shaped workload (TestColumnarDensityWithDeltas): 77% of the rows
// in a columnarized segment were dead delta records, and the sealed arena grew 13%.
//
// So a sealed segment should ideally hold NO delta records at all. Getting there means dropping
// them when the segment seals -- they are garbage by then, superseded by the whole records the
// collapse just wrote -- which is a seal-time rewrite that does not exist yet.
//
// (An earlier version of this comment said the two were simply not enabled together. A later one
// said they were compatible, citing the correctness test. Both were too strong.)

// deltaTracker counts, per key, how many delta records have been written since that key's
// last full record. Absence of a key means "no full record has been written by this process",
// which forces a full record -- the conservative direction, and what makes a fresh start or a
// reopen self-healing without reading anything.
type deltaTracker struct {
	mu sync.Mutex
	// depth is keyed by the key's 64-bit hash, not the key itself, for two reasons that both
	// matter at this write rate. A map with string keys holds pointers, so the GC scans every
	// entry on every cycle; a map[uint64]int holds none and is skipped entirely. And passing a
	// key as a string forces an allocation per write -- Go elides the []byte->string copy only
	// for a direct map index, not for a function argument or a map STORE.
	//
	// Collisions are tolerated rather than resolved, which is safe here in a way it would not
	// be in the store: two keys sharing a counter can only make a chain longer or shorter than
	// DeltaMax intended. The bound is advisory -- correctness needs a chain to terminate at a
	// whole record, which keyExists establishes, never a particular length.
	depth map[uint64]int
	// deltas and fulls count the decisions taken. They are the only way to tell a working
	// delta store from one that has quietly fallen back to storing whole ads on every write --
	// which reads back perfectly correctly and saves nothing, so no correctness test can catch
	// it. Also the ratio an operator wants when deciding whether DeltaMax is earning its keep.
	deltas, fulls atomic.Int64
}

func newDeltaTracker() *deltaTracker { return &deltaTracker{depth: make(map[uint64]int)} }

// maxTrackedKeys bounds the tracker's memory. It holds one small entry per key that has been
// written, which for a schedd mirror is one per job -- bounded, but not by anything the store
// controls, and a long-lived process churning through job ids would grow it without limit.
//
// Overflow drops the whole map rather than evicting cleverly: an absent key simply means "no
// full record is known", which forces the next write for it to be a full record. That is the
// conservative direction -- it costs writes, never correctness -- and it self-heals within one
// write per live key.
const maxTrackedKeys = 1 << 20

// next reports whether the write for key should be a delta, and records the decision. A
// caller that cannot use a delta (an attribute was deleted, nothing changed, the key has no
// full record yet, or the bound is reached) gets false and the key's depth resets.
func (t *deltaTracker) next(h uint64, eligible, baseExists bool, max int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.depth) >= maxTrackedKeys {
		clear(t.depth)
	}
	d := t.depth[h]
	// Absence means "depth 0" now, not "no base": collapseSealedChains guarantees every live
	// key has a whole record, so the only question left is whether the key exists at all --
	// which the caller answers with a location lookup, no decode. Treating absence as "no
	// base" instead would force a whole record per key per seal.
	if !eligible || !baseExists || d >= max {
		t.depth[h] = 0 // this write is a full record; the chain restarts here
		t.fulls.Add(1)
		return false
	}
	t.depth[h] = d + 1
	t.deltas.Add(1)
	return true
}

// forget drops a key's depth so its next write is a full record. Used when a key is deleted
// and when compaction materializes, so the tracker never claims a base exists that the store
// no longer holds.
func (t *deltaTracker) forget(h uint64) {
	t.mu.Lock()
	delete(t.depth, h)
	t.mu.Unlock()
}

// deltaAd builds the ad to store in a delta record: the named attributes, taken from the
// full ad the caller already has. Attribute names are case-insensitive, so the wanted set is
// matched folded. Returns nil if any name is missing, which forces a full record rather than
// storing an incomplete delta.
func deltaAd(full *classad.ClassAd, changed []string) *ast.ClassAd {
	if full == nil || len(changed) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(changed))
	for _, n := range changed {
		want[strings.ToLower(n)] = struct{}{}
	}
	src := full.AST()
	if src == nil {
		return nil
	}
	out := &ast.ClassAd{Attributes: make([]*ast.AttributeAssignment, 0, len(changed))}
	for _, a := range src.Attributes {
		if _, ok := want[strings.ToLower(a.Name)]; ok {
			out.Attributes = append(out.Attributes, &ast.AttributeAssignment{Name: a.Name, Value: a.Value})
			delete(want, strings.ToLower(a.Name))
		}
	}
	if len(want) != 0 {
		return nil // a named attribute was not on the ad: store a full record instead
	}
	return out
}

// mergeDelta applies a delta's attributes over base, last write winning. base is mutated.
func mergeDelta(base, delta *classad.ClassAd) {
	if delta == nil {
		return
	}
	mergeDeltaAST(base, delta.AST())
}

// mergeDeltaAST is mergeDelta for a caller holding the delta as a decoded AST rather than as a
// ClassAd. Merging only ever reads the AST's attribute list, so wrapping one in a ClassAd first
// bought nothing and cost a full name-index rebuild per delta merged (classad.FromAST calls
// rebuildIndex) -- which on a real queue replay was the largest single item under materializeAt
// after the decode itself.
func mergeDeltaAST(base *classad.ClassAd, d *ast.ClassAd) {
	if base == nil || d == nil {
		return
	}
	for _, a := range d.Attributes {
		base.Insert(a.Name, a.Value)
	}
}

// storeReads counts reads of a stored record made to satisfy a write. It exists so a test can
// assert that patching an attribute performs NONE: the removal of that read is the point of
// delta records, it is invisible in the results, and nothing else would notice it coming back.
var storeReads atomic.Int64

// deltaHdrDispatch selects how a reader decides whether a record is a delta.
//
//	true  -- from the record HEADER (recIsDelta): a 4-byte read, no decompression.
//	false -- from the record PAYLOAD (isDeltaRecord): decompress the record first, which is how
//	         delta records were first implemented.
//
// Production always uses the header; nothing outside a test writes this. It exists because the
// two dispatches are the whole subject of a measurement -- running them as separate builds
// compares two machine states as much as two implementations, and running them in one process
// over one dataset does not -- and because a differential test that runs both is a stronger
// check on the header flag than any assertion about it: for every record a test writes, the two
// independent encodings of "this is a delta" must lead to the same ad.
var deltaHdrDispatch = true

// sealWalkRecords and sealWalkDeltas count what the seal-time walk (liveDeltaKeys) examines:
// every live record in a sealing segment, and the few that are actually open chains. The ratio is
// the argument for classifying from the header: under the payload dispatch every record counted
// here cost a decompression, and only the second counter's worth of them needed one. Internal
// because it answers a design question, not an operational one.
var (
	sealWalkRecords atomic.Int64
	sealWalkDeltas  atomic.Int64
)

// segRecIsDelta classifies a record under whichever dispatch is selected. Only the seal-time
// walk uses it: that walk visits every live record in a sealing segment, so it is where the
// payload dispatch was most expensive -- tens of thousands of decompressions per seal, to find
// the handful of keys whose chains are still open.
func segRecIsDelta(seg *segment, off uint32) bool {
	if deltaHdrDispatch {
		return recIsDelta(seg.data, off)
	}
	raw, err := seg.codec.Decompress(nil, recAd(seg.data, off))
	return err == nil && isDeltaRecord(raw)
}

// isDeltaRecord reports whether decompressed record bytes are a delta, according to the flag in
// the record's PAYLOAD. The header flag (recIsDelta) is what the read paths dispatch on, because
// it needs no decompression; this is the independent second copy, used to check that the two
// agree at the one place where disagreement would be silent (see materializeAt).
func isDeltaRecord(rec []byte) bool { return wire.IsDelta(rec) }

// compactLiveDeltas counts live delta records met by a compaction, which collapseBeforeRewrite
// is supposed to make impossible; each one aborts that shard's compaction.
// compactDroppedDeltas counts superseded delta versions dropped from the time-travel window
// rather than carried forward (see the compaction loop).
// deltaFlagMismatches counts records whose header flag and payload flag disagree -- a refused
// read, and the signal that a copy path lost or invented a flag.
var (
	compactLiveDeltas    atomic.Int64
	compactDroppedDeltas atomic.Int64
	deltaFlagMismatches  atomic.Int64
)

// DeltaAnomalies reports the three things that should all stay zero in a healthy delta store:
// live deltas met by compaction, superseded deltas dropped from the travel window, and records
// whose header and payload disagree about being a delta.
func DeltaAnomalies() (liveAtCompact, droppedFromHistory, flagMismatch int64) {
	return compactLiveDeltas.Load(), compactDroppedDeltas.Load(), deltaFlagMismatches.Load()
}

// encodeDelta encodes the record bytes for a write: a delta holding only the changed
// attributes when the collection has delta mode on and this write qualifies, else the whole
// ad exactly as before. The decision is recorded in the tracker, so the caller does not have
// to reason about the chain length.
//
// Delta mode is off unless Options.DeltaMax is set, so every existing caller and store keeps
// byte-identical behavior.
// h is the key's hash, which the caller has already computed: taking the key as a string and
// re-deriving it here cost a string allocation for the parameter and a []byte allocation per use
// of it, three per write, for a number the caller was holding.
func (c *Collection) encodeDelta(dst []byte, key []byte, h uint64, ad *classad.ClassAd, changed []string, removed bool) ([]byte, bool) {
	if c.deltaMax <= 0 || c.deltas == nil || !c.inline {
		return c.encodeAdInto(dst, ad.AST()), false
	}
	var d *ast.ClassAd
	eligible := !removed && len(changed) > 0
	if eligible {
		if d = deltaAd(ad, changed); d == nil {
			eligible = false
		}
	}
	// canWriteDelta LAST, and only when a delta is actually wanted: it records the store's
	// delta mode, and asking it on every write recorded it for stores that only ever held whole
	// records -- which is the permanence this lazy marker exists to avoid.
	if eligible && !c.canWriteDelta() {
		eligible = false
	}
	if !c.deltas.next(h, eligible, c.keyExists(key, h), c.deltaMax) {
		return c.encodeAdInto(dst, ad.AST()), false
	}
	// Deltas are encoded with the inline-name form and no hot header: the hot header indexes
	// a whole ad for the match fast path, and a fragment has no business claiming to be one.
	return wire.EncodeInlineDelta(dst, d, nil, c.shouldEncrypt, c.sealer), true
}

// materializeAt reconstructs the whole ad for key as of snapshot s0 from a delta chain,
// returning uncompressed wire bytes. The caller must hold at least the shard read lock: this
// reads segment bytes, and a segment's mapping is only guaranteed alive under that lock.
//
// It gathers every version of the key visible at s0 from BOTH resolution routes -- the
// in-directory bucket chain and the sealed key index -- rather than walking one of them,
// because which route holds a given version depends on compaction and reopens, and a delta
// whose base moved to the other route would otherwise be unresolvable. The versions are
// ordered by commit sequence, the newest full record among them is the base, and the deltas
// after it are merged over it in order.
//
// A chain with no full record cannot happen by construction (a key's first write is always
// full, and anything that drops versions materializes first), so it is reported as a miss
// rather than guessed at: returning a partial ad would look like data and be wrong.
// hasFull reports whether any gathered version is a whole record, i.e. whether the chain can
// be resolved from what has been found so far.
func (sh *shard) hasFull(vers []deltaVer) bool {
	for _, v := range vers {
		if !recIsDelta(v.seg.data, v.off) {
			return true
		}
	}
	return false
}

// want says which FORM the caller needs, and it decides how the merge is done -- the two forms
// have different cheapest routes and producing the unwanted one is pure waste:
//
//   - mWire alone: splice the entry bytes. Nothing is decoded and nothing is re-encoded, so this
//     is the cheap path, and it is what the scans, the watch publisher and the seal-time collapse
//     want (the collapse stores the bytes).
//   - mObj: decode and merge into an object, and encode only if mWire was asked for too. A point
//     read wants the object and nothing else, and used to get an encode of the merged ad that its
//     caller immediately decoded again.
//
// Asking for both is the expensive combination and only the collapse does it, only when the
// collection has an ordered index or a watcher -- the two things that need the object.
//
// dst, when non-nil, is where the merged BYTES are appended -- for a caller whose result is
// transient and who already owns a reusable buffer (the scan iterators do, and their contract
// already says the bytes live only until the next record). A caller that RETAINS the bytes must
// pass nil: the collapse buffers them until its commit lands.
func (sh *shard) materializeAt(c *Collection, key []byte, h uint64, s0 uint64, want materializeWant, dst []byte) ([]byte, *classad.ClassAd, bool) {
	var vers []deltaVer
	add := func(seg *segment, off uint32) {
		if seg == nil {
			return
		}
		if recSeq(seg.data, off) <= s0 && bytes.Equal(recKey(seg.data, off), key) {
			vers = append(vers, deltaVer{recSeq(seg.data, off), seg, off})
		}
	}
	for l := sh.dirGet(h); l.valid(); {
		seg := sh.segForLoc(l)
		if seg == nil {
			break
		}
		add(seg, l.off)
		l = recNext(seg.data, l.off)
	}
	sh.forEachSealedRecord(key, h, func(seg *segment, off uint32) bool {
		add(seg, off)
		return true
	})
	// Neither route is guaranteed to reach a key's OLDER versions. rebuildDir -- which runs on
	// every crash recovery, and on a clean reopen of a time-travel or parent/child collection --
	// relinks each bucket to only each key's CURRENT record, and forEachSealedRecord skips the
	// active segment by construction. A chain living inside the active segment, which is the
	// normal shape, therefore loses its base and the key silently vanishes: Get says not-found
	// while Has says true and a scan yields a fragment.
	//
	// So when the routes above produced no whole record, fall back to reading the active
	// segment directly. Chains never span a seal, so that is the only place the rest of one can
	// be. Bounded to the case that actually needs it: with the directory intact this never runs.
	if !sh.hasFull(vers) && sh.act != nil {
		seg := sh.act
		for off := uint32(0); off < uint32(seg.used); {
			tl := recTotalLen(seg.data, off)
			if tl == 0 || off+tl > uint32(seg.used) {
				break
			}
			// recIsMarker, not recKeyLen()&markerFlag: recKeyLen MASKS the flag bits off, so that
			// test was always true and this walk never skipped a marker. Harmless as it stood --
			// a marker is keyless and add() matches on the key -- but it was not doing what it
			// said, and with a second flag now in the same field it would matter.
			if !recIsMarker(seg.data, off) {
				add(seg, off)
			}
			off += tl
		}
	}
	if len(vers) == 0 {
		return nil, nil, false
	}
	sort.Slice(vers, func(i, j int) bool { return vers[i].seq < vers[j].seq })

	// Locate the base -- the newest whole record -- from the record HEADERS, then decompress
	// only from there forward. Establishing it by decompressing newest-first and stopping at the
	// first whole record read the same set of records, but it could not know it had failed until
	// it had decompressed every version.
	base := -1
	for i := len(vers) - 1; i >= 0; i-- {
		if !recIsDelta(vers[i].seg.data, vers[i].off) {
			base = i
			break
		}
	}
	if base < 0 {
		return nil, nil, false // no full record: see above, do not guess
	}
	// Decompress the participants into reused buffers. They all have to be live at once (the
	// merge reads every one), so this is one buffer per chain position, reused across calls rather
	// than allocated per call -- which at DeltaMax deltas of a multi-KB ad was 9% of a real queue
	// replay's allocation. The pooled state therefore retains up to DeltaMax ad buffers per pooled
	// instance, which is the memory this trades for the churn.
	ds, _ := decompressPool.Get().(*decompressState)
	if ds == nil {
		ds = &decompressState{}
	}
	defer decompressPool.Put(ds)
	raws := ds.grow(len(vers))
	for i := base; i < len(vers); i++ {
		stored, codec, ok := segStoredOrReassembled(c, vers[i].seg, vers[i].off)
		if !ok {
			return nil, nil, false
		}
		raw, err := codec.Decompress(ds.bufs[i][:0], stored)
		if err != nil {
			return nil, nil, false
		}
		ds.bufs[i] = raw
		// The header said what this record is; the payload says so independently. They can only
		// disagree if a copy path lost or invented a flag, and the consequence of trusting the
		// header then is either merging onto a fragment or dropping every delta after it -- both
		// silent. Checked here because these bytes are decompressed anyway, so it is free.
		if isDeltaRecord(raw) != (i > base) {
			deltaFlagMismatches.Add(1)
			return nil, nil, false
		}
		raws[i] = raw
	}
	// Bytes only: splice the entry bytes and never build an object. Splicing and then decoding
	// for a caller that wanted an object is no cheaper than decoding and merging -- measured as a
	// wash -- which is why this is gated on what the caller asked for rather than always tried.
	if want == mWire {
		if spliced, ok := c.spliceMerge(dst, raws[base:]); ok {
			return spliced, nil, true
		}
	}
	if want&mObj != 0 {
		objectMerges.Add(1)
	}
	full, err := c.decodeWire(raws[base])
	if err != nil {
		return nil, nil, false
	}
	merged := classad.FromAST(full)
	for i := base + 1; i < len(vers); i++ {
		d, derr := c.decodeWire(raws[i])
		if derr != nil {
			return nil, nil, false
		}
		mergeDeltaAST(merged, d)
	}
	// Encode only for a caller that wants bytes. A point read does not: it takes the object, and
	// encoding the merged ad so that its caller could decode it again was the round trip this
	// whole path exists to avoid.
	if want&mWire == 0 {
		return nil, merged, true
	}
	return c.encodeAdInto(dst, merged.AST()), merged, true
}

// readWant is the form a point read needs: the object, except for a redacting read, which must
// take the bytes because redaction happens in the decode.
func readWant(redact bool) materializeWant {
	if redact {
		return mWire
	}
	return mObj
}

// materializeWant is the set of forms a materialize caller needs back.
type materializeWant uint8

const (
	mWire materializeWant = 1 << iota // the merged ad as wire bytes
	mObj                              // the merged ad as an object
)

// decompressState holds one reusable decompression buffer per chain position, for materializeAt.
type decompressState struct {
	bufs [][]byte
	raws [][]byte
}

// grow sizes the state for a chain of n participants and returns the raws slice to fill.
func (d *decompressState) grow(n int) [][]byte {
	for len(d.bufs) < n {
		d.bufs = append(d.bufs, nil)
	}
	if cap(d.raws) < n {
		d.raws = make([][]byte, n)
	}
	d.raws = d.raws[:n]
	clear(d.raws)
	return d.raws
}

var decompressPool = sync.Pool{New: func() any { return &decompressState{} }}

// mergeState is spliceMerge's reusable scratch: the wire splicer's per-call state plus the
// overlay slice it takes. Pooled because materializeAt runs concurrently under the shard read
// lock, and because the whole point of splicing is to stop allocating per merge.
type mergeState struct {
	sc       wire.MergeScratch
	overlays []wire.Ad
}

var mergePool = sync.Pool{New: func() any { return &mergeState{} }}

// spliceMerge merges a chain -- raws[0] the whole record, the rest its deltas oldest-first -- by
// copying attribute entries rather than decoding them, and returns the merged wire bytes.
//
// It reports false when the splice does not apply, and the caller falls back to decoding, which
// is the source of truth: an interned or standalone ad names its attributes by an id into a table
// the other participants do not share, so entries cannot move between ads.
func (c *Collection) spliceMerge(dst []byte, raws [][]byte) ([]byte, bool) {
	if len(raws) == 0 || !c.inline {
		return nil, false
	}
	// No shortcut for a chain of one. Returning raws[0] directly would hand the caller a slice of
	// materializeAt's REUSED decompression buffer, which the next merge overwrites -- and the
	// collapse retains what it is given until its commit lands. The splice below copies.

	ms, _ := mergePool.Get().(*mergeState)
	if ms == nil {
		ms = &mergeState{}
	}
	defer func() {
		// Clear before pooling, do not just truncate: the scratch holds slices of the
		// DECOMPRESSED participant ads, and a pooled struct that keeps those pointers alive
		// pins one ad per overlay per pooled instance for as long as the pool holds it. Length
		// zero does not drop what the backing array still references.
		ms.overlays = ms.overlays[:0]
		clear(ms.overlays[:cap(ms.overlays)])
		ms.sc.Release()
		mergePool.Put(ms)
	}()
	ms.overlays = ms.overlays[:0]
	for _, r := range raws[1:] {
		ms.overlays = append(ms.overlays, wire.Ad(r))
	}
	out, ok := wire.AppendAdMergedInline(dst, wire.Ad(raws[0]), ms.overlays, &ms.sc)
	if !ok {
		spliceFallbacks.Add(1)
		return nil, false
	}
	spliceMerges.Add(1)
	return out, true
}

// The three ways a delta merge can be served. Reported together because any one of them alone
// misleads: a store whose merges all fall back is paying for the splice attempt and getting the
// decode, and a store whose callers all want objects never attempts a splice at all -- both read
// back perfectly correctly, so nothing else would notice.
var (
	spliceMerges    atomic.Int64 // spliced entry bytes
	spliceFallbacks atomic.Int64 // splice attempted and refused (interned/standalone/malformed)
	objectMerges    atomic.Int64 // decoded and merged into an object, because the caller wanted one
)

// SpliceStats reports how delta merges were served: by splicing attribute bytes, by decoding
// after the splice refused, and by decoding because the caller asked for an object rather than
// bytes (a point read, or a collapse in a collection with an ordered index or a watcher).
func SpliceStats() (spliced, spliceRefused, objectPath int64) {
	return spliceMerges.Load(), spliceFallbacks.Load(), objectMerges.Load()
}

// DeltaStats reports how many records this collection has stored as deltas versus in full
// since it was opened. Both zero means delta mode is off (or nothing has been written).
func (c *Collection) DeltaStats() (deltas, fulls int64) {
	if c.deltas == nil {
		return 0, 0
	}
	return c.deltas.deltas.Load(), c.deltas.fulls.Load()
}

// resolveDelta is the shared tail of the point-read paths (shard.get and shard.getAt) once a
// record's stored bytes are in hand. It reports "handled" only for a record that actually IS a
// delta; for everything else the caller proceeds exactly as it did before delta records existed.
//
// isDelta comes from the record HEADER (recIsDelta), read by the caller which has the segment and
// offset. It used to be established here by decompressing the record and looking at its payload,
// which meant that turning delta mode on made every point read of every ordinary record pay a
// decompression it had no use for. A store's records are mostly ordinary, so that was the common
// case.
//
// The third result is the merged ad as an OBJECT, which materializeAt built to produce the bytes.
// A caller wanting an object should take it rather than decode the bytes back -- but never for a
// redacting read, since the object holds every sealed value open.
//
// Callers must hold the shard read lock: materializeAt reads other segments' bytes.
func (sh *shard) resolveDelta(c *Collection, key []byte, h, s0 uint64, isDelta bool, stored []byte, codec Codec, want materializeWant) ([]byte, Codec, *classad.ClassAd, bool, bool) {
	// nil dst: a point read's bytes are copied out from under the shard lock by its caller.
	if !c.deltaRead {
		return nil, nil, nil, false, true // not handled here; caller proceeds as before
	}
	if !deltaHdrDispatch { // see deltaHdrDispatch: the pre-header dispatch, for the A/B
		raw, err := codec.Decompress(nil, stored)
		if err != nil {
			return nil, nil, nil, true, false
		}
		if !isDeltaRecord(raw) {
			return raw, identityCodec{}, nil, true, true
		}
	} else if !isDelta {
		return nil, nil, nil, false, true
	}
	merged, mergedAd, ok := sh.materializeAt(c, key, h, s0, want, nil)
	if !ok {
		return nil, nil, nil, true, false
	}
	return merged, identityCodec{}, mergedAd, true, true
}

// materializeOpenChains writes a full record for every key that currently ends in a delta,
// so that afterwards no surviving delta depends on an older version.
//
// This is how the one hard invariant of delta records is kept: replay needs the base to still
// be there, and compaction is free to DROP superseded versions once they fall below its retain
// floor. Rather than teach compaction to reconstruct fragments -- inside a function that reads
// segment bytes off-lock and already carries two production SIGSEGV scars -- the chains are
// collapsed from outside first, using nothing but ordinary reads and writes. After this every
// key's newest record is self-contained and every delta behind it is superseded, so whatever
// compaction reclaims, what remains is readable.
//
// It is called at the head of Compact, which is rare. The cost is one full write per key with
// an open chain, which is the write that delta mode deferred in the first place.
// collapseBeforeRewrite collapses every live delta chain ahead of anything that REWRITES
// records. It must be called by every route into compactShard -- Compact, RetrainDict, and the
// codec-change path -- not just Compact.
//
// Two separate hazards, either of which is permanent:
//   - compactShard drops superseded versions below the retain floor, so a surviving delta can
//     lose the whole record it merges from and the key becomes unreadable.
//   - its interning re-encode (compact.go, appendInterned) rebuilds a record from its AST and
//     does not carry the wire flags, so a delta comes out looking like a complete ad. That is
//     not a read-time miss that replay can recover; it is corruption written back to disk.
//
// RetrainDict reached compactShard with no collapse at all, which was reproduced as 40 of 40
// keys permanently truncated from 45 attributes to 5.
func (c *Collection) collapseBeforeRewrite() {
	// deltaRead, not deltas/deltaMax: a store REOPENED without DeltaMax still holds delta
	// records, and every guard in this feature used to key on the write-side variables -- so
	// such a store silently stopped collapsing before compaction and re-enabled
	// columnarization, which is precisely where its fragments get rewritten as whole ads.
	if !c.deltaRead {
		return
	}
	if c.deltas == nil {
		c.deltas = newDeltaTracker() // read-only reopen: still needs somewhere to track a collapse
	}
	c.collapseLiveChains()
}

// encodePatchOnly produces the record bytes for a write buffered as a bare patch (see
// Txn.PatchAttrs), where no whole ad was ever read or built.
//
// The fast path encodes the patch as a delta and touches nothing else -- which is the entire
// point: the write costs the encoding of the attributes it changed, not of the ad they belong
// to. The slow path is taken when a delta will not do, and only there is the stored ad read
// and merged: on a key's first write, when the chain has reached its bound, and when the write
// removed an attribute (which a delta of present attributes cannot express). So a read happens
// once per re-materialization rather than once per update.
func (tx *Txn) encodePatchOnly(dst []byte, b *txnBuf, h uint64) ([]byte, bool) {
	c := tx.c
	eligible := len(b.removed) == 0 && b.patch != nil
	if eligible && c.deltaMax > 0 && c.deltas != nil && c.inline && c.canWriteDelta() {
		if c.deltas.next(h, true, c.keyExists(b.key, h), c.deltaMax) {
			return wire.EncodeInlineDelta(dst, b.patch.AST(), nil, c.shouldEncrypt, c.sealer), true
		}
	} else if c.deltas != nil {
		// Record the decision so the chain restarts here even when delta mode declined.
		c.deltas.next(h, false, false, c.deltaMax)
	}
	// A whole record is required: compose it from the stored ad plus this write's changes.
	// b.ad is filled in so the rest of Commit (ordered-index maintenance, watch publication)
	// sees the same object it would have for an ordinary Put.
	storeReads.Add(1)
	ad, ok := tx.readStored(b.key)
	if !ok {
		ad = classad.New()
	}
	mergeDelta(ad, b.patch)
	for _, n := range b.removed {
		ad.Delete(n)
	}
	b.ad = ad
	return c.encodeAdInto(dst, ad.AST()), false
}

// deltaModeFile records that a store has written delta records, so a later open knows it must
// replay them whatever DeltaMax it was given. See initDeltaMode.
const deltaModeFile = "deltamode"

// deltaModeVersion is the marker's content: which of the two possible delta ENCODINGS the
// store's records use.
//
//	"1" -- pre-release: the only record of being a delta is a flag inside the record's
//	       compressed payload, so classifying a record required decompressing it.
//	"2" -- current: the record HEADER carries deltaFlag as well, so a reader classifies without
//	       decompressing (and the payload flag remains as a cross-check).
//
// A "1" store cannot be read by this code: its delta records have no header flag, so every one
// of them would be taken for a whole ad and served as a fragment -- exactly the failure the
// marker exists to prevent. It is refused at open instead. Only a build from the unmerged
// delta-records branch could have written one.
const deltaModeVersion = "2"

// errPreReleaseDeltaStore reports a store whose delta records predate the header flag.
var errPreReleaseDeltaStore = errors.New(
	"collections: store contains pre-release delta records (deltamode marker \"1\") that this build cannot read; " +
		"it was written by an unreleased build of the delta-records branch and must be rebuilt")

// initDeltaMode reconciles the configured DeltaMax with what the store on disk already holds.
//
// Writing deltas is a choice this process makes (deltaMax); READING them is not optional -- a
// store that contains even one delta must replay for the rest of its life, or it silently
// serves fragments. So the marker is written when delta mode is enabled and, once present,
// turns replay on regardless. Turning DeltaMax back to 0 therefore stops NEW deltas being
// written while leaving the existing ones readable, which is what makes the option safe to
// change and safe to roll back.
func (c *Collection) initDeltaMode(dir string) error {
	path := filepath.Join(dir, deltaModeFile)
	if b, err := os.ReadFile(path); err == nil {
		if v := string(bytes.TrimSpace(b)); v != deltaModeVersion {
			// Refusing to open beats opening and serving fragments. See deltaModeVersion.
			return errPreReleaseDeltaStore
		}
		c.deltaRead = true
		c.deltaMarked.Store(true) // already recorded; do not rewrite it
	} else if !os.IsNotExist(err) {
		return err
	}
	// The marker is NOT written here. It records that the store CONTAINS deltas, and enabling
	// the option is not the same as storing one -- writing it at open meant that setting
	// DeltaMax once, even if no delta was ever stored, turned replay on permanently with no
	// supported way back. It is written on the first actual delta write instead (see
	// markDeltaMode), which makes the option genuinely reversible: set it back to 0 and a store
	// that never wrote a delta is byte-for-byte an ordinary one.
	c.deltaDir = dir
	return nil
}

// markDeltaMode records, once and durably, that this store now contains delta records -- so a
// later open replays them whatever DeltaMax it is given. Called on the first delta write.
//
// Durability matters more here than anywhere else in the feature: this file decides whether the
// store's records are legible at all. Lose it and every delta is served as a whole ad, which is
// the 45-attributes-becomes-1 failure it exists to prevent, so it goes down via
// tmp+fsync+rename+dir-fsync rather than a bare os.WriteFile. (basecodec, which this otherwise
// mirrors, has that gap; it is not a precedent worth copying.)
//
// Ordering: called BEFORE the delta record it describes is committed, so a crash can leave the
// marker without the record -- harmless, a store with no deltas reads identically either way --
// but never the record without the marker.
func (c *Collection) markDeltaMode() {
	if c.deltaDir == "" || c.deltaMarked.Load() {
		return
	}
	c.deltaMarkMu.Lock()
	defer c.deltaMarkMu.Unlock()
	if c.deltaMarked.Load() {
		return
	}
	if err := writeFileDurable(filepath.Join(c.deltaDir, deltaModeFile), []byte(deltaModeVersion)); err != nil {
		return // could not record it: the next write tries again rather than storing a delta
	}
	c.deltaMarked.Store(true)
}

// canWriteDelta reports whether a delta may be stored, recording the store's delta mode first.
// A delta must never reach disk before the marker that makes it legible.
func (c *Collection) canWriteDelta() bool {
	if c.deltaDir == "" {
		return true // in-memory: nothing to persist, nothing to recover
	}
	c.markDeltaMode()
	return c.deltaMarked.Load()
}

// TrackedKeys reports how many keys the delta tracker holds. The tracker is the one new
// in-memory structure delta records add -- one small entry per key that has been written -- so
// this is what an operator checks when asking what the feature costs in RAM, and what a test
// checks to confirm the bound is doing something.
func (c *Collection) TrackedKeys() int {
	if c.deltas == nil {
		return 0
	}
	c.deltas.mu.Lock()
	defer c.deltas.mu.Unlock()
	return len(c.deltas.depth)
}

// collapseSealedChains is the heart of the seal-scoped design: when a segment seals, every
// key whose chain is still open gets a whole record written, and the tracker is emptied.
//
// Two things follow, and together they are the reason to do it:
//
//   - No LIVE delta ever exists outside the active segment. Every fragment in a sealed
//     segment is superseded by the whole record written here, so anything that rewrites a
//     sealed segment -- columnarization above all -- can keep assuming each live record is a
//     whole ad. The sealed format is untouched.
//   - A chain can never span a seal, so the tracker only ever describes the ACTIVE segment
//     and can be dropped wholesale. Its size stops being a function of how many keys the
//     table holds and becomes a function of how many are touched between seals.
//
// Emptying the tracker is only safe BECAUSE of the first property: afterwards every live key
// provably has a whole record, so a missing tracker entry means "depth 0, base exists" rather
// than "no base known". Getting that backwards costs the entire optimization -- it forces a
// whole record per key per seal, which measured WORSE than not having deltas at all.
//
// Runs after a commit, never inside one: collapsing reads and writes keys, and the seal that
// triggers it happens under the shard write lock.
func (c *Collection) collapseSealedChains() {
	if c.deltas == nil {
		// Delta mode off: nothing to collapse, but the seal bookkeeping still has to be
		// drained. writeRecord appends to pendingSeal unconditionally, and leaving it to grow
		// retains a pointer to every segment the shard has ever sealed -- including ones
		// compaction has retired and unmapped. That is a leak in the DEFAULT configuration,
		// which is exactly where it must not be.
		for _, sh := range c.shards {
			if sh.sealedPending.Swap(false) {
				sh.mu.Lock()
				sh.pendingSeal = nil
				sh.mu.Unlock()
			}
		}
		return
	}
	pending := false
	for _, sh := range c.shards {
		if sh.sealedPending.Load() {
			pending = true
			break
		}
	}
	if !pending || !c.collapsing.CompareAndSwap(false, true) {
		// Deliberately does NOT clear sealedPending before the CAS. Clearing it first meant a
		// committer that sealed while a collapse was in flight consumed its own flag and then
		// returned, so that seal was never collapsed and a live delta stayed in a sealed
		// segment -- silently breaking the invariant the whole design rests on. The flags are
		// consumed only by the goroutine that wins the CAS, in liveDeltaKeys.
		return
	}
	defer c.collapsing.Store(false)

	c.collapseLiveChains()
}

// collapseLiveChains writes a whole record for every key whose current record is a delta, and
// empties the tracker. It is the one operation behind both the seal collapse and the
// pre-compaction pass: in each case the requirement is the same -- afterwards no live fragment
// depends on anything that is about to be sealed away or reclaimed.
//
// The keys come from the active segments rather than the tracker, which is keyed by hash and
// cannot name them. That is not a workaround: by this function's own invariant the active
// segment is the only place a live fragment can be.
func (c *Collection) collapseLiveChains() {
	open := c.liveDeltaKeys()
	c.deltas.mu.Lock()
	clear(c.deltas.depth) // the tracker describes the active segment, which is now collapsed
	c.deltas.mu.Unlock()

	for _, key := range open {
		// Retry a lost race rather than abandoning the key. A conflict means a concurrent writer
		// committed between this transaction's snapshot and its commit; the newer version is now
		// current and may itself be a delta, so the answer is to redo the read-modify-write, not
		// to skip. Skipping was silently permanent: see collapseRetry.
		done := false
		for attempt := 0; attempt < collapseAttempts && !done; attempt++ {
			done = c.collapseOneKey(key)
		}
		if !done {
			// Still not collapsed. Remember it so the next pass tries again; a key that ends in
			// a delta in a sealed segment is exactly what must not be left behind.
			c.rememberCollapse(key)
		}
	}
}

// collapseAttempts bounds the immediate retries of one key's collapse. A conflict needs a
// concurrent writer to that same key, so a couple of attempts covers it; anything that survives
// this goes on the retry set for the next pass rather than spinning here, because the collapse
// runs on the way out of a commit and must not block on a contended key.
const collapseAttempts = 3

// rememberCollapse records a key whose collapse did not complete, for a later pass to retry.
func (c *Collection) rememberCollapse(key []byte) {
	c.collapseMu.Lock()
	if c.collapseRetry == nil {
		c.collapseRetry = map[string]struct{}{}
	}
	c.collapseRetry[string(key)] = struct{}{}
	c.collapseMu.Unlock()
	collapseDeferred.Add(1)
}

// collapseDeferred counts collapses postponed to a later pass. It should be near zero; a climbing
// count means keys are repeatedly losing the race and their chains are living in sealed segments
// in the meantime.
var collapseDeferred atomic.Int64

// CollapseDeferrals reports how many delta-chain collapses have been postponed to a later pass.
func CollapseDeferrals() int64 { return collapseDeferred.Load() }

// collapseRaceHook, when non-nil, runs inside collapseOneKey after the transaction's snapshot is
// taken and before it commits -- the window a concurrent writer has to make the collapse conflict.
// Test-only (nil in production): the race is otherwise not reachable on demand, and what it
// guards is the invariant that no live delta outlives its segment's seal.
var collapseRaceHook func(key []byte)

// collapseOneKey attempts a single collapse of key, reporting whether the key is done -- either
// collapsed, or no longer in need of it (deleted, or already whole). A false return means the
// caller must try again: the key still ends in a delta.
func (c *Collection) collapseOneKey(key []byte) bool {
	{
		// Read and write under ONE transaction, so the snapshot is taken before the read and a
		// commit that lands in between actually conflicts. Reading with c.Get first and opening
		// the transaction afterwards captured the snapshot AFTER the value, so the conflict
		// check could never fire and the collapse wrote a stale whole ad over a newer update --
		// reproduced as 16 of 16 keys losing committed increments.
		tx := c.Begin()
		idx := c.shardOf(key, c.h.Hash(key))
		sh := c.shards[idx]
		// snapOf takes the shard read lock itself, so it must run BEFORE this one: Go's RWMutex
		// is not reentrant for RLock, and a writer arriving between the two acquisitions
		// deadlocks the pair. Caught by the concurrent-writer test, which hung.
		s0 := tx.snapOf(idx)
		sh.mu.RLock()
		// Bytes are what the collapse stores. The object is wanted only by the two things that
		// ask Commit for one -- ordered-index maintenance and watch publication -- so asking for
		// it unconditionally would force the decode-and-encode route on every collapse in a
		// collection that has neither.
		want := mWire
		if c.hasOrdered() || sh.hub.watching() {
			want |= mObj
		}
		// nil dst: the collapse buffers these bytes until its commit lands.
		merged, mergedAd, ok := sh.materializeAt(c, key, c.h.Hash(key), s0, want, nil)
		sh.mu.RUnlock()
		if !ok {
			// Deleted underneath us, or already whole -- both mean nothing to collapse. But
			// "materialize failed" is not the same as "no longer a delta": a damaged chain also
			// lands here, and dropping it would leave the fragment sealed. Ask the store.
			return !c.endsInDelta(key)
		}
		// The merged WIRE BYTES, written straight through. Going via Get would decode these
		// same bytes into a ClassAd purely so Put could encode them again -- a round trip per
		// chain per seal, which profiled at 57% of an ingest run.
		//
		// mergedAd rides along because materializeAt built it to produce those bytes. Commit
		// asks for the object (ordered-index maintenance, watch publication) and, given only
		// bytes, decoded them -- reinstating the same round trip from the other side, at 14% of
		// a real queue replay's allocation. Handing over what we already have costs nothing.
		if collapseRaceHook != nil {
			collapseRaceHook(key)
		}
		tx.putWireAd(key, merged, mergedAd, c.decodeWireAd)
		if r := tx.Commit(); r.Conflicted() {
			return false // a newer writer won; the caller retries against the newer version
		}
		return true
	}
}

// endsInDelta reports whether key's CURRENT record is a delta, resolving the location without
// decoding anything. It answers the question the collapse needs when a materialize fails: "is
// there still a fragment here to worry about", as distinct from "did the merge work".
func (c *Collection) endsInDelta(key []byte) bool {
	h := c.h.Hash(key)
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	l, ok := sh.findCurrent(sh.dirGet(h), key)
	if !ok {
		if l, ok = sh.lookupSealed(key, h); !ok {
			return false // no live record at all
		}
	}
	seg := sh.segForLoc(l)
	return seg != nil && recIsDelta(seg.data, l.off)
}

// keyExists reports whether key has a live record, resolving its location without decoding
// anything. It is what replaces the tracker's old "have I written a base for this key"
// bookkeeping: after a seal-collapse the tracker is empty, so the question has to be asked of
// the store -- but only the cheap half of it. A key with no record must get a whole record,
// since a delta chained to nothing is unreadable.
func (c *Collection) keyExists(key []byte, h uint64) bool {
	sh := c.shards[c.shardOf(key, h)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	// findCurrent, not findVisible with seqMax: a LIVE record carries supersededBySeq ==
	// seqMax, and findVisible asks for supersededBySeq > s0, so a seqMax snapshot matches
	// nothing at all. That mistake made every write look like a new key and turned every
	// delta back into a whole record -- silently, since the results stay correct.
	if l, ok := sh.findCurrent(sh.dirGet(h), key); ok {
		return sh.segForLoc(l) != nil
	}
	l, ok := sh.lookupSealed(key, h)
	return ok && sh.segForLoc(l) != nil
}

// liveDeltaKeys returns the keys whose CURRENT record is a delta, read from the active
// segments. The tracker is keyed by hash and so cannot name them; the active segments can,
// and by the seal-collapse invariant they are the only place a live fragment exists.
func (c *Collection) liveDeltaKeys() [][]byte {
	// Keys a previous pass could not finish come first: their segments are already sealed and
	// nothing else will ever scan them again (pendingSeal is consumed below), so this set is the
	// only thing standing between a lost collapse and a permanent live delta in a sealed segment.
	var out [][]byte
	c.collapseMu.Lock()
	for k := range c.collapseRetry {
		out = append(out, []byte(k))
	}
	clear(c.collapseRetry)
	c.collapseMu.Unlock()

	for _, sh := range c.shards {
		// Resolve AND read under one unbroken read lock, with no segment pointer carried
		// across a release. Retirement happens under the write lock and the munmap runs after
		// it is dropped, so a pointer captured in one locked section and dereferenced in the
		// next is a use-after-munmap -- reproduced as a SIGSEGV in recTotalLen. The flags are
		// consumed here, by the collapse that is about to act on them.
		sh.mu.Lock()
		sh.sealedPending.Store(false)
		segs := append([]*segment(nil), sh.pendingSeal...)
		sh.pendingSeal = nil
		if sh.act != nil {
			segs = append(segs, sh.act)
		}
		for _, seg := range segs {
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if !recIsMarker(seg.data, off) && recSuperseded(seg.data, off) == seqMax {
					sealWalkRecords.Add(1)
					if segRecIsDelta(seg, off) {
						sealWalkDeltas.Add(1)
						out = append(out, append([]byte(nil), recKey(seg.data, off)...))
					}
				}
				off += tl
			}
		}
		sh.mu.Unlock()
	}
	return out
}

// writeFileDurable writes data to path so that it survives a crash: a temp file in the same
// directory, fsync'd, renamed into place, and the directory fsync'd so the rename itself is
// durable.
func writeFileDurable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil // the rename landed; an unopenable dir is not worth failing the open over
	}
	defer d.Close()
	return d.Sync()
}

// deltaVer is one version of a key: where it lives and when it was committed.
type deltaVer struct {
	seq uint64
	seg *segment
	off uint32
}

// errNoWireDecoder reports a wire-buffered write whose object was asked for without a decoder
// having been supplied. Only putWire creates such a buffer, and it always supplies one.
var errNoWireDecoder = errors.New("collections: wire-buffered write has no decoder")

// decodeWireAd is decodeWire returning the public ClassAd wrapper, for callers that need the
// object rather than the AST (see Txn.putWire).
func (c *Collection) decodeWireAd(w []byte) (*classad.ClassAd, error) {
	a, err := c.decodeWire(w)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(a), nil
}

// hasOrdered reports whether the collection maintains any ordered index. Callers use it to
// skip preparing input that only ordered-index maintenance consumes.
func (c *Collection) hasOrdered() bool { return len(c.ordered) > 0 }
