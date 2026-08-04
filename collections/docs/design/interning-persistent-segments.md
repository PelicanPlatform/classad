# Design: interning persistent segments (per-segment dictionaries)

## Motivation

Persistent collections today store records **inline-name encoded** (`flagInlineNames`):
every attribute key is a literal name string in every record. This is self-describing
(segments recover with no external table) but costs on three axes. Measured on the 1500-ad
OSPool slot corpus (`collections/interning_measure_test.go`):

- **Decompressed size: −56%** (inline 32.4MB → interned 14.35MB). Halves the RAM footprint
  of the active (uncompressed) segment, the block cache, and mmap-resident pages.
- **Decode: ~26% faster, −20% bytes allocated, −29% allocs** (26 994ns vs 36 410ns/ad) —
  integer-id lookups instead of name compares. On the decompressed path, so zstd doesn't
  erode it.
- **Feature reach.** Every id-keyed accelerator (adschema columnar scan, future ones) must
  currently *canonicalize* inline records to interned wire first (`recordToInterned`).
  Interned-at-rest makes that a no-op and lets id-keyed **persisted** structures work.

**On-disk (zstd) size is only ~7–9% smaller**, because zstd already dedups the repeated
inline names — so compressed footprint alone does **not** justify this work; RAM + decode +
feature-reach do. Decide against the actual bottleneck (hot working-set RAM, scan/decode
throughput) rather than disk.

## The pivotal choice: per-segment dictionaries, not a global table

An earlier draft proposed one durable **global** intern table for the whole collection.
Measurement (same harness, segments compressed independently = on-disk reality) shows a
**per-segment** dictionary (each sealed segment carries its own id→name dict, Parquet/ORC
style) is the better design:

| segment size | global zstd | per-seg zstd | per-seg overhead |
|---|---|---|---|
| K=128 (12 segs) | 1.02MB | 1.06MB | +5.8% |
| K=512 (3 segs)  | 968KB  | 981KB  | +1.4% |
| K≥2048 (1 seg)  | 962KB  | 962KB  | ~0%   |

The 8MB default segment holds ~840 of these ads, so the **realistic operating point is
K≈512–2048 → per-segment costs ≤1%** over global. Decode and decompressed size are
**identical** between the two (both use integer ids). In exchange for that ≤1%, per-segment
**eliminates the sharpest risks** of a global table:

| | global table | per-segment dict |
|---|---|---|
| self-contained segments | ✗ global load-bearing state | **✓** |
| write-ordering crux (names durable before records) | ✗ the hard part | **✓ gone — dict sealed with segment** |
| recovery | load table first | per-segment, no ordering |
| corruption blast radius | whole store | one segment |
| adschema block persistence | needs stable global ids | **self-contained** |
| cross-segment id space | one | per-segment (minor indirection) |

So: **per-segment dictionaries.** A segment stays self-describing (its own dict + interned
records) — the same durability property inline has today, minus the per-record name cost —
with no global durable table, no recover-first ordering, and no fsync-before-msync crux.

## 1. Coexistence is already free (per-ad flag)

The wire header carries `flagInlineNames` **per ad** (`wire/format.go`), and `wire.Decode(b,
t)` already dispatches on it. So the transition is incremental and safe:

- New segments can be written interned while **every existing inline segment stays
  readable** — no migration, no rewrite.
- The read path stops branching on the collection-level `c.inline`; the **per-ad flag** (or
  a per-segment encoding tag) decides. For interned ads the decoder needs *that segment's*
  dictionary; for inline ads it needs nothing.

This does mean interned decode is **dictionary-scoped to the segment**, not the collection —
see §4.

## 2. Per-segment dictionary — format & build (phase 1)

- **Where:** in the sealed segment's on-disk container, beside the records. The dict section
  is fully **mmap-backed and heap-free per segment** (§4 details the structures):
  `[u32 count][id→offset table: count×u32][name blob: length-prefixed names in id order][name→id MPH (appendMPH)]`.
  Id `k` = the k-th name. Self-contained; a reader mmaps it and probes zero-copy — it builds
  **no** Go map or slice per segment, so loading thousands of sealed dicts does not claw back
  interning's RAM win. Dict is small (~900 names, ~26KB raw / ~6KB zstd for this corpus; MPH
  ~1KB; offsets ~3.6KB).
- **When built:** at **seal**. The active (unsealed) segment stays inline (point writes,
  recent data — never pays transcode), exactly mirroring adschema's active=row / sealed=PAX
  split. Seal already walks every record (buildSegIndex, adschema block build); interning
  folds into that pass: decode each inline record, intern its names into a fresh per-segment
  table, re-encode interned, and emit `[dict][interned records]`.
