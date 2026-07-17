# A ClassAd Database Engine: Design and Decisions

*A design book for the HTCondor community.*

This document describes the design of `collections` — an embeddable, high-performance
storage-and-query engine for ClassAds, written in Go
(`github.com/PelicanPlatform/classad/collections`, Go 1.25). It is one substrate that
serves several of HTCondor's ClassAd-shaped problems: the live in-memory collection a
daemon queries, the durable on-disk variant that survives restart, the append-only
history/archive that outgrows RAM, and the negotiator/schedd operations —
matchmaking, ranking, and the priority-ordered job queue — layered on top.

It is meant to be read end-to-end. It starts from the ideas and gets progressively
more detailed; early chapters give the shape, later ones the mechanism and the
rationale for each decision. Source files are named where they help navigation
(e.g. `shard.go`), but the text is self-contained. External references are linked
inline.

---

## Table of contents

**Part I — Foundations**
1. [Why a ClassAd database engine](#1-why-a-classad-database-engine)
2. [The ClassAd data model](#2-the-classad-data-model)

**Part II — The storage substrate**
3. [The wire format](#3-the-wire-format)
4. [Compression: ZSTD with trained dictionaries](#4-compression-zstd-with-trained-dictionaries)
5. [The sharded MVCC store](#5-the-sharded-mvcc-store)
6. [Durability and group commit](#6-durability-and-group-commit)
7. [Compaction and reclamation](#7-compaction-and-reclamation)
8. [Persistence and recovery](#8-persistence-and-recovery)

**Part III — Querying**
9. [The compiled query engine](#9-the-compiled-query-engine)
10. [Indexing](#10-indexing)
11. [Parallel query](#11-parallel-query)

**Part IV — Beyond key/value**
12. [Change data capture: Watch](#12-change-data-capture-watch)
13. [Parent/child chaining (cluster/proc)](#13-parentchild-chaining-clusterproc)
14. [The history archive](#14-the-history-archive)

**Part V — Negotiator and schedd operations**
15. [Matchmaking: match and rank](#15-matchmaking-match-and-rank)
16. [The maintained ordered index](#16-the-maintained-ordered-index)
17. [Consistency, MVCC, and concurrency](#17-consistency-mvcc-and-concurrency)

**Part VI — Reflection**
18. [Design principles and decisions](#18-design-principles-and-decisions)
19. [Correctness and testing](#19-correctness-and-testing)

**Appendices**
- [A. Performance summary](#appendix-a-performance-summary)
- [B. API sketch](#appendix-b-api-sketch)

---

# Part I — Foundations

## 1. Why a ClassAd database engine

HTCondor stores and queries ClassAds everywhere: the schedd's job queue, the
collector's daemon ads, the negotiator's view of slots, the history file of completed
jobs. Each of these is, at heart, *a set of ClassAds you insert, update, delete, and
query by expression* — and each has historically grown its own storage and its own
query loop. The recurring costs are the same: decoding ads to evaluate a constraint,
re-sorting a queue every negotiation cycle, holding more in memory than a process
should, and keeping an index consistent with the data.

This engine takes the position that these are **one problem with several policy
layers**. A single substrate provides:

- a **dense binary wire format** for ClassAds that can be filtered without being fully
  decoded;
- a **compiled query engine** that evaluates a constraint directly against those
  encoded bytes;
- a **sharded, MVCC, append-only store** with snapshot-isolated, exactly-once scans;
- **secondary indexes** (value, categorical, and ordered) built over the same bytes;
- **durability** (memory-mapped arenas, group-committed writes, crash recovery);
- and, on top, the HTCondor-specific verbs: **matchmaking**, **ranking**, a
  **maintained priority-ordered index** for the schedd, and **change subscriptions**.

The same engine is instantiated three ways, differing only in policy, not in
mechanism:

| Instantiation | Regime | What it drops / adds |
|---|---|---|
| **In-memory `Collection`** | working set fits in RAM | the default; GC reclaims retired segments |
| **Persistent `Collection`** | fits in RAM, survives restart | mmap arenas, `msync` durability, recovery |
| **`Archive`** | larger than RAM, append-only | drops MVCC, compaction, key directory; adds a catalog, zone maps, and pageable indices |

### Design tenets

Five commitments recur through every chapter, and it is worth stating them up front
because they explain most of the decisions:

1. **Consistency over cleverness.** Where a choice trades a small performance gain for
   a weaker guarantee, we keep the guarantee. Scans are exactly-once; recovery is
   deterministic; indexes are re-verified.
2. **Reuse the substrate.** Every new capability — indexing, matchmaking, the archive,
   the ordered index — is built from the wire format, the `vm` engine, the segment
   backend, and the reclamation machinery. New code is policy, not a parallel stack.
3. **Soundness by construction.** Fast paths (index pre-filters, wire-native
   evaluation, zone-map pruning) are allowed to *widen* a candidate set but never to
   drop a true result — an authoritative check always re-verifies. A wrong fast-path
   decision costs selectivity, never correctness.
4. **Pay for what you touch.** Filtering reads only the attributes a query mentions;
   a rejected ad is never fully decoded; an unbuilt index just full-scans; an
   unwatched store pays nothing for Watch.
5. **Prove it with differential tests.** The engine is checked against a brute-force
   oracle and, for the evaluator, against HTCondor's own C++ libclassad, continuously
   and under the race detector.

## 2. The ClassAd data model

A ClassAd is an unordered set of attributes, each mapping a case-insensitive name to
an *expression*. Expressions are evaluated lazily and in a specific context; the same
attribute can be a literal (`Memory = 8192`), a computation
(`RequestMemory = TARGET.Memory / 2`), or a reference to another ad's attributes.
Evaluation is **three-valued** (true/false/undefined, plus an error value), with
type coercions — a subtlety the engine must preserve exactly, because HTCondor's
policy expressions depend on it.

Two features of the model shape everything downstream:

- **Scoped references.** An unscoped name resolves in the current ad; `MY.x` is the
  current ad explicitly; `TARGET.x` is the *other* ad in a match; `PARENT.x` walks an
  enclosing scope. Matchmaking and chained (cluster/proc) ads both lean on this.
- **Bilateral matchmaking.** Two ads *match* when each one's `Requirements` expression,
  evaluated with the other as `TARGET`, is true. A job and a slot match only if the
  job accepts the slot **and** the slot accepts the job. `Rank` expressions then order
  the acceptable candidates. This symmetry is the negotiator's core operation, and
  Chapter 15 builds it on the engine.

The operations the engine must therefore support are: point `Get`/`Put`/`Delete` by
key; `Update` in batches; `Scan` and `Query(constraint)`; `Match(job)` and ranked
match; a maintained ordered iteration (the schedd queue); and `Watch` subscriptions.
The rest of the book is how each is made fast and consistent.

The engine builds on a separate, standalone Go implementation of the ClassAd language
(the `classad` and `ast` packages: parser, evaluator, three-valued logic, the standard
function library). That implementation is itself validated by differential fuzzing
against HTCondor's C++ libclassad (Chapter 19). This engine reuses its evaluator for
all authoritative results, so semantics are identical by construction.

---

# Part II — The storage substrate

## 3. The wire format

A query should not have to reconstruct a whole ClassAd object to test a predicate.
That principle drives the on-disk/on-wire representation (`collections/wire`).

An ad is encoded as a sequence of `(name, value-node)` entries. The value node is the
crux: **every attribute value is a self-delimiting expression node**. A literal
integer is a one-byte tag plus a varint; a string is a tag plus length plus bytes; a
compound expression (arithmetic, a function call, a list) is its AST serialized
node-by-node. Because each node is self-delimiting, a reader can **skip** any value it
does not care about without parsing it (`skipNode`), and a zero-copy `Ad` accessor can
`Lookup` or `ForEach` over attributes without allocating. This is what lets the query
engine (Chapter 9) read `TARGET.Memory` straight out of the bytes and ignore the other
forty attributes of a slot ad.

### Byte layout

An encoded ad is a 3-byte header, an optional hot-attribute directory, then the
attribute entries:

```
[magic:1=0xCA] [version:1] [flags:1]
(if flags has flagStandalone:  an embedded intern table — omitted for stored ads)
[hotCount: uvarint]   hotCount × ( uvarint(key), uvarint(offset) )   // the hot directory
[attrCount: uvarint]  attrCount × ( key, value-node )                // the entries region
```

The `key` is a `uvarint(internID)` for an interned ad, or `uvarint(len)+bytes` for an
inline-names ad. Each `value-node` is a tag byte plus a tag-specific payload — the same
recursive encoding for a literal or a whole expression. The hot directory's `offset` is
**relative to the start of the entries region** (not the ad), which avoids a chicken-and-egg
problem: an absolute offset would depend on the header's own size, which depends on how many
bytes those offsets take to encode.

The value-node tags are a small closed set. The scalars a query touches most are one byte
plus a minimal payload:

| Tag | Meaning | Payload |
|---|---|---|
| `0x00`–`0x03` | undefined / error / false / true | *(none — the tag is the value)* |
| `0x04` | integer | zigzag varint |
| `0x05` | real | 8 bytes IEEE-754 LE |
| `0x06` | string | `uvarint(len)` + bytes |
| `0x07` | attribute ref | scope byte + `uvarint(nameID)` |
| `0x08` | binary op | op byte + node(left) + node(right) |
| … | list, func, cond, select, … | children encoded inline, pre-order |

Worked example — the ad `[ Arch = "INTEL"; Cpus = 4; Requirements = TARGET.Memory >= 2048 ]`,
interned (illustrative ids `Arch=10`, `Cpus=11`, `Requirements=12`, `Memory=13`), with
`Cpus` declared hot:

```
CA                        magic
01                        version 1
00                        flags = 0  (interned, shared table, not inline)
01                        hotCount = 1
0B 09                     hot: key=11 (Cpus), offset=9 into the entries region
03                        attrCount = 3
── entries region (offset 0 starts here) ───────────────────────────────
0A  06 05 49 4E 54 45 4C  Arch: id=10, node = string(0x06) len=5 "INTEL"
0B  04 08                 Cpus: id=11, node = int(0x04) zigzag(4)=8        ← offset 9 = its node
0C  08 08                 Requirements: id=12, node = binop(0x08) op=">="(8)
        07 02 0D            left  = attrRef(0x07) scope=TARGET(2) name=13 (Memory)
        04 80 20            right = int(0x04) zigzag(2048) = uvarint 4096
```

`Cpus`'s hot offset is 9 because its value node begins 9 bytes into the entries region
(the `Arch` entry is 8 bytes: 1 id + 7 node). A `Lookup(Cpus)` reads the node directly at
`entriesStart + 9`; a `Lookup(Arch)` — not hot — skips forward node-by-node with `skipNode`.
An inline-names ad is identical except each `key` is `uvarint(len)+bytes` (e.g. `04 41 72 63
68` for `"Arch"`), the hot directory keys are a 32-bit case-folded name hash instead of an id,
and refs/funcs/selects use the inline-name node variants that carry the name inline.

Attribute names are handled in one of two modes, a deliberate split:

- **Interned** (in-memory `Collection`). A shared `InternTable` maps each name to a
  small integer id, case-folded but case-preserving. Records store the id, so a pool of
  ads that all have `Owner`, `JobStatus`, `RequestMemory`… pays for those strings once.
- **Inline** (persistent `Collection` and `Archive`). Each record stores its names
  verbatim, so a segment file is **self-contained** — recoverable with no shared table
  to persist or corrupt. The density lost to repeating names is recovered by the ZSTD
  dictionary (Chapter 4). Inline records run about 1.5× the size of interned ones
  before compression; with a trained dictionary the residual gap narrows to ~1.35×.

The choice is a direct consequence of tenet 1: interning is denser, but persisting and
recovering a shared, mutable string table across crashes is a correctness liability,
so the durable variants trade a little density for self-contained records.

Two more wire features matter:

- **The hot header.** `Options.HotAttrs` names the "popular" attributes a workload
  filters on (e.g. `Cpus`, `Memory`, `Arch`, `OpSys`, `State`). The encoder front-loads
  those, storing `(name-or-id, entries-relative offset)` pairs, so `Lookup` of a hot
  attribute is O(1) instead of a scan of the entry list. Offsets are *entries-relative*
  to avoid the circularity of encoding an offset that depends on the header's own size.
  On the partial-decode path this is roughly a 2× win. Popularity can be refreshed
  automatically from observed access frequency (`RefreshHotSet`), computed by walking
  wire bytes with no decode.
- **Streaming construction and direct ingest.** A `StreamEncoder` builds a wire ad one
  attribute at a time — scalar setters write literals with no AST at all — so ads
  arriving on a socket in HTCondor's old ClassAd text form can be turned into wire
  bytes without materializing an intermediate object. `UpdateOld` parses that socket
  form with a conservative scalar fast path (recognizing non-negative ints, reals,
  escape-free ASCII strings, and the keyword literals) and only falls back to the full
  parser for computed values or awkward inputs. End to end this is about 3× faster with
  3× fewer allocations than parse-then-`Put`, and it is differentially tested to produce
  identical results.

### Layout note: interleaved entries vs. a full directory

The current body **interleaves** `key` and `value` — `(id, node)(id, node)…` — and the hot
directory is a *sparse* index over it: it stores an offset only for the attributes a workload
actually touches. An alternative worth considering is a **fully split** layout — all keys and
their offsets first (a complete directory), then all value nodes — essentially promoting every
attribute to "hot." The trade-offs are real and point in different directions:

- **Random access to a *cold* attribute.** A full directory wins. Today a non-hot `Lookup` is
  an O(n) `skipNode` walk (recursively parsing each intervening value); a full directory makes
  it an O(1) offset read (or O(log n) binary search, since entries are already stored in a
  canonical sorted order). If access were unpredictable — arbitrary attribute projection,
  interactive exploration — this uniformity would matter.
- **Name-only scans.** A split layout wins clearly: tallying attribute frequency (what
  `RefreshHotSet` does), or reading a schema, can walk the key list contiguously without
  touching — or `skipNode`-ing past — a single value.
- **Size.** The interleaved + sparse-hot layout wins. A full directory pays an offset for
  *every* attribute in *every* record; across a large pool with a long tail of rarely-queried
  attributes, that is real space (and offsets compress less well than the repetitive names and
  values the dictionary loves). The hot directory pays offsets only for the ~handful of
  attributes that are actually hot.
- **Full materialization (the output path).** Interleaving wins on locality: decoding a whole
  ad is one sequential pass with each name adjacent to its value, rather than two separate
  regions.

The design bet is that **HTCondor's access pattern is predictable**: queries filter on a small,
stable set of attributes (`Owner`, `JobStatus`, `RequestMemory`, `Arch`, …), and the output path
materializes the whole ad. So the sparse, *adaptive* hot directory — which `RefreshHotSet` sizes
automatically to the attributes a workload observes — gives directory-like O(1) access to exactly
those, without paying an offset for the long tail, and keeps full-ad decode cache-friendly. A
full directory would be the better choice for a more ad-hoc, projection-heavy, or schema-inspecting
workload; it is a clean future variant (the hot header already proves the mechanism), and the two
could even coexist by letting the hot set grow to cover all attributes for such collections. For
the negotiator/schedd/collector patterns this engine targets, the interleaved-with-sparse-hot
layout is the deliberate choice.

Uncompressed, the wire form is roughly 64% of the ClassAd text size; the real win comes
from compression next.

## 4. Compression: ZSTD with trained dictionaries

Job and slot ads are extraordinarily repetitive: the same attribute names, the same
`Requirements` boilerplate, the same enumerated string values, ad after ad. General
compression captures some of this, but the redundancy is *across* ads and each ad is
small, so a per-ad ZSTD stream cannot see it. The answer is a **trained dictionary**.

The engine trains a ZSTD dictionary on the **wire bytes** of a sample of the collection
(not the text form — you compress the wire form, so that is what the dictionary must
model), and compresses each record against it. On a real OSPool sample the effect
compounds:

| Encoding | Bytes/ad |
|---|---:|
| Identity (uncompressed wire) | 8681 |
| ZSTD, no dictionary | 4117 |
| **ZSTD + trained dictionary** | **1286 (6.5×)** |

Two engineering points earned in practice:

- The dictionary must be built correctly. The `klauspost/compress` `BuildDict` does not
  select dictionary content for you: you supply the content (`History`) and the repeat
  offsets. Training on the wrong bytes, or omitting the offsets, yields an empty or
  invalid dictionary — a subtle failure worth flagging to anyone reusing the library.
- Dictionaries are **per-segment**. A segment records which dictionary it was written
  under, so records always decode with the codec that wrote them. `RetrainDict` trains a
  fresh dictionary (typically as part of compaction, when records are being rewritten
  anyway) and points new writes at it; older segments keep theirs. Training can hit a
  degenerate klauspost divide-by-zero on some samples, so it is wrapped to return an
  error and keep the prior codec rather than crash the process.

Because a stable job-ad schema is exactly the case a trained dictionary loves, this is
also why the archive (Chapter 14) prefers a per-segment dictionary over a shared intern
table: the dictionary recovers the density of inline names without the recovery
complexity of a persisted global string table.

## 5. The sharded MVCC store

The store (`store.go`, `shard.go`, `segment.go`) is where keys, versions, and bytes
live. Its structure follows from two requirements: concurrent writers must not
serialize on one lock, and scans must see a consistent, exactly-once view while writes
proceed.

**Sharding.** The keyspace is partitioned into independently-locked *shards* (a
power-of-two count; default 16). A key's shard is chosen by hash. Each shard owns its
own directory, its own arena, and its own commit sequence, so writers to different
shards never contend.

**Arenas of append-only segments.** Within a shard, records live in *segments* —
append-only byte arenas, the engine's equivalent of SSTables. A record is
`(seq, supersededBySeq, next-in-chain, key, wire-ad-bytes)`. New versions are appended;
nothing is mutated in place except two small atomic fields. A per-shard directory,
`map[keyhash] → location`, points at the head of each key's version chain; lookups
compare the full key inline to resolve hash collisions.

**MVCC visibility.** Every record carries a commit sequence `seq`; when a newer version
(or a delete) supersedes it, its `supersededBySeq` is stamped atomically. A scan
captures the shard's current sequence `S0` at snapshot time and yields exactly the
records with `seq ≤ S0 < supersededBySeq`. This single interval rule gives:

- **Snapshot isolation** — a scan sees the store as of `S0`, ignoring later writes.
- **Exactly-once** — for any key that existed at `S0`, *precisely one* version
  satisfies the interval; no duplicates, no skips.

This is the load-bearing invariant of the whole engine. It is what makes parallel scans
trivially correct (Chapter 11: disjoint segments, no cross-segment dedup), what makes
compaction safe against in-flight scans (Chapter 7), and what recovery reconstructs
(Chapter 8). It holds under the race detector with concurrent updates, deletes, and
compaction running together.

**A delete writes no record** — it only stamps `supersededBySeq` on the current version
(an MVCC tombstone). Scans already in progress still see the pre-delete version; new
scans do not. This makes delete cheap but has one consequence the durable and Watch
paths must handle (Chapters 8 and 12): the *evidence* of a delete is a stamp on an old
record, which compaction eventually reclaims.

## 6. Durability and group commit

Strict durability means a `Put` does not return until its bytes are safely persisted —
for the persistent variant, `msync`'d to the mapped file. Done naively, that is one
`fsync`/`msync` per write, and throughput collapses to the disk's sync rate.

**Group commit** (`commit.go`) amortizes it. Concurrent writers to a shard coalesce
into one batch that commits under a single sequence bump and a single durability sync.
The protocol is a leader/handoff:

- An uncontended writer becomes the flusher and applies its write directly — a
  fast path with no channels and no allocation.
- Contended writers enqueue and block on a per-request signal; one of them is appointed
  leader, commits the whole queued batch at one fresh `seq`, runs the sync once, and
  signals the rest. Each leader does at most one batch, then hands off, bounding
  latency.

Every write path — `Put`, `Update`, `UpdateOld`, `Delete` — routes through it. The
payoff is real: with a simulated 200 µs sync, ads-per-sync rises from 1 to 12.5 as
writers scale 1→24 (≈12.6× throughput); with a real `msync` on the persistent store,
1→7.9 ads per sync at 16 writers. The design lesson from benchmarking was that write
throughput was **not** limited by lock contention (it scaled with cores) but by
per-call allocation — which is why `Put` was made to encode and commit directly rather
than route through the batch machinery of `Update`.

## 7. Compaction and reclamation

Because segments are append-only and updates/deletes leave superseded records behind,
a shard accumulates dead bytes. Compaction reclaims them; it is **per-shard** and driven
by each shard's garbage ratio.

The hard part is doing it without stopping the world or breaking an in-flight scan. The
compactor runs in three phases, only the first and last under the shard lock:

1. **Snapshot (locked, brief).** Record the set of source segments and *seal* the
   active segment (so concurrent writes go to fresh post-barrier segments).
2. **Recompress (lock-free).** Copy each key's current record into new private
   destination segments — the expensive work, done without the lock, so writers proceed.
3. **Commit (locked, brief).** Transfer any supersession stamps that landed mid-copy
   onto the destination copies (so a record superseded during the copy is not resurrected),
   rebuild the directory with fresh chains, and swap the new segments in.

Reclaiming the *old* segments is a separate concern with two backends:

- **In-memory:** a retired segment is simply unreferenced; the Go garbage collector
  frees it once no scan still holds it. Scans naturally keep the segments they read
  alive.
- **Memory-mapped:** the OS will not reclaim a mapping for us, so the segment backend
  adds pin/reap epoch tracking. A scan `pin()`s the mmap segments it reads; compaction
  or rotation `retire()`s a segment from the live set but defers `munmap`+`unlink`
  until the pin count drains (`reap()`). This lives entirely behind the segment
  backend and governs *file lifecycle only* — MVCC visibility stays the `seq` rule for
  both backends. The same pin/reap machinery is what makes archive rotation safe against
  a query scanning a segment that is rolling out (Chapter 14).

## 8. Persistence and recovery

The persistent `Collection` is the same store with file-backed arenas. It is opened
with `Open(Options{Dir: …})` (an empty `Dir` gives the in-memory `New`); `Close`
flushes and unmaps. It is Unix-first (via `golang.org/x/sys/unix`), and it assumes the
working set still fits in RAM — disk is for durability and fast restart, not for
exceeding memory (that regime is the archive's, Chapter 14).

**Segment files.** Each segment is a file, `ftruncate`'d to its size and `mmap`'d
`MAP_SHARED`. A reserved header holds magic/version and the codec's dictionary id. New
records are written to the mapping; group commit `msync`s the newly-written pages once
per coalesced batch before returning.

**Recovery leans on the MVCC sequence, not on flushing every stamp.** On `Open`, each
shard's directory is rebuilt by scanning the durable region of its segments and
replaying by `seq`: **the current version of a key is its record with the maximum `seq`.**
This is the same max-wins rule the store uses live, and it is what lets updates stay
fast: an update's supersession of the *old* record is an atomic write to a possibly-
unsynced page, but recovery does not need it — max-seq dedup yields the new value
regardless. Two cases need care and get it:

- **A single-current-version invariant.** After replay, any current record that is not
  its key's max-seq winner is marked superseded. This fixes two latent duplicate-scan
  bugs: a crash after compaction (both source and destination files on disk) and a
  non-durable supersession stamp that landed in an already-synced region.
- **Deletes must sync.** A delete writes no new record, so recovery's max-seq rule
  cannot re-derive it; the tombstoned record's page must reach disk. So `Delete` flushes
  the tombstone page. Deletes are comparatively rare, so this does not slow the update
  path.

**Torn writes** are handled with a per-record CRC-32C over the immutable bytes (it
excludes the mutable supersession/chain fields). Recovery scans a segment until the first
bad CRC and stops — a torn tail from a crash mid-write is ignored, and only the committed
prefix recovers.

**Dictionaries persist** as files named by id under the store directory; each segment
file's name records the dictionary it used, so recovery reconstructs the right codec per
segment. Base (identity or configured) codec is not persisted — it is reconstructed from
`Options`.

**Derived state is rebuilt, not persisted.** Secondary indexes (Chapter 10) and the
maintained ordered index (Chapter 16) are not written to disk; they are rebuilt from the
recovered records on `Open`. This is a recurring choice: derived structures are cheaper
to rebuild deterministically than to persist and keep crash-consistent.

Recovery costs about 19–27 µs per ad (50k ads recover in roughly a second). A directory
checkpoint to make restart sub-linear is a possible future optimization; the baseline is
a full replay.

---

# Part III — Querying

## 9. The compiled query engine

A query is a ClassAd boolean expression — `Owner == "alice" && JobStatus == 1 &&
RequestMemory <= 4096`. Evaluating it against millions of ads is the hot path, and the
engine's central performance idea is: **do not build a ClassAd to reject one.**

The query is compiled once (`collections/vm`) from its AST into flat bytecode
(`[]Instr`). The interpreter runs native operations for the hot core — comparisons,
boolean logic, arithmetic, attribute references — and delegates the actual value
semantics (three-valued logic, coercions) to the same exported hooks the tree-walking
`classad` evaluator uses, so results are identical by construction. Anything outside the
native core — a ternary, a list, a function call, a `select`/subscript — compiles to a
single `OpEvalNode` escape hatch that runs the full evaluator on that subtree.

Against a stored ad, the match test uses the cheapest path that is safe, in three tiers:

1. **Wire-native.** If the query is *native* (no `OpEvalNode`), it evaluates directly
   against the encoded bytes: a pluggable resolver reads scalar-literal attributes
   straight out of the wire (`wire.LiteralValue`), building no ClassAd at all. If a
   referenced attribute turns out to be a non-literal expression, the resolver signals
   `fellBack` and the engine drops to the next tier for that ad.
2. **Partial decode.** Decode only the attributes the query reads — the transitive
   closure of referenced names, including `MY`-scope self-references — and evaluate
   against that. Everything else in the ad stays encoded.
3. **Full decode.** For queries that read attributes by a runtime-computed name
   (`eval()`), decode the whole ad.

Only ads that *match* are ever fully decoded to be handed to the caller. The result is
dramatic on selective queries: a real-ad selective query drops from ~167 µs/ad (full
decode of every ad) to ~3.5 µs/ad wire-native — about 48× — and from ~10 allocations per
ad to ~2, with ~44× less scan memory. Partial decode alone is ~15×; the hot header adds
roughly another 2× on that path.

Every tier is **differentially tested to equal a full decode + tree-walk evaluation**,
and the bytecode interpreter is fuzzed against the tree-walker over tens of millions of
executions. Correctness is not traded for the speed.

## 10. Indexing

A selective query should visit few ads, not all of them. The engine indexes attributes
two ways (`index.go`, `index_query.go`):

- **Value indexes** on numeric attributes, for equality and range (`Memory >= 4096`).
- **Categorical indexes** on string attributes, for equality and set membership
  (`Arch == "X86_64"`, `OpSys == "LINUX" || OpSys == "WINDOWS"`).

Indexes are **per-segment**, stored as [roaring bitmaps](https://roaringbitmap.org/) of
record offsets, and they are **built on demand** (`Reindex`) rather than on the write
path — so writes and compaction never block on index maintenance, and a freshly written
segment's *tail*, past the point the index covers, is simply full-scanned. This
reindex-on-demand model is a deliberate decoupling: indexing is a background activity, and
a query on an unbuilt index degrades to a scan rather than stalling a writer.

The query planner is the reusable heart of the index path, and it embodies tenet 3
(soundness by construction):

- **Probes.** `vm.Query.Probes()` extracts the index-satisfiable conjuncts of a query:
  it constant-folds, flattens the top-level `&&` spine, and classifies each conjunct as
  `attr OP literal` (or an OR-of-equalities on one attribute). Anything it does not
  recognize — a disjunction it cannot turn into set membership, a non-literal comparand —
  is simply omitted.
- **Superset + re-verify.** The planner turns probes into candidate offset bitmaps and
  intersects them; the scan then visits only candidates. Crucially, **the full query is
  re-verified against every candidate.** So a probe the planner drops, or an index that
  covers only part of a segment, only costs selectivity — never a wrong answer. Dropping
  a constraint can only *widen* the candidate set.

Two refinements make the index path do less work per query:

- **Boundary-searched value index.** A value index keeps its distinct keys in a sorted
  companion slice (`sortedKeys`, filled at build), so a range probe binary-searches to the
  matching `[from, to)` key run (`sort.Search`) and ORs only those bitmaps rather than
  scanning every distinct value; equality stays an O(1) map lookup. The persisted archive
  index (§14) does the same over its on-disk sorted runs.
- **Per-segment skip + selectivity (`segstats.go`).** Each built segment index carries a
  compact, postings-free summary per indexed attribute: the numeric **min/max** range, a
  categorical **Bloom filter** over the value set, an exact distinct-value count plus a
  mergeable **HyperLogLog** sketch, and the top-N heavy hitters. It earns its keep twice.
  (1) *Segment skip* — before enumerating a segment's candidates, a required equality or
  range conjunct the summary proves unsatisfiable (a value outside `[min,max]`; every probe
  value a Bloom miss) lets the whole indexed prefix be skipped — *provided* no exceptional
  (present-but-wrong-type) record could still match, since those are always re-verified.
  (2) *Probe ordering* — the planner estimates each probe's candidate cardinality from the
  top-N and HLL and intersects the most selective first. These sketches are bounded (≤8 KiB
  Bloom + 1 KiB HLL per attribute per segment); a Bloom hit may be a false positive (only
  forgoing a skip, never dropping a match), so soundness is preserved by construction.

Because indexes are derived state, they are **not persisted** in the live/normal
collection — a persistent collection rebuilds them on `Open` (the same `Reindex` over
recovered segments), which is why an index configuration must be supplied identically at
reopen. (The archive is the exception: its segments are immutable once sealed, so it
persists a write-once sidecar index — §14.) A demand tracker records which attributes
queries filter on, so `SuggestIndexes` can recommend indexes a workload would benefit
from, and the auto-tuner grows/trims indexes against a memory watermark. That watermark is
measured by `IndexSizes`, which reports each attribute's resident **posting** bytes (the
roaring bitmaps + keys) and, separately, its **sketch** bytes (`SketchBytes`: the Bloom +
HLL above) so the sketch overhead is visible rather than hidden — the budget itself is
calibrated on posting bytes.

## 11. Parallel query

A large full-scan query can fan out across segments (`parallel_scan.go`). What makes this
safe and cheap is, again, the MVCC rule — parallelism falls out of invariants already
present rather than needing new machinery:

1. **Segments are immutable once written; reads are lock-free.** Workers share no mutable
   state.
2. **Exactly-once needs no coordination.** The version of a key visible at `S0` lives in
   *exactly one* segment, so workers can take disjoint segments and simply concatenate
   their outputs — no dedup, no shared "seen" set.
3. **Results are order-insensitive** — no merge.

The unit of work is one segment window; workers claim the next with an atomic counter
(at segment granularity a task is milliseconds of scan, so the atomic is free). Each
worker does the whole per-record path — decompress, match, and for matches decode — so
the expensive half (decoding matches) is parallelized; only the final `yield` is serial.
Matches flow back over one bounded, sub-batched channel; an early stop sets an atomic flag
and drains cleanly so pins are always released (never a use-after-unmap).

Fan-out is **gated**. It engages only when the scan is large enough to amortize the setup
and there are at least two segments, and it draws from a **machine-wide worker budget**
(a semaphore sized to `GOMAXPROCS`) taken with a non-blocking greedy try. The reasoning is
that intra-query parallelism helps *latency* at low concurrency but not *throughput* when
many queries already saturate the cores — so under load, queries decline to fan out and
fall back to serial rather than oversubscribing. Measured: a 40k-ad decode-heavy scan is
~8.2× faster at 8 workers; under `GOMAXPROCS` concurrent queries the budget keeps
oversubscription to a graceful ~10–15% cost rather than a collapse.

(Indexed queries and raw-byte scans stay serial for now — indexed scans already visit
far fewer records, and raw scans alias reused buffers — both are straightforward
extensions of the same structure.)

---

# Part IV — Beyond key/value

## 12. Change data capture: Watch

Daemons often need to *follow* a collection, not just query it — rebuild a cache, feed a
dashboard, drive a listener. `Watch` (`watch.go`) delivers, for every add or update, the
**full ad** (not a delta), and a **tombstone** for every delete; a client can disconnect
and later **resume** from an opaque cursor it persisted. Semantics are **at-least-once**:
over-delivery is fine, a missed change is not.

**The cursor is per-shard, by design.** There is no global transaction counter — that
would reintroduce the write contention sharding removed. So the cursor is opaque bytes
that internally hold `{epoch, perShardSeq[]}`: one `commitSeq` per shard, plus a per-store
`epoch` identity. On resume, each shard replays records with `seq >` its cursor entry;
a mismatched epoch (the store was rebuilt from empty) forces a full replay.

**Catch-up then live, with no gap.** The subtle part is joining a historical replay to a
live stream without missing or excessively duplicating events. Watch registers for live
delivery *first*, snapshots the per-shard sequence `S_reg`, then does an exactly-once
catch-up scan filtered to `seq > cursor`, then drains the events buffered during catch-up
and streams live. The union of "catch-up up to `S_reg`" and "live beyond `S_reg`" covers
everything past the cursor with no gap; the overlap is a harmless duplicate.

**Deletes are the one genuinely hard case.** A delete leaves only a supersession stamp,
which compaction eventually reclaims — so a client resuming from far enough back cannot be
told, precisely, which keys vanished. The engine keeps a bounded per-shard **delete
journal** (`Options.WatchHistory` entries): within that window, deletes replay exactly;
beyond it (or on an epoch mismatch), Watch emits a `Reset` and replays all current ads,
and the client reconciles deletions by diffing the replay against the keys it holds. This
is the classic full-state-subscriber fallback, and it is the *only* inherent limit.

The live path hooks the commit point (where `commitSeq` is bumped), reusing the group-commit
structure — one notification per coalesced batch. A slow client that overflows its bounded
buffer is **demoted** (told to resync from its last cursor) rather than allowed to block
writers. The event protocol is small: `Upsert`, `Delete`, `Reset` (discard state; a full
snapshot follows), `Synced` (catch-up done, here is a durable resume cursor), `Resync`
(reconnect). A client applies `Upsert`/`Delete` identically whether catching up or live;
the only control it must honor is `Reset`.

The **archive** has its own Watch (Chapter 14) that is strictly simpler — append-only, so
the cursor is a durable log position (segment id + offset, like a Kafka offset), catch-up
is an oldest-first replay, and there are no deletes to reconstruct. Its cursor even resumes
incrementally across a reopen, because segment ids and its epoch are persisted. (The
collection's epoch is currently per-process, so a reopened persistent collection forces a
full replay — persisting it is a known follow-up.)

## 13. Parent/child chaining (cluster/proc)

HTCondor's job queue is not flat: a proc ad `cluster.proc` inherits shared attributes from
its cluster ad `cluster.-1`, stored once. A query like `DAGManJobId == 42` must see the
cluster's attribute on every proc. This is the one "join" the engine supports, and it is
built from scoped evaluation (`store.go`, `PARENT_CHILD` semantics).

Two options enable it: `ParentKeyFor` maps a child key to its parent key (bindings are
immutable — HTCondor forbids re-parenting), and `IsStructural` marks parent-only ads
(cluster ads), which are stored and used as parents but **hidden** from
Query/Scan/Watch results by default — exactly as `condor_q` omits cluster ads.

**Co-location dissolves the hard problem.** A whole family (a root and its descendants) is
routed to **one shard**, chosen by the family root. Because a parent and its children share
a shard — and therefore a single MVCC snapshot — chained evaluation is consistent and
lock-free, with **no cross-shard fetch**. Cross-shard consistency is the hardest problem in
a sharded store, and co-location makes it never arise here.

**Evaluation and output are handled differently**, matching HTCondor's own behavior:

- *Filtering* (does this ad match?) chains at the **wire level**: the wire-native resolver,
  on a miss, falls through to the co-located parent's wire bytes — so a non-match never
  builds a ClassAd. The decode fallbacks `SetParent` the decoded ad, and the evaluator
  walks the parent chain for unscoped references.
- *Output* (the ad handed back) is **flattened**: the parent's attributes are materialized
  into the child (child overrides), so direct reads and serialization see the inherited
  attributes — like `condor_q` showing a job's full ad.

The scan is two-pass over one snapshot: collect the structural ads, then evaluate each
child with its parent chained, hiding structural ads and flattening matches. Semantics
mirror HTCondor: **no re-parenting**; **auto-delete of an empty parent** when its last
child leaves (HTCondor's `ClusterCleanup`); the archive stores **flattened** standalone ads
so a history record outlives its parent; and Watch **fans out on a parent change**,
re-emitting affected children (excluding parent-private attributes like job-factory
bookkeeping that children do not inherit). The current limitation is that chained
collections use the serial two-pass scan — secondary indexes and query fan-out do not yet
understand chaining.

## 14. The history archive

The `condor_history` file is a different beast: write-once, rotated by age in whole files,
**larger than RAM**, and queried overwhelmingly for *recent* or time-bounded ads — "the
last K matching this constraint, newest first." The engine serves it with a **sibling store
type**, `Archive` (`archive*.go`), that shares the substrate but has its own policy layer.

**The key insight is that the archive removes the store's three most complex subsystems,
and that is exactly what pays for the scale problem:**

| Live `Collection` | `Archive` |
|---|---|
| MVCC `seq`/`supersededBySeq` | none — every record is live from write to rotation |
| Compaction (reclaim garbage) | none — a sealed segment never contains garbage |
| Per-key tombstones / `Delete` | none — deletion is whole-segment drop by age |
| In-RAM directory `map[hash]→loc` | dropped (queries are constraint scans) |
| Index rebuilt as data changes | **immutable sidecar**, built once at seal |

Because a segment is sealed once and never mutated, its index is **write-once**: build it,
`fsync` it beside the data, never invalidate it. Persisting an *immutable* index is trivial
compared to a mutable one — this is why the archive is *easier* exactly where scale makes
the live store hard. What gets harder is purely the > RAM consequence: data, indexes, and
directory can each exceed memory, so every resident structure must be bounded or pageable.

The design that follows:

- **Segment lifecycle.** Append to one active mmap segment; on a size/time threshold, seal
  it, build its index once over the immutable bytes, serialize a sidecar file, and drop the
  in-memory copy. The active (unsealed) segment has no persisted index and is just
  full-scanned — reusing the store's existing "index covers `[0, upto)`, tail is scanned"
  model, since the active segment is all tail.
- **Catalog + zone maps — the highest-leverage feature.** A small persistent catalog lists
  every segment (id, path, length, count, sidecar path) plus per-segment **min/max** for a
  few key attributes (completion time, `ClusterId`, …). Because append order ≈ time order, a
  time-bounded query **prunes whole segments without opening their data or their index**:
  `CompletionDate >= T` skips any segment whose max is below `T`. This turns "show me last
  week's jobs" from a 10⁴-segment scan into a 3-segment scan, at a cost of a few MB of
  always-resident summary. Restart is O(segments): load the catalog and mmap the sidecars,
  not O(records). The catalog is also the **commit point** for structural changes (seal,
  rotation): write it, `fsync`, then act on files, so a torn write recovers the prior
  consistent version.
- **Pageable indexes for > RAM.** A high-cardinality attribute's per-value lookup structure
  would blow the heap long before its bitmap payloads do, so the archive **materializes no
  map**. The sidecar stores each attribute's postings as a **sorted key run** (sorted
  values + parallel bitmap-offset array) queried *directly over the mmap*: equality is a
  binary search, range a boundary scan, and roaring bitmaps are built lazily via zero-copy
  `FromBuffer` for only the postings a probe touches. Resident memory is therefore
  O(#indexed attributes), independent of value cardinality; keys, offsets, and payloads all
  stay demand-paged. Indexes load lazily, so a zone-pruned segment never pages its index in,
  and unmapping routes through the same `pin/reap` hook so a rotation never unmaps under a
  live scan. Each categorical block also carries the **Bloom filter** built at seal (sidecar
  v5): a query for a value the filter proves absent skips the key-blob binary search
  entirely — the win for a large, mostly-categorical history table, where an equality lookup
  for a value not in a segment would otherwise page and binary-search that segment's whole
  sorted key run. (Zone maps already give the numeric analogue; the Bloom is the categorical
  one.) The sidecar is versioned and rebuilt at seal, so a format bump needs no migration —
  an older sidecar is rejected and the segment reindexed.
- **Newest-first + LIMIT pushdown.** Segments (and records within the active tail) are
  visited in reverse chronological order, and a `Limit(K)` stops the scan once K matches are
  yielded — essential when the constraint is broad but the caller wants one page.
- **Rotation** drops the oldest segment: remove it from the catalog (the commit point), then
  `retire()` → deferred `reap()` (`munmap`+`unlink`) once any in-flight scan's pin drains.
  This is exactly the reclamation the persistent store already built, applied to age-based
  drop instead of compaction — no new concurrency design.
- **Single time-ordered log, not hash-sharded.** Hash-sharding would destroy the time
  locality that makes zone-map pruning and newest-first iteration work, and the archive is a
  single appending writer anyway. So the archive is one time-ordered segment log; concurrency
  is reader-vs-rotation, which pin/reap already solves.

The query path composes cleanly: **prune** segments by zone map (reusing `Probes()`),
produce **candidate offsets** from each surviving sidecar exactly as the live index does,
**re-verify** each candidate with the full `vm` predicate (superset + reverify, wire-native),
and decode only true matches — newest-first, with early termination. Results are identical to
a brute-force evaluation over the source ads, fuzz-verified across seal boundaries, pruning,
and LIMIT.

---

# Part V — Negotiator and schedd operations

The engine's final layer implements the HTCondor operations that motivated it. These are
where "a ClassAd database" becomes "the negotiator's and schedd's data plane." The full
proposal and rationale live in the companion design note; this chapter is the standalone
summary.

## 15. Matchmaking: match and rank

The flagship operation: given a job ad, find the slots it *symmetrically matches* — the job
accepts the slot **and** the slot accepts the job — and rank them. This reuses the standard
`classad.MatchClassAd` for the authoritative bilateral test, and layers three optimizations,
each sound because the authoritative match always has the last word.

- **Parallel bilateral scan (A1).** Match fans out across segments like a query. One wrinkle:
  `MatchClassAd` *mutates* both ads' target fields, so a shared job cannot be matched
  concurrently — each worker gets its own deep copy of the job. Results merge with no dedup
  (the MVCC exactly-once rule again). Ranked results are collected and sorted; `MatchSorted`
  returns the top-K by the job's `Rank`.
- **Wire-native job-side reject (A3).** Most slots fail the job's `Requirements`, and fully
  decoding a slot only to reject it is wasteful. So the job's `Requirements` is compiled once
  and evaluated against each slot's *wire bytes* (Chapter 9's wire-native path, with `TARGET`
  resolved from the slot). If the job **definitely** rejects the slot — all referenced
  attributes resolve to literals and the result is boolean false — the full decode and match
  are skipped. Any uncertainty (an undefined, a non-literal attribute, a non-native
  `Requirements`) falls through to the full match, so no true match is ever lost. On a 40k-slot
  pool with half the slots rejected, this takes serial matching from 183 ms to 57.6 ms (3.2×),
  and combined with the fan-out, to 19.1 ms at 8 workers — 9.6× over the original serial
  baseline.
- **Index candidate pre-filter (A2).** When the slots are indexed and the job is selective,
  even the wire-native reject is more work than necessary — better not to *visit* a
  non-candidate at all. The job's `Requirements` is rewritten into a predicate *over the slot*:
  `TARGET.attr` becomes the slot's own attribute, and every job reference is baked to its
  evaluated constant (or, if it is target-dependent, non-scalar, or opaque like `ifThenElse`,
  collapsed to the undefined literal so its conjunct is dropped). Feeding that through the same
  `Probes()` → `planIndex` → candidate-scan path visits only candidate slots, each still
  re-verified by the full match. **This is sound by construction**: the rewrite can only ever
  produce probes on the slot's own attributes against job constants, so a dropped or broadened
  probe merely widens the candidate set. On a 40k-slot pool with a ~2.5%-selective job, this
  takes the wire-reject full scan from 5.78 ms to 1.09 ms (5.3×).

The three tiers compose the way the query engine's do: an index narrows *which* slots are
visited (A2); a wire-native check rejects most of those without decoding (A3); and the full
bilateral match, run in parallel (A1), is the authoritative arbiter for the survivors.

## 16. The maintained ordered index

The schedd walks its idle jobs in a fixed priority order — `JobPrio`, then submission time,
then the order the job entered the queue — per user, to decide what to run next. Re-sorting
that queue every negotiation cycle throws away work, because the ordering key is stable across
a job's lifetime. The engine offers a **maintained, filtered, ordered index** instead: a
secondary index the collection keeps sorted on the write path, so the schedd iterates it
directly — a priority queue that is always ready (`ordered.go`).

It is configured by an `OrderSpec{Partition, Where, Keys, Cluster}` and read via
`Ordered(i, partition, resume)`:

- **Partitioned** by an attribute (e.g. `Owner`) — one independent ordered run per user.
- **Filtered** by a `Where` predicate (e.g. `JobStatus == 1`), so it holds only the ads that
  matter. This is the subtle part: membership churns as jobs transition into and out of idle,
  so state transitions are inserts and deletes, not just "add on submit, remove on completion."
- **Ordered** by composite `Keys` (each ascending or descending), with a **stable insertion
  sequence as the final tiebreaker** — "the order the job entered the queue," surfaced so the
  application never has to reconstruct it.
- **Resumable.** Each step yields the ad plus a cursor that resumes strictly after it, so a
  negotiator can walk a partition across many calls (Chapter 17 covers what stays consistent
  during a long walk).
- **Cluster signature.** `OrderSpec.Cluster` names the attributes whose combined value forms a
  64-bit signature, computed once at write time and surfaced with each ad. The schedd's
  resource-request-list construction is then a run-length fold over the ordered stream — start
  a new RRL when the signature changes — a stored-integer compare instead of re-hashing each
  ad's requirement attributes every cycle. The windowing stays in the schedd; the engine just
  supplies the signature.

**Why a B-tree and not a heap.** A heap gives the top element cheaply but not *resumable,
non-destructive, ordered iteration* — and the schedd wants to walk *down* the list, cycle after
cycle, not pop it. The structure is a [copy-on-write B-tree](https://github.com/tidwall/btree):
writes mutate one master under a lock (an O(log n) insert, delete, or reposition per event —
versus an O(n log n) re-sort of the whole idle set every cycle); reads take an O(1) copy-on-write
snapshot and iterate it lock-free. On a state transition the index does the minimum: attribute
churn that changes neither membership nor key is a no-op; a transition into/out of idle is one
O(log n) insert or delete; a rare priority edit is one O(log n) reposition.

The trade for a heap or a per-cycle sort is the whole point: the schedd's re-sort-every-cycle
cost becomes per-transition log-time maintenance, which wins comfortably whenever the idle set is
large and transitions are bounded by the slots started per cycle (they are).

The index is maintained over the same inherited view the rest of the engine uses: a structural
(cluster) ad is never a member, and a child's `Where`/`Partition`/`Keys` resolve parent
attributes via the parent chain — so a partition key like `Owner` that lives on the cluster ad
still partitions the child procs. Like the value indexes, it is derived state, rebuilt from the
recovered ads on a persistent `Open`.

## 17. Consistency, MVCC, and concurrency

This chapter ties the guarantees together, because "how consistent is a read?" has one answer
for the store and a deliberately different one for the ordered index, and the difference is a
decision worth explaining.

**The store: per-shard snapshot isolation, exactly-once.** A `Scan`/`Query` reads each shard at
a fixed `S0` and yields exactly one version per key (Chapter 5). Note that this is *per-shard*:
a scan concatenates independent per-shard snapshots rather than one global atomic snapshot —
because a global snapshot would need a global sequence counter, reintroducing write contention.
This per-shard level is the engine's consistency contract, and everything reads against it.

**The ordered index: a single mutable structure, not per-segment.** The value indexes are
per-segment and MVCC-versioned (visibility resolved at read by the `seq` rule). The ordered
index is deliberately *not*: it is one in-memory copy-on-write B-tree holding one entry per key,
maintained imperatively. This is a genuine fork in the design, and the reasoning is worth stating
because a reader steeped in LSM/SSTable design would expect a per-segment index:

- Per-run indexes are *forced* in an LSM because on-disk runs are immutable. This index is
  in-memory, derived, and rebuilt on `Open` (never persisted), so it is free of that constraint —
  it is a mutable secondary index, not an LSM per-run index.
- Per-segment would also be the *wrong shape* for the access pattern. The schedd wants a **global**
  ordered walk; per-segment ordered runs would force a k-way merge plus cross-segment version
  resolution on every negotiation cycle — reintroducing exactly the per-cycle merge cost the index
  exists to eliminate. One tree gives an O(1) snapshot and an O(k) walk, and stays **decoupled from
  segment lifecycle** (compaction never has to rebuild it).

**Concurrency control.** Two locks that never contend for long: `oi.mu` guards master mutation and
the O(1) snapshot copy; and each B-tree carries its own `RWMutex`. A walk holds only the *snapshot's*
read lock (a private tree, uncontended); writers take the *master's* lock — different mutexes, no
cross-blocking. Copy-on-write makes shared nodes read-only after the copy (both trees copy-before-
write), so a reader traversing a node while a writer path-copies it is race-free (verified under
`-race`).

**What a long ordered walk sees.** Suppose the negotiator consumes the ordered stream one entry at
a time over many seconds, with network I/O between steps. Nothing blocks — writers keep committing.
The snapshot freezes *order and membership* at the call instant, while each ad is fetched **live**
per step. So a job that leaves idle mid-walk is still visited (its live ad reflects the new state;
the negotiator re-verifies), a deleted one is skipped, a repriced one appears at its old slot with
current content, and a newly-idle one is not seen until a later snapshot. A single long-held walk
also pins the snapshot's nodes, so heavy churn accrues extra memory until it ends. **For long feeds,
chunk with the resume cursor**: each call re-snapshots in O(1) and resumes after the cursor, bounding
pinned memory and picking up members added below the cursor between chunks.

**The write-consistency boundary, stated honestly.** Ordered-index maintenance runs *after* the
store commit, not in the same critical section. Under **serialized per-key writes** — the schedd's
model, where the job queue is a single serialized log — the index is exactly consistent. Under
genuinely concurrent writes to the *same* key from different goroutines, the index can converge to a
stale version until that key's next write (the store keeps the authoritative version by max-`seq`;
the index self-heals). Making it fully correct there would require either per-segment indexes (the
expensive-read path above) or version tombstones in the index (unbounded state for a filtered index
whose non-members dominate) — neither warranted for the single-writer-per-key target. This is a
chosen boundary, not an oversight.

---

# Part VI — Reflection

## 18. Design principles and decisions

Five ideas recur; the engine is largely their consequences.

1. **Reuse the substrate.** Every capability is the wire format + the `vm` engine + the segment
   backend + the reclamation machinery, plus a thin policy layer. Matchmaking reuses the parallel
   scan and the wire-native evaluator; the archive reuses the segment/index/reclamation stack and
   drops what it does not need; the ordered index reuses the recovery scan to rebuild. There is one
   engine, not five.
2. **Soundness by construction.** Every fast path is a *superset* filter with an authoritative
   re-verify behind it: index probes, wire-native reject, the match pre-filter, zone-map pruning.
   The property is always "a wrong fast-path decision costs selectivity, never correctness," and it
   is what makes aggressive optimization safe to ship.
3. **Consistency is the product.** The MVCC `seq ≤ S0 < supersededBySeq` interval is the single
   invariant under exactly-once scans, safe parallelism, safe compaction, and deterministic
   recovery. Where a stronger guarantee (a global snapshot, a fully MVCC ordered index) would cost
   contention or the wrong access pattern, the weaker guarantee is chosen *explicitly* and its
   boundary documented (Chapter 17), not left implicit.
4. **Derived state is rebuilt, not persisted.** Indexes and the ordered structure are reconstructed
   from recovered records on `Open`. Rebuilding deterministically is cheaper and less bug-prone than
   persisting a structure and keeping it crash-consistent.
5. **Gate the complexity; measure the gain.** Parallel fan-out, the match pre-filter, dictionary
   training, and hot headers all *engage only when they pay* — a size threshold, a worker budget, a
   selective job, a built index — and each shipped behind a benchmark that justified it. Complexity
   that does not pay for itself is left on the serial path.

A selection of concrete decisions and their rationale:

| Decision | Rationale |
|---|---|
| Self-delimiting wire value nodes | Skip/inspect any attribute without decoding it — the basis of the whole fast path |
| Interned names in-memory, inline on disk | Density where cheap; self-contained, easily-recovered records where durability matters |
| Per-segment trained ZSTD dictionaries | 6.5× density on repetitive ads without a shared, crash-fragile string table |
| Append-only segments + MVCC `seq` | Lock-free reads, exactly-once scans, trivial parallelism, max-`seq` recovery |
| Group commit | Amortize `msync` across concurrent writers (≈12× under a slow sync) without weakening durability |
| Reindex-on-demand (not on write) | Writes and compaction never block on index maintenance; unbuilt index degrades to a scan |
| Co-locate a cluster/proc family on one shard | Chained evaluation is consistent and lock-free; the cross-shard fetch never arises |
| Archive as a sibling type, not a `Collection` mode | Dropping MVCC/compaction/directory is what pays for the > RAM scale; bolting flags on would erode live-store invariants |
| Ordered index as one COW B-tree, not per-segment | Matches the schedd's global ordered-walk access pattern; O(1) snapshot, decoupled from compaction |
| Wire-native reject + index pre-filter for Match, both re-verified | 9.6× then 5.3× on selective matchmaking, with zero correctness risk |

## 19. Correctness and testing

The optimizations in this book are only tenable because correctness is pinned down independently and
continuously.

- **Differential fuzzing against HTCondor's C++ libclassad.** The underlying ClassAd evaluator is
  fuzzed against HTCondor's own libclassad, loaded in-process, on the assumption that C++ is correct
  unless the behavior is "very strange." The divergence rate is down to roughly 5 per 360k random ads,
  all deeply pathological; genuine C++ quirks that Go deliberately does not mirror are documented for
  upstream rather than copied. The parser is fuzzed the same way. This is why "evaluate via the same
  hooks as the tree-walker" (Chapter 9) is a real guarantee, not an aspiration.
- **Differential testing of every fast path against a brute-force oracle.** Wire-native evaluation,
  partial decode, the index candidate scan, the parallel scan, the match pre-filter, and archive
  queries are each tested to produce exactly the result set of a full decode + full evaluation over
  the source ads.
- **Exactly-once under the race detector.** The MVCC invariant is stress-tested with concurrent
  updates, deletes, compaction, dictionary retraining, and indexed reads running together under
  `-race` — the ordered index's copy-on-write model included.
- **Crash and recovery tests.** Reopen with updates and deletes; a truncated (torn) tail recovers only
  the committed prefix; a deleted key stays deleted; retrained dictionaries reconstruct across restart;
  compaction reclaims files without duplicating on the next scan.
- **A benchmark matrix.** A sweep over {format × workload × query type × concurrency} produces the
  performance tables, so a change that regresses a regime is visible.

---

## Appendix A: Performance summary

Representative figures from a 12-core development box on a real OSPool ad corpus. They are
directional, not a spec sheet.

| Area | Baseline | Optimized | Factor |
|---|---|---|---|
| Density (bytes/ad) | 8681 (uncompressed) | 1286 (ZSTD + dict) | 6.5× |
| Selective query | 167 µs/ad (full decode) | 3.5 µs/ad (wire-native) | ~48× |
| Query allocations | ~10/ad | ~2/ad | 5× |
| Group commit (200 µs sync) | 1 ad/sync | 12.5 ads/sync @ 24 writers | ~12.6× |
| Persistent recovery | — | ~19–27 µs/ad (50k ≈ 1 s) | — |
| Parallel query (40k, decode-heavy) | 7038 ms (serial) | 855 ms @ 8 workers | 8.2× |
| Matchmaking (40k, half reject) | 183 ms (serial) | 19.1 ms (wire-native + 8 workers) | 9.6× |
| Matchmaking, selective + indexed | 5.78 ms (wire-reject scan) | 1.09 ms (index pre-filter) | 5.3× |
| Old-ClassAd ingest | parse + Put | `UpdateOld` fast path | ~3× |

## Appendix B: API sketch

The primary surface, abbreviated. Types are illustrative, not exhaustive.

```go
// Construction
func New(opts Options) *Collection                  // in-memory
func Open(opts Options) (*Collection, error)         // Dir set ⇒ persistent (recovers)
func (c *Collection) Close() error

type Options struct {
    Shards           int
    SegmentSize      int
    Codec            Codec           // compression (default identity)
    HotAttrs         []string        // front-loaded in the hot header
    CategoricalAttrs []string        // string equality / set-membership indexes
    ValueAttrs       []string        // numeric equality / range indexes
    Ordered          []OrderSpec     // maintained filtered ordered indexes
    Dir              string          // "" ⇒ in-memory; set ⇒ persistent
    QueryParallelism int             // 0 auto, 1 serial, N cap
    WatchHistory     int             // >0 enables Watch (per-shard delete journal)
    ParentKeyFor     func(key []byte) []byte // cluster/proc chaining
    IsStructural     func(key []byte) bool
    CommitSync       func()          // durability hook (group-committed)
    // ...
}

// Point + batch
func (c *Collection) Put(key []byte, ad *classad.ClassAd) error
func (c *Collection) Update(batch []AdUpdate) error
func (c *Collection) UpdateOld(batch []OldAdUpdate) error   // socket-form fast path
func (c *Collection) Get(key []byte) (*classad.ClassAd, bool)
func (c *Collection) Delete(key []byte) bool

// Query
func (c *Collection) Scan() iter.Seq[*classad.ClassAd]
func (c *Collection) Query(q *vm.Query) iter.Seq[*classad.ClassAd]
func (c *Collection) Reindex()

// Matchmaking
func (c *Collection) Match(job *classad.ClassAd) iter.Seq[*classad.ClassAd]
func (c *Collection) MatchSorted(job *classad.ClassAd, limit int) []*classad.ClassAd

// Ordered index (schedd priority queue)
type OrderSpec struct {
    Partition string    // e.g. "Owner"
    Where     string    // membership predicate, e.g. "JobStatus == 1"
    Keys      []SortKey  // composite order; each ascending/descending
    Cluster   []string   // attributes forming the RRL cluster signature
}
func (c *Collection) Ordered(index int, partition classad.Value, resume OrderCursor) iter.Seq[OrderedAd]
// OrderedAd = { Ad *classad.ClassAd; Cursor OrderCursor; Signature uint64 }

// Change data capture
func (c *Collection) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error)

// Archive (append-only, > RAM, rotated)
func CreateArchive(opts ArchiveOptions) (*Archive, error)
func OpenArchive(opts ArchiveOptions) (*Archive, error)
func (a *Archive) Append(ad *classad.ClassAd) error
func (a *Archive) Query(q *vm.Query) iter.Seq[*classad.ClassAd]              // newest-first
func (a *Archive) QueryLimit(q *vm.Query, limit int) iter.Seq[*classad.ClassAd] // + LIMIT pushdown
func (a *Archive) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error)
```

---

*This book describes the engine as built. Where a capability is partial or a follow-up is
planned — the collection's per-process Watch epoch, chaining-aware indexes and match fan-out, a
recovery checkpoint for sub-linear restart — the relevant chapter says so. Corrections and
questions from the HTCondor community are the point of circulating it.*
