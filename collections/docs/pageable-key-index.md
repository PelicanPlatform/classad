# Design: pageable primary key index (key-count past RAM)

Status: proposal / RFC
Scope: `collections` mmap-segment store (and, transitively, `db`, the collector, htcondordb)

## Problem

The store already keeps ad **values** and **attribute indexes** out of RAM: values
live in demand-paged mmap segments, and a sealed segment's query index converts to a
pageable mmap sidecar (`msidx`). So *value* footprint can exceed RAM today.

What cannot exceed RAM is **key count**. Every live key has an entry in an in-RAM
hash directory, `shard.dir map[uint64]loc` (`shard.go`), consulted by every
`Get`/`Put`/`Delete`. At ~40–50 B/key with Go-map overhead, 100M keys ≈ 4–5 GB of
directory that must be resident. For a store whose *working set* is small but whose
*key count* is large (htcondordb history; a very large pool), the directory is the
binding limit.

Goal: make the primary key index **pageable**, so steady-state RAM ≈ working set
(the active segment's keys + hot pages + per-segment filters), not O(total keys).

Non-goals: changing the query/scan engine, the MVCC/seq model, or the value
encoding. This is about *where the key→location directory lives*.

## What makes this tractable: currency is already a local property

A naive "page out the directory" reads like an LSM-tree conversion (sorted runs,
newest-wins merge, tombstones). It is simpler here because of an existing invariant:

> A record is **current** iff its `supersededBySeq` field == `seqMax`. When a key is
> updated or deleted, the store marks the *old* record superseded **in place**,
> wherever it lives — including in a sealed segment (segments stay RW-mapped for
> exactly this).

So we never need cross-segment version ordering to decide currency: at most one
record per key is non-superseded, anywhere in the store. Finding a key's current
value is "find the record for this key whose `supersededBySeq == seqMax`." That
turns the problem from "merge sorted runs" into "locate the key's live record,"
which a per-segment key index answers directly.

## Design

### 1. Per-sealed-segment key sidecar
When a segment seals (a new active segment is allocated, or at compaction/close), an
immutable **key index** is written for it: `key-hash → in-segment offset` for every
record, a sorted `(hash, off)` array (binary search, ~12 B/key, pageable). Same
lifecycle as the attribute-index sidecar (`msidx`): built once at seal, mmapped on
reopen, torn down with the segment at reap. A sealed segment is immutable, so its key
index never changes (supersession flips a bit in the segment *data*, not the index).

**One sidecar file per segment.** To keep the per-segment file/inode/fd footprint at
two (`.dat` + `.idx`) — the same budget the fd work targets — the key index does not
get its own file; it is embedded in the existing `.idx` **container**: the attribute
blob first (offset 0, so its roaring bitmaps stay aligned and the existing ARCX
writer/parser — shared with the archive table type — are reused unchanged), the key
blob second, and a 12-byte trailer with the two lengths. The container is now written
for **every** persistent sealed segment (previously the `.idx` existed only when
attribute indexes were configured); the attribute section is simply empty for a
key-only collection.

### 2. Per-segment Bloom filter
A small Bloom filter per sealed segment, keyed on key-hash, bounds probe fan-out: a
lookup only touches sidecars whose Bloom says "maybe." At ~1% FPR that is ~1.2 B/key
(~120 MB for 100M keys) — the one structure we may keep resident, and it can itself be
mmapped if needed. This is the RAM floor of the design, ~40× smaller than today's
directory.

### 3. RAM directory holds only the active segment
`shard.dir` keeps entries **only for the active (unsealed) segment's keys**. When a
segment seals, its key sidecar + Bloom are written and its keys are **evicted** from
`shard.dir`. RAM for the directory is thus bounded by the active segment size, not
total keys.

### Read / write / delete paths

- **Get(key)**: check `shard.dir` (active) → if a current record, return it. Else
  probe sealed segments newest→oldest, Bloom-gated; for each sidecar hit, read the
  record and check `supersededBySeq == seqMax`. The single live record wins; a key
  whose records are all superseded (deleted, or only stale versions) is absent.
- **Put(key)**: append the new version to the active segment; set `shard.dir[hash]`.
  Mark the previous current record superseded — it is either in `shard.dir` (O(1)) or,
  if the key's live version was in a sealed segment, located via the same Bloom-gated
  sidecar probe and its `supersededBySeq` written (segments are RW-mapped; the
  supersession write is msync'd, as tombstones already are).
- **Delete(key)**: locate the current record (active or sealed-probe) and mark it
  superseded; if it was in the active segment, drop it from `shard.dir`.
- **Scan/Query**: unchanged. Scans already walk segment records via `forEachVisible`
  (superseded-aware) and never consult `shard.dir`.

### Compaction
Unchanged in spirit (rewrite live records into fresh segments, drop superseded/dead,
reap sources), plus: emit the key sidecar + Bloom for each destination segment, and
keep evicted keys out of `shard.dir`. Compaction is also what bounds probe fan-out:
it collapses a key's stale copies across segments down to one.

### Reopen (subsumes the clean-shutdown snapshot)
The per-segment key sidecars **are** a persisted per-segment directory. On reopen the
full directory is assembled by mapping the sidecars (pageable) + scanning only the
active segment — O(active), no full record scan. This generalizes the increment-2
`dir.snap` (a whole-shard directory snapshot): the snapshot was the stepping stone;
per-segment sidecars are the same idea made pageable and compaction-friendly. A
missing/invalid sidecar for a segment falls back to scanning *that segment* (not the
whole store) to rebuild it, mirroring how `msidx` already degrades.

## RAM budget

| Structure | Today | Proposed |
| --- | --- | --- |
| Primary directory | O(all keys), resident (~4–5 GB @ 100M) | O(active-segment keys), resident |
| Per-segment key index | — | pageable mmap sidecar (~12 B/key) |
| Per-segment Bloom | — | ~1.2 B/key (~120 MB @ 100M), resident or mmap |
| Values, attr indexes | already pageable | unchanged |

Net: resident RAM goes from O(all keys) to O(active keys) + O(#keys × Bloom bits),
the latter ~40× smaller and itself pageable.

## Costs and mitigations

- **Write read-amplification on cold keys.** Updating/deleting a key whose live
  version is in a sealed segment now costs a Bloom-gated sidecar probe to find the
  old record to supersede (was O(1) via the RAM dir). Mitigation: hot keys are
  rewritten into the active segment on update, so they stay in the RAM dir; the probe
  cost falls only on *cold-key* mutations, which are rare in the target workloads
  (history is append-mostly; the collector's working set fits in RAM and is
  unaffected).
- **Read fan-out.** A `Get` miss in the active dir probes sealed segments; Blooms cap
  this to ~(#segments the key was ever in), which compaction keeps small.
- **Bloom is still O(#keys) bits.** Acceptable (~40× under the directory) and
  pageable; a stronger scheme (partitioned/hierarchical filters) is possible later.

## Crash consistency

Sidecars are derived data: a torn/absent sidecar is rebuilt by scanning its (single)
segment, exactly as `msidx` degrades today. Supersession writes into sealed segments
are already msync'd (the tombstone path). So no new durability primitive is needed;
recovery is per-segment, not whole-store.

## Phasing

1. **Key sidecar format + build-at-seal + mmap-on-reopen** — DONE. The key index is
   embedded in the always-present `.idx` container; built at seal (the Reindex pass)
   and at Close, mapped on reopen, torn down with the segment. No eviction yet (the
   directory is still full and authoritative); the sidecar is populated and validated
   beside it (every record findable by hash, every live key locatable, deletes
   honored). Also fixed a latent loader bug: `.idx`/`.kidx` sidecar names matched the
   segment-file `Sscanf` (trailing input ignored), so sidecars were loaded as phantom
   segments — now the loader requires an exact `.dat` suffix.
2. **Bloom filters + sealed-segment probe** — DONE. A resident per-segment key Bloom
   (built from the key index at seal/load) gates a `shard.lookupSealed` probe that
   locates a key's current record in the sealed segments (Bloom → key index →
   `supersededBySeq == seqMax`). The directory is still authoritative; the probe is
   validated against it by an oracle test (for every key: a sealed-resident record is
   reproduced by the probe at the same location, an active-resident one is correctly
   missed, a deleted one is not found).
3. **Evict sealed keys from `shard.dir`** — the RAM win. DONE (first cut). Every
   by-key path is now dir-then-probe -- `get`, `put`/`del` (probe + supersede a
   sealed old version), and the MVCC paths `getAt` (snapshot probe) and
   `conflictSince` (the OCC guard also scans the sealed records, so a conflict on an
   evicted key is still detected). Eviction happens at **reopen**, where every sealed
   segment is indexed so no version escapes the probe (a version lives one per
   segment; dir-chain + sealed-probe cover them all). Result: the resident directory
   is O(active segment), not O(all keys) -- 3% of the keys in the test. Tests: the
   probe/dir oracle, an **OCC torture test** (concurrent increments on *evicted*
   counters converge with no lost updates), evicted write-write conflict, evicted
   snapshot isolation; full race suite green. STILL TO DO within this phase:
   operation-time eviction (a background seal+evict pass, so a long-running process
   realizes the win before reopen) and **compaction** eviction (compaction currently
   rebuilds the full directory -- correct, but re-populates it until the next reopen).
4. **Retire `dir.snap`** (increment 2) in favor of per-segment sidecars.

Each phase is independently shippable and testable; the RAM behavior only changes at
phase 3.

## Risks / open questions

- **Probe cost under adversarial update patterns** (many cold-key updates). Needs a
  benchmark; may motivate a small "recently-touched sealed keys" RAM cache.
- **Bloom sizing / false-positive budget** vs probe cost — tune with real key
  cardinalities.
- **Interaction with chaining** (`childParentHash`) and **ordered indexes**
  (`ordered.byKey`, itself O(#keys) RAM) — ordered indexes would need the same
  pageable treatment to fully realize the RAM win for negotiator-style workloads;
  out of scope here, noted as a sibling follow-up.
- **Sidecar format stability** / versioning for on-disk compatibility.

## Relationship to shipped work

- Increment 1 (release fds after mmap) — orthogonal; already merged-pending (#38).
- Increment 2 (`dir.snap` clean-shutdown snapshot) — a stepping stone this design
  generalizes and eventually retires (phase 4).
- This proposal (increment 3) is the one that lifts the **key-count** ceiling.