- **Durability:** the dict is written **inside** the segment via the segment's existing
  atomic write / msync — no separate file, no cross-file ordering. A torn segment is
  detected and rebuilt exactly as today (the segment is derived-at-seal from the active
  log). Recovery reads the dict then the records; a corrupt dict fails only that segment.
- **No global state.** `c.intern` (the in-memory table) is still used for the active segment
  and query-side name↔id, but it is **not** persisted and **not** load-bearing for any
  sealed segment.

## 3. Interned write at seal + read-both (phase 1 core)

- **Seal transcode = a segment rewrite.** The active segment is written inline as records
  arrive; at seal it is rewritten into `[segment header][dict][records with interned bodies]`,
  mirroring the compaction rewrite (build a new arena, copy each record preserving its
  seq/superseded/key header while swapping the body inline→interned, write + msync + swap in).
  The active segment and all existing sealed segments are untouched; only the sealing pass
  changes. Folds into the existing seal walk (buildSegIndex reads every record already).
- **Encoding tag = a byte in the segment header** (DECIDED). A segment is uniform in encoding
  (one seal produced it); a version/flags byte at the start of the segment makes it fully
  **self-describing** — recovery reads the encoding (and, if interned, the dict) from the
  segment alone, no directory/external state. Old segments (no/inline tag) read as inline; new
  sealed segments read as interned-with-dict.
- **Rollout = on by default for persistent collections** (DECIDED). Every new seal on a
  persistent collection is interned; existing inline segments stay inline and are read via the
  per-segment dispatch (read-both), converting naturally as segments seal/compact. No opt-in
  flag — so phase 1 correctness must be airtight before it lands (heavy mixed-segment tests).
- **Reads:** route decode through the per-segment encoding tag: inline → `DecodeInline`;
  interned → `DecodeResolve` against the segment's dict (`segDictName`). `decodeWire`/
  `wireLookup`/`ForEachNamed` stop consulting the collection-level `c.inline` and consult the
  **segment's** tag + dict (threaded to each call site).

## 4. Dictionary lookup structures & query-side name↔id (phase 1 care)

Per-segment interning makes **ids segment-local**, so a query's attribute name resolves to a
*different* id in each segment. The resolution structures must not reintroduce the per-segment
Go heap that interning is meant to remove — so both directions live in the mmap.

**Active / global in-memory table (`c.intern`): Go map, unchanged.** It is *mutable* (Intern
grows it during ingest), so a static perfect hash cannot back it; it is the warm working set
anyway. Its hot direction (`id→name` at encode) is already a slice index.

