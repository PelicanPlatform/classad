# Design: the ClassAd history / archive store

> Status: **implemented (H1–H5)** in `collections/archive*.go`. Built on the mmap
> segment backend + epoch reclamation (`mmapseg.go`, `segment.pin/unpin/retire/reap`,
> `shard.snapshot`/`releaseWindows`). Companion to the in-memory `collections` store
> and the persistent-`Collection` effort; this document explains why the archive is
> a *sibling* store on the same engine rather than a mode of `Collection`.
>
> Delivered: append/seal/roll-over with immutable roaring sidecar indexes; segment
> catalog + zone maps with whole-segment pruning; O(segments) + CRC crash recovery;
> zone-prune → index → wire-native reverify queries, newest-first with LIMIT;
> age/size/count rotation via retire/reap. H5 sidecar indexes are a **fully
> demand-paged** v2 format: each attribute's postings are a sorted key run queried
> directly over the mmap — binary search for equality, boundary scan for range — so
> resident memory is O(#indexed attributes) regardless of value cardinality (keys,
> offset arrays, and roaring payloads all stay in the mapping; see `mmapSegIndex`).
> Indexes load lazily, so a segment queries never touch never pages its index in. A
> GlobalJobId point index was intentionally skipped — cluster.proc queries are served
> by the normal ClusterId/ProcId index.

## 1. Context

HTCondor keeps a **history file** (a.k.a. the *archive*): a linearly-written log of
ClassAds — typically completed job ads emitted by the schedd/negotiator. Its
defining properties:

- **Write-once.** Each ad is appended once and **never updated**.
- **Rotated by age.** Old data is deleted in bulk (whole files roll out), never
  per-record.
- **Larger than RAM.** By design the archive spans months/years and cannot be held
  resident. Any index — and even a key→location directory — must therefore be
  **persistable and pageable**, not an in-memory structure.
- **Time-ordered access.** `condor_history` overwhelmingly asks for *recent* or
  *time-bounded* ads, usually the *last K matching* a constraint (owner, cluster,
  job status), newest first.

The `collections` engine (dense `wire` encoding + `vm` query bytecode + per-segment
roaring indexing + mmap arenas) is a strong substrate for this. But the archive is
**not** the live store with a persistence flag flipped — see §3.

## 2. Goals / non-goals

**Goals**

- Append-only ingest at schedd rates; the tail is queryable immediately.
- Query with the existing `vm` engine and constraint semantics (three-valued
  logic, coercions) — identical results to a full scan.
- Time-bounded and "last K" queries that touch **O(relevant segments)**, not the
  whole archive, and terminate early.
- Indices and catalog that **persist**, restart in **O(segments)**, and page from
  disk when they exceed RAM.
- Bulk **rotation/retention** (by age, total size, or segment count) that is cheap
  (unlink) and safe against in-flight queries.

**Non-goals**

- Point-update or delete-by-key (the archive is immutable; rotation is the only
  deletion).
