# Design note: mmap-backed indexes for sealed segments (live + in-memory)

Status: plan. Stacked on the MPH work (sidecar v6, PR #27).

## Motivation

A live `Collection` (persistent or purely in-memory, e.g. the htc-collector) keeps each
segment's index as an in-RAM `segIndex`: `map[string]*roaring.Bitmap` and friends. That is
fast (O(1) hash equality, no page faults) but has three costs that grow with the store:

1. **Non-reclaimable heap.** The index is committed Go heap. As the store grows the index
   grows with it, and there is a hard cliff: exceed RAM and the process OOMs. Nothing about
   it is evictable.
2. **GC tax.** The index is millions of pointers (map buckets, per-value bitmaps, roaring
   containers) that the collector traverses every cycle. Mark cost and pause pressure scale
   with index size. (The *data* arenas are large pointer-free byte slices — cheap to mark;
   the index is where the traversal cost lives.)
3. **Slow start.** Rebuilding (or, post-#26, CLIX-deserializing) the index on `Open` is
   O(records or bitmaps).

A sealed segment is immutable until compacted, exactly like an archive segment. The archive
already stores an immutable index as a **sidecar read zero-copy over an mmap**
(`mmapSegIndex`): O(#attributes) resident, demand-paged, GC-invisible (one `[]byte`), and
O(1) to "load" (just map it). Adopting that representation for sealed live segments fixes all
three costs. The MPH (v6) closes the one historical gap (binary-search equality), so a
sealed sidecar's equality is now competitive with the hash map.

## The representation matrix

One reader (`mmapSegIndex`), three backings; the active (mutable) segment stays in-RAM.

| segment | index |
|---|---|
| active (write frontier, mutable) | in-RAM `segIndex` (heap) — small, bounded |
| sealed, persistent | sidecar bytes = **file** mmap (reclaimable from disk) |
| sealed, in-memory (collector) | sidecar bytes = **anonymous** mmap (off-heap, `MADV_FREE`-able) |

The reader is identical; only the byte source differs. For the in-memory case the anonymous
mapping takes the index **off the Go heap** — out of GOGC pacing and RSS, zero mark cost —
which is the GC win the collector wants. (Reclaim is weaker for a pure-memory DB: eviction
means swap, usually undesirable; the GC benefit is the point.)

**Pinning:** we do NOT need a separate in-RAM "pinned" mode. A file-backed sidecar can be
`mlock`'d to hold it resident. So there is one representation, optionally pinned, rather than
two.

## The discovery: the sidecar must carry segStats (v7)

The live query planner uses a richer index surface than the archive query does:

- archive: `covers`, `candidateOffsets`, `probeOffsets` (+ external zone-map prune).
- live: the above **plus** `selectivityOrder`, `estCandidates`, `canSkip`, `skipsPrefix`,
  and DNF `coversGroups` / `candidateOffsetsGroups`.

Every one of the extra methods reads `segStats` — min/max (value range skip), top-N + ndv
(selectivity estimate), bloom (categorical skip). The sidecar (v6) serializes postings +
bloom + MPH but **not** min/max/top-N/ndv/HLL; the live tier recomputes those with
`finishStats`, which for a mmap index would page in every bitmap on load — defeating the
pageable design.

So a `mmapSegIndex` cannot answer `canSkip`/`estCandidates`/`selectivityOrder` today.
Swapping sealed segments to it without fixing this would silently drop segment-skip and
selectivity ordering for sealed data (e.g. a range query could no longer skip out-of-range
sealed segments) — a real regression.

**Fix:** serialize a per-attribute stats block into the sidecar (**v7**): `covered`, `exc`,
`ndv`, `hasRange`, `min`, `max`, the top-N heavy hitters, and the HLL registers (the bloom is
already there since v5). It is tiny and bounded (a few scalars + top-N + ~1 KiB HLL per attr)
and lets `mmapSegIndex` reconstruct a faithful `segStats` cheaply, without touching postings.

## Interface

Extract the live planner's index surface into an interface both `*segIndex` and
`*mmapSegIndex` satisfy (they mostly mirror already; `mmapSegIndex` gains the stats-backed
methods from v7 and the DNF-group variants). The segment holds it atomically; the active
segment stores a `*segIndex`, a sealed segment a `*mmapSegIndex`. The query path dispatches
through the interface with no per-segment special-casing (it already tolerates heterogeneous
segments).

## Correctness

Unchanged invariants: a sealed segment is immutable until compacted (compaction produces a
new file → new sidecar); MVCC supersession is re-checked from record bytes, not the index;
the query re-verifies every candidate, so any index only affects selectivity, never the
answer. The MPH already falls back to the authoritative sorted run. So this is a
representation change, not a semantics change.

## Supersedes CLIX

The CLIX snapshot (#26) restores an in-RAM `segIndex` on `Open`. With sealed segments mapping
their sidecar instead, the CLIX write/restore path is removed: `Open` maps sidecars (O(1))
rather than deserializing. `writeSidecarIndex` (the archive format, now v7) is written at
seal for live segments too.

## Incremental steps — all landed

1. ✅ **Sidecar v7 — segStats block.** Serialize per-attr stats; `mmapSegIndex` readers +
   `statsFor`. Round-trip test: mmap stats == in-RAM `finishStats` stats. *(Later: v8 CRC.)*
2. ✅ **mmapSegIndex full surface.** `canSkip`, `skipsPrefix`, `estCandidates`,
   `selectivityOrder`, DNF group variants on the v7 stats. Parity tests vs `segIndex`.
3. ✅ **Read-index interface + dispatch.** `readIndex`/`indexPrimitives` extracted; segment
   holds either (`idx` or `msidx`); planner dispatches via `readIdx()`. Shared planner logic
   as free functions so the two tiers can't diverge.
4. ✅ **Backing store.** `mapFile` + anonymous `mapAnon`; unmap on reap via `onReap`.
5. ✅ **Seal path.** `sealSegmentIndex`: on seal write the sidecar, map it (file/anon by store
   kind), publish `msidx` via CAS, register unmap with reap, drop the heap copy. Active
   stays in-RAM. Option A: a converted segment's index config freezes.
6. ✅ **Open path + CLIX removal.** `loadSealedIndex` maps sidecars for sealed segments (CRC +
   spec-gen + extent checked); `index_snapshot.go` (CLIX) removed.
7. ✅ **Accounting + doc.** `Collection.SidecarSizes()` reports the live store's sealed
   sidecars (mapped/evictable, MPH+Bloom broken out); `IndexSizes` now measures only the
   in-RAM active segments; design README §8/§10 updated.
8. ✅ **In-memory anon flip (the collector's GC win).** Sealed RAM segments seal their index
   into an **anonymous** mmap sidecar (`sealSegmentIndexAnon`, off the Go heap), gated on
   in-memory + mmap-supported + indexes configured at construction (`shard.sealRAM`). The
   crux was **pin/reap for RAM segments**: a plain RAM segment relies on the GC and skips
   pin/reap, but an anon mapping is not GC-managed, so a scan reading it could race a
   compaction that unmaps it. Fix: a RAM segment that carries an anon sidecar is made
   pin/reap-eligible **from birth** (`segment.pinReap`, set in `allocSeg`/compaction for a
   sealing shard), so `segment.mapped()` (`file != nil || pinReap`) drives pin/unpin/retire/
   closeUnmap uniformly — the pin at scan start unconditionally brackets any later `msidx`
   read, exactly as `file != nil` does for a persistent segment. `Collection.Close` now
   routes through the pin-aware `closeUnmap` so the anon sidecars unmap (and, as a bonus, the
   persistent path stops leaking its `.idx` mapping at Close). `TestSealedSegmentsGoAnonMmap`
   covers it; full suite + race clean.

## Follow-ups (not in this work)

- **Data arenas off-heap for the in-memory DB.** A separate, larger GC win (the arenas are
  pointer-free → pacing/RSS/mark cost across the whole record body, not just the index).
  Anonymous-mmap the RAM segment backing itself. The pin/reap-for-RAM machinery added in
  step 8 is a prerequisite (a scan reading anon record bytes must pin them too).
- **`mlock` knob** for latency-critical stores that want the sidecar pinned resident.