**Sealed per-segment dict: fully mmap-backed, zero per-segment heap** (reuses the existing
sealed-index machinery — `mph.go` + the key-index's blob+verify+fallback pattern):

- **`name→id` → mmap MPH** (`buildMPH`/`appendMPH`/`mphLookupBytes`). Lookup is *rare* (once
  per probed-attr per segment at planning), so the driver is **not** speed but RAM: a Go
  `map[string]uint32` per dict is ~50–70 B/entry of GC-scanned resident heap (~60 KB/segment
  → hundreds of MB across a large store); the MPH is ~1 KB of pageable mmap. MPH is
  non-authoritative (member→its slot; non-member may false-hit), so verify the name at the
  resolved slot against the name blob and fall back (linear scan of the ~900-name dict, or a
  small sorted index) on a miss — exactly the key-index contract.
- **`id→name` → id-ordered name blob + `id→offset` table**, both serialized in the segment.
  `id→name` = `nameAt(offsets[id])`, two zero-copy mmap reads. Static, GC-free, pageable. The
  naive `[]string`/`map[uint32]string` at load is the heap-resident anti-pattern we avoid.
  `id→name` is usually **off the hot path** — scan/count/filter work in ids/positions and
  never resolve names; it's paid only when materializing a full ad back to named form.

**Query-side `name→local-id` via the per-segment MPH** (this *replaces* the earlier
local→global-id array idea): a probe carries the attribute name; each segment resolves
`name→local-id` directly from its mmap MPH (zero heap), then scans by that local id. No
per-segment translation array, no global-id coupling for the sealed tier.

- **Accelerators (adschema) that want a stable id space:** the adschema *schema* is
  collection-level and can stay keyed by **name** (each segment's block maps schema-name →
  that segment's local id via the same MPH at block-build time). The block's hot columns are
  position-based (schema order), so most of the scan never touches the dict at all.

## 5. adschema payoff (folds P3(2) in)

With per-segment dicts (§4):
- A sealed segment's columnar block references segment-local ids, persisted **inside the
  same segment container** as the dict — self-contained, no global table needed. The block's
  fields bind to the collection schema by **name** (resolved to that segment's local id via
  the dict MPH at block-build), so a reloaded block matches the collection schema with no
  global-id table.
- `marshalColSegment`/`unmarshalColSegment` (already built) persist the block; the schema is
  re-derivable/name-keyed. Persist the schema-scan config (enabled + schema + hot) at the
  collection level (mirror `TimeTravel` in `db/indexpersist.go`); reload sets
  `schemaScanState`, then attach each segment's reloaded block. **No rebuild on reopen.**
- `recordToInterned` becomes identity for interned segments (kept only for legacy inline).

## 6. Encryption-at-rest interplay (sequence explicitly)

Only inline has a sealing encoder today (`EncodeInlineWithHotEnc`); `EncodeWithHot*` is
plaintext. So **interned seal is plaintext-only first**; an encrypted collection stays on
inline+enc until an interned+enc encode/decode pair (`EncodeWithHotEnc`) lands. Gate:
don't seal-to-interned an encrypted collection until then. Existing encrypted inline data is
unaffected throughout.

## 7. Phasing & release

1. **Phase 1 — per-segment interned seal.** Dict format inside the segment container;
   interned seal transcode (opt-in per collection); per-segment encoding tag; read-both
   (inline old, interned new); query-side `name→local-id` via the per-segment MPH (§4). Tests: write a
   mix of inline + interned segments in one collection, identical query results vs all-inline
   control; measured size/decode delta; reopen; corrupt-dict → that segment rebuilt, others
   fine. **Also unblocks adschema P3(2)** (self-contained blocks).
2. **Phase 1b — adschema block persistence** rides on phase 1: persist colSegment in the
   segment container + schema-scan config; reload without rebuild.
3. **Phase 2 — encryption** (`EncodeWithHotEnc`/decode), then allow encrypted collections to
   seal interned.
4. **Phase 3 (later) — migration lever + retire inline write path.** A recompaction re-emits
   sealed segments interned with zero operator action (compaction already re-encodes forward);
   optionally expose `.rewrite`-to-interned. Eventually collapse `inline.go` dual-paths once
   every seal is interned.

Each phase: branch → PR → tag; db/dbrpc bump as usual.

## 8. Risks & mitigations

- **Per-segment dict adds a small on-disk cost (~1% at realistic segment size, up to ~6% at
  tiny K).** Mitigation: dicts only exist on sealed segments (K large); don't seal-interned
  tiny segments if it ever matters.
- **Query-side per-segment id translation is new surface.** Mitigation: option (b) confines
  it to a per-segment array built once at dict-load; accelerators keep global ids.
- **Encoding tag must be honored everywhere a body is decoded.** Audit every `c.inline`
  read: decode-time ones move to the per-segment tag; write-mode ones stay. This is the bulk
  of phase-1 care (same audit the global design needed, minus the durability crux).
- **On-disk win is modest (~7–9% zstd).** Not a disk-savings project; justify on RAM/decode.
  Documented up front so the decision is honest.

## 9. Verification

- **Phase 1:** a collection with both inline (pre-flip) and interned (post-flip) sealed
  segments returns identical results to an all-inline control across queries/aggregates;
  reopen round-trips (segments self-describe — no global table to reload); corrupt one
  segment's dict → only that segment rebuilds; measured size + decode deltas reproduce the
  harness numbers on a live store.
- **Phase 1b:** adschema persistent tests reload blocks (no rebuild) and match the row
  engine; reopen preserves the accelerator.
- **Cross-module:** db + dbrpc suites green against the bumped collections.

## Open questions

1. **Query id-translation: RESOLVED** — per-segment MPH (`mph.go`) resolves `name→local-id`
   from the mmap, zero per-segment heap; `id→name` via an id-ordered name blob + offset table
   (also mmap). Active/global table stays a Go map. (Supersedes the earlier local→global-id
   array; adschema schema binds by name.)
2. **Encoding tag location: RESOLVED** — a byte in the segment header; the segment is
   self-describing (recovery reads encoding + dict from the segment alone).
3. **Rollout: RESOLVED** — on by default for persistent collections (no opt-in flag); existing
   inline segments read via per-segment dispatch and convert as they seal/compact. Correctness
   must be airtight before landing.
4. **Encrypted-interned priority:** phase 2 soon, or encrypted collections stay inline
   indefinitely (acceptable; just no RAM/decode win there)? (Encrypted collections keep sealing
   inline until phase 2 regardless, since only inline has a sealing encoder today.)