- Holding the working set in RAM (that is the live `Collection`'s regime).
- Cross-segment global secondary indexes as a v1 requirement (per-segment + a
  coarse segment-level summary suffices; see §4.3).

## 3. Key insight: simpler data model, harder systems problem

The archive **removes** the three most complex subsystems of the live store, and
that is exactly what pays for the scale problem:

| Live `Collection` | Archive |
|---|---|
| MVCC `seq`/`supersededBySeq` visibility | **None** — every record is live from write to rotation |
| Compaction (reclaim superseded garbage) | **None** — a sealed segment never contains garbage |
| Per-key tombstones / `Delete` | **None** — deletion is whole-segment drop by age |
| In-RAM directory `map[hash]→loc` | **Dropped** (or a single persistent `GlobalJobId` index) |
| Index rebuilt/mutated as data changes | **Immutable sidecar**, built once when a segment seals |

Because a segment is **sealed once and never mutated**, its index is *write-once*:
build it, `fsync` it next to the data, never invalidate it. Persisting an immutable
index is trivial compared to persisting a mutable one — this is the crux of why the
archive is easier where it counts.

What gets *harder* is purely the > RAM consequence: data, indices, and the
directory can each exceed memory, so every resident structure must either be
bounded (the catalog/zone maps) or pageable from disk (the indices).

**Conclusion:** model the archive as a **distinct store type** (`Archive`) that
shares the engine (`wire`, `vm`, the segment/mmap backend, the `segIndex` structure,
ZSTD+dict, epoch reclamation) but has its own policy layer. Bolting archive
semantics onto `Collection` as flags would erode the live-store invariants.

## 4. Design

### 4.1 Segment lifecycle: append → seal → immutable sidecar index

- One **active segment** is appended to (mmap-backed, as today via `mmapseg.go`).
  On reaching a roll-over threshold (size or wall-clock/time-span), it is **sealed**.
- Sealing triggers a one-shot **index build** (`buildSegIndex` over the immutable
  bytes) whose result is **serialized to a sidecar file** next to the segment and
  `fsync`'d. The in-memory `segIndex` may then be dropped and re-`mmap`'d on demand.
- The **active (unsealed) segment has no persisted index**; queries full-scan it.
  This reuses the existing "index covers `[0, upto)`, tail is full-scanned" model
  verbatim — the active segment is just an all-tail segment, bounded by the
  roll-over size, so the scan is cheap.

This maps cleanly onto today's per-segment index: the only additions are
*serialization* of `segIndex` and a *seal* trigger.

### 4.2 Segment catalog (manifest)

A small persistent **catalog** lists every segment: id, file path, byte length,
record count, sidecar-index path, and **zone maps** (§4.3). It is the recovered
state of the store.

- **Restart = load the catalog + `mmap` sidecars** → O(segments), not O(records).
  For 10⁹ ads at ~10⁵ ads/segment that is ~10⁴ segments; the catalog is tens of MB,
  comfortably resident.
- The catalog is the **commit point** for structural changes (seal, rotation): write
  the new catalog, `fsync`, then act on files. A torn catalog write recovers the
  prior consistent version.

### 4.3 Zone maps — the highest-leverage feature

For each segment, store per-attribute **min/max** for a few key attributes
(completion time, `ClusterId`, `JobStatus`, …) in the catalog. Because append order
≈ time order, a time-bounded query **prunes whole segments** without opening their
data *or* their index:

- `CompletionDate >= T` skips every segment whose `max(CompletionDate) < T`.
- Cost: ~16 bytes/attr/segment → a few MB for the whole archive, always resident.
- Zone maps are *ranges*, so they tolerate the mild out-of-order-ness of real
  completion timestamps (segments' time ranges may overlap slightly; pruning stays
  correct, just slightly less selective).

This is what turns "show me last week's jobs" from a 10⁴-segment scan into a
3-segment scan. Build it first; it dominates the win.

### 4.4 Query path

1. **Prune** the segment list by zone maps (§4.3) against the query's probes
   (reuse `vm.Query.Probes()`).
2. For each surviving segment, use its (mmap'd) sidecar `segIndex` to produce
   candidate offsets (`candidateOffsets`), exactly as `scanShardIndexed` does today;
   full-scan any segment lacking a usable index.
3. **Re-verify** each candidate with the full `vm` predicate (the existing
   superset+reverify model) and decode only matches.

Two archive-specific additions:

- **Newest-first iteration.** Visit segments (and, within the active tail, records)
  in reverse chronological order, because `condor_history` wants the most recent
  matches first.
- **LIMIT pushdown.** A `Limit(K)` on the iterator lets the scan **stop early** once
  K matches are yielded — critical when the constraint is broad but the caller only
  wants a page. The current forward, unbounded per-shard scan cannot do this; the
  archive iterator must thread a remaining-count and honor consumer stop (the
  `yield`-returns-false path already exists).

### 4.5 Pageable indices (the > RAM case) — delivered

A high-cardinality attribute (e.g. `GlobalJobId`) costs ~2–4 bytes/doc in roaring
*payload*, but the per-value lookup structure (map key + bitmap header per distinct
value) is ~100+ bytes/value on the Go heap — so a naive index materialized into maps
exceeds RAM well before the payloads do. The archive therefore does **not**
materialize any map. As implemented (`mmapSegIndex`):

- The v2 sidecar stores each attribute's postings as a **sorted key run** (a sorted
  `[]string`/`[]float64` + a parallel bitmap-offset array), written once at seal.
- Queries read it **directly over the mmap**: equality is a binary search, range is a
  boundary search plus a scan of only the matching keys. Roaring bitmaps are built
  lazily via zero-copy `FromBuffer` for just the postings a probe touches.
- Resident memory is therefore **O(#indexed attributes)** — one small directory entry
  per attribute — regardless of value cardinality. Keys, offset arrays, and payloads
  all stay in the demand-paged mapping. The catalog + zone maps remain the only always-
  resident structures.
- Indexes load **lazily** on first query, so a zone-pruned segment never maps its
  index. The mmap is unmapped through a `segment.onReap` hook, tied to the existing
  `pin/unpin/retire/reap` epoch machinery, so rotation `munmap`+`unlink`s an index
  only after any in-flight scan drops its pin — never a torn read.

This is Option A: a **separate** `mmapSegIndex` view for the immutable archive; the
live `Collection`'s mutable in-RAM `segIndex` (rebuilt by `Reindex`) is unchanged.

### 4.6 Rotation & retention

- **Policy**: retain by max age, max total bytes, or max segment count (configurable;
  evaluate on seal and/or a timer).
- **Mechanism**: drop the oldest segment = remove it from the catalog (the commit
  point), then `munmap`+`unlink` the segment and its sidecar index. This is exactly
  `segment.retire()` → deferred `reap()` when the pin count drains — a query
  scanning a rotating segment holds a pin and the unlink waits. **No new
  concurrency design needed**; it is the reclamation the persistent store already
  built, applied to age-based drop instead of compaction.

### 4.7 Writer model & partitioning

The live store hash-shards for write concurrency and point-lookup. For the archive
that is **counterproductive**: hash-sharding destroys time locality, so a
time-range query fans out to every shard and zone-map pruning loses its edge. The
archive is typically a **single appending writer**, so write concurrency is not the
constraint.

- Prefer a **single time-ordered log of segments** (or coarse partitioning by time
  window / by writer), so zone-map pruning and newest-first iteration are natural.
- Concurrency is reader-vs-rotation, which the pin/reap epoch model already handles.

### 4.8 The key directory

The in-RAM `map[hash]→loc` directory also does not fit at archive scale, and
point-lookup by key is rare here. Options, cheapest first:

- **No general directory.** Queries are constraint scans; `Get(key)` is unsupported
  or is itself a constraint query on `GlobalJobId`.
- **One persistent secondary index** on `GlobalJobId` only (a sidecar hash/sorted
  index), if direct fetch-by-id is required.

Recommend starting with *no directory* and adding the `GlobalJobId` index only if a
consumer needs it.

### 4.9 Density

Job ads have a **stable, repetitive schema**, so ZSTD + a **per-segment trained
dict** (already in `codec.go`) captures most of the redundancy without a shared,
persisted intern table — avoiding the recovery complexity a global string
dictionary would add. Inline attribute names (as the persistent `Collection` uses)
keep segments self-contained; the dict recovers the density. Measure inline+dict vs
a per-segment intern table on a real history sample before adding the latter.

## 5. What is reused vs new

**Reused unchanged**: the `wire` VALUE-node format and hot header; the `vm` bytecode
+ wire-native eval + `Probes()`; the `segIndex` postings structure and
`candidateOffsets`/superset+reverify; ZSTD + per-segment dict (`codec.go`); the
**mmap segment backend** (`mmapseg.go`); and the **epoch/refcount reclamation**
(`segment.pin/unpin/retire/reap`, `shard.snapshot`/`releaseWindows`) — the single
most valuable borrowed piece, because rotation-vs-live-query is precisely what it
solves.

**New**: `segIndex` (de)serialization + mmap; the seal trigger; the **segment
catalog + zone maps**; segment pruning in the planner; **newest-first iteration +
LIMIT pushdown**; the retention/rotation policy driver; and the `Archive` store type
tying these together.

## 6. Relationship to the persistent-`Collection` effort

The archive needs **one narrow thing** from that effort — the mmap backend and the
pin/reap reclamation (both now landed). The rest of the persistence plan targets a
*fits-in-RAM live* store: MVCC-durability, group-commit `msync`, and directory
rebuild-by-scan recovery. Those are **orthogonal to — and in places at odds with —**
the archive (which has no MVCC, no directory, and cannot afford O(records)
recovery). So the archive is a parallel track that consumes a shared primitive, not
a downstream of the full persistence design.

## 7. Milestones (gated)

- **H1 — sealed segments + persisted sidecar index.** Seal-on-roll-over; serialize/
  `mmap` `segIndex`; active tail full-scanned. Query results identical to a full
  scan (fuzz).
- **H2 — catalog + zone maps + pruning.** Persistent catalog; per-segment zone maps;
  planner prunes segments by zone map; O(segments) restart.
- **H3 — newest-first + LIMIT.** Reverse-chronological iteration and early-termination
  `Limit(K)`; verify "last K" touches only the newest surviving segments.
- **H4 — rotation & retention.** Age/size/count policy; drop via `retire`→`reap`;
  crash-safe catalog commit; in-flight-query safety under `-race`.
- **H5 — pageable indices at scale.** Frozen/mmap roaring; resident set stays bounded
  (catalog+zone maps only); benchmark index-not-resident query latency.

## 8. Verification

- **Correctness/fuzz**: archive query == brute-force `vm` eval over source ads,
  across seal boundaries, pruning, and LIMIT.
- **Recovery**: write N + seal, reopen ⇒ catalog + sidecars restore all segments;
  truncated tail of the active segment recovers only the committed prefix; a torn
  catalog write recovers the prior version.
- **Rotation under load**: concurrent ingest + queries + rotation under `-race`;
  a query scanning a rotating segment completes (pin holds off `reap`); no
  use-after-munmap.
- **Scale/bench**: time-bounded query latency vs archive size (should track relevant
  segments, not total); "last K" early-termination; resident memory stays ~catalog
  size as data grows; density inline+dict vs intern table on a real history sample.

## 9. Open questions & risks

- **Roll-over trigger**: size vs time-span vs both. Time-span improves zone-map
  selectivity but yields uneven segment sizes; size is simpler. Likely size-primary
  with a max-age cap.
- **Zone-map attribute set**: which attrs earn a min/max. Start with completion time
  + `ClusterId`; the `SuggestIndexes` demand tracker could later inform this.
- **Timestamp skew**: out-of-order completion times widen zone ranges and reduce
  pruning; quantify on real data. Segment overlap is correct but potentially less
  selective.
- **Sidecar index format stability**: roaring's portable format is stable, but the
  surrounding sidecar layout needs a version header for forward compatibility.
- **Segment count blow-up**: too-small segments inflate the catalog and per-query
  index-open overhead; too-large hurt pruning granularity and rotation. Needs a
  sizing pass on real history volumes.
- **Multi-writer / concurrent history files**: if more than one producer appends,
  either serialize through one writer or partition by writer — decide before H4.
