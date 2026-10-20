# A ClassAd Database Engine: Design and Decisions

*A design book for the HTCondor community.*

This document describes the design of `collections`, an embeddable, high-performance
storage-and-query engine for ClassAds written in Go
(`github.com/PelicanPlatform/classad/collections`, Go 1.26). It also covers the `db`
package built on top of it. One substrate serves several of HTCondor's ClassAd-shaped
problems:

- the live in-memory collection a daemon queries;
- the durable on-disk variant that survives restart;
- the append-only history archive that outgrows RAM;
- the negotiator and schedd operations layered on top: matchmaking, ranking, and the
  priority-ordered job queue.

The `db` package adds named tables, transactions, encryption, views and change export,
which together form the database that htcondordb serves.

It is meant to be read end-to-end. It starts from the ideas and gets progressively
more detailed: early chapters give the shape, later ones the mechanism and the
rationale for each decision. Source files are named where they help navigation
(e.g. `shard.go`), but the text is self-contained. External references are linked
inline. Appendix C collects the on-disk formats in one place for readers who need
the file layout without the narrative.

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
7. [Compaction and segment rewrites](#7-compaction-and-segment-rewrites)
8. [Persistence, recovery, and the on-disk layout](#8-persistence-recovery-and-the-on-disk-layout)
9. [Columnar segments](#9-columnar-segments)
10. [Encryption at rest](#10-encryption-at-rest)

**Part III — Querying**
11. [The compiled query engine](#11-the-compiled-query-engine)
12. [Indexing](#12-indexing)
13. [Parallel query](#13-parallel-query)
14. [Aggregates, projection, and top-K](#14-aggregates-projection-and-top-k)

**Part IV — Beyond key/value**
15. [Transactions, delta records, and time travel](#15-transactions-delta-records-and-time-travel)
16. [Change data capture: Watch](#16-change-data-capture-watch)
17. [Parent/child chaining (cluster/proc)](#17-parentchild-chaining-clusterproc)
18. [The history archive](#18-the-history-archive)

**Part V — Negotiator and schedd operations**
19. [Matchmaking: match and rank](#19-matchmaking-match-and-rank)
20. [The maintained ordered index](#20-the-maintained-ordered-index)
21. [Consistency, MVCC, and concurrency](#21-consistency-mvcc-and-concurrency)

**Part VI — The database layer**
22. [The `db` package: tables, catalog, views, exporters](#22-the-db-package-tables-catalog-views-exporters)

**Part VII — Reflection**
23. [Design principles and decisions](#23-design-principles-and-decisions)
24. [Correctness and testing](#24-correctness-and-testing)

**Appendices**
- [A. Performance summary](#appendix-a-performance-summary)
- [B. API sketch](#appendix-b-api-sketch)
- [C. On-disk reference](#appendix-c-on-disk-reference)

---

# Part I — Foundations

## 1. Why a ClassAd database engine

HTCondor stores and queries ClassAds everywhere: the schedd's job queue, the
collector's daemon ads, the negotiator's view of slots, the history file of completed
jobs. Each of these is, at heart, *a set of ClassAds you insert, update, delete, and
query by expression*. Each has historically grown its own storage and its own query
loop. The recurring costs are the same:

- decoding ads to evaluate a constraint;
- re-sorting a queue every negotiation cycle;
- holding more in memory than a process should;
- keeping an index consistent with the data.

This engine takes the position that these are **one problem with several policy
layers**. A single substrate provides:

- a **dense binary wire format** for ClassAds that can be filtered without being fully
  decoded;
- **columnar sealed segments**, which store each common attribute once per row group,
  so scans, counts and aggregates read a column rather than every ad;
- a **compiled query engine**, with a scalar and a vectorized executor, that evaluates
  a constraint directly against encoded bytes or columns;
- a **sharded, MVCC, append-only store** with snapshot-isolated, exactly-once scans and
  optimistic multi-writer transactions;
- **secondary indexes** (value, categorical, key, and ordered) built over the same
  bytes and served from memory-mapped sidecars;
- **durability**: memory-mapped arenas, group-committed writes, crash recovery, and
  verified reads;
- on top, the HTCondor-specific verbs: **matchmaking**, **ranking**, a **maintained
  priority-ordered index** for the schedd, **change subscriptions**, and **point-in-time
  queries**.

The same `Collection` type is instantiated three ways, differing in policy rather than
mechanism:

| Instantiation | Regime | What it drops / adds |
|---|---|---|
| **In-memory** (`New`) | working set fits in RAM | the default. Segments live on the heap; sealed segments' indexes move to anonymous mmap sidecars. No columnarization |
| **Persistent** (`Open` with `Dir`) | survives restart; values and keys can exceed RAM | file-backed mmap arenas, `msync` durability, recovery. Sealed segments are interned, columnarized, and served from mmap sidecars; the key directory pages out |
| **Append-only** (`Options.AppendOnly`, wrapped as `Archive`) | larger than RAM, write-once, rotated | one shard, no supersession or per-key delete, no garbage compaction. Adds newest-first scans, zone maps, retention rotation, and segment merging |

An earlier design made the archive a separate store type. It was folded back into
`Collection` as a mode (Chapter 18) once the shared substrate — segments, sidecars,
pin/reap, columnar blocks — made a second copy of the engine pure duplication.

### Design tenets

Five commitments recur through every chapter. They explain most of the decisions, so
they are stated up front:

1. **Consistency over cleverness.** Where a choice trades a small performance gain for
   a weaker guarantee, we keep the guarantee. Scans are exactly-once, recovery is
   deterministic, and persisted caches are validated before they are trusted.
2. **Reuse the substrate.** Every new capability — indexing, matchmaking, the archive,
   the ordered index, columnar storage — is built from the wire format, the `vm` engine,
   the segment backend, and the reclamation machinery. New code is policy, not a
   parallel stack.
3. **Soundness by construction.** A fast path takes one of two forms, and every fast
   path must say which:
   - A **superset filter** (index probes, zone maps, the match pre-filter) may *widen*
     the candidate set but never drop a true result. The authoritative evaluator
     re-verifies every candidate.
   - An **exact answer** (a count from a posting list, a match decided from columns)
     replaces evaluation outright. It is allowed only where the answer is provably the
     evaluator's answer, and it must *decline* — hand the record or segment to the
     ordinary path — whenever a value is an expression, of an unexpected type, or
     otherwise outside what it models.

   Either way a wrong fast-path decision may cost speed, never correctness. Exact paths
   carry more risk, and Chapter 24 describes the differential tests that guard them.
4. **Pay for what you touch.** Filtering reads only the attributes a query mentions. A
   rejected ad is never reassembled or fully decoded. An unbuilt index just full-scans,
   and an unwatched store pays nothing for Watch.
5. **Prove it with differential tests.** The engine is checked against a brute-force
   oracle, and the evaluator against HTCondor's own C++ libclassad, continuously and
   under the race detector.

## 2. The ClassAd data model

A ClassAd is an unordered set of attributes, each mapping a case-insensitive name to
an *expression*. Expressions are evaluated lazily and in a specific context. The same
attribute can be a literal (`Memory = 8192`), a computation
(`RequestMemory = TARGET.Memory / 2`), or a reference to another ad's attributes.
Evaluation is **three-valued** (true/false/undefined, plus an error value), with
type coercions. The engine must preserve this exactly, because HTCondor's policy
expressions depend on it.

One consequence recurs throughout this book. The boolean operators `&&` and `||` are
**not commutative**: they short-circuit left to right, and an ERROR on the left is
returned before the right side is looked at. `ERROR || TRUE` is ERROR, while
`TRUE || ERROR` is TRUE. Any optimization that reorders, rewrites, or partially
evaluates an expression must respect that.

Two features of the model shape everything downstream:

- **Scoped references.** An unscoped name resolves in the current ad. `MY.x` names the
  current ad explicitly; `TARGET.x` is the *other* ad in a match; `PARENT.x` walks an
  enclosing scope. Matchmaking and chained (cluster/proc) ads both lean on this.
- **Bilateral matchmaking.** Two ads *match* when each one's `Requirements` expression,
  evaluated with the other as `TARGET`, is true. A job and a slot match only if the
  job accepts the slot **and** the slot accepts the job. `Rank` expressions then order
  the acceptable candidates. This symmetry is the negotiator's core operation, and
  Chapter 19 builds it on the engine.

The engine must therefore support:

- point `Get`/`Put`/`Delete` by key, and `Update` in batches;
- optimistic transactions (`Begin`/`Commit`);
- `Scan` and `Query(constraint)`, including point-in-time queries and projections;
- counts and grouped aggregates;
- `Match(job)` and ranked match;
- a maintained ordered iteration (the schedd queue);
- `Watch` subscriptions.

The rest of the book is how each is made fast and consistent.

The engine builds on a separate, standalone Go implementation of the ClassAd language:
the `classad` and `ast` packages, which provide the parser, the evaluator, three-valued
logic, and the standard function library. That implementation is itself validated by
differential fuzzing against HTCondor's C++ libclassad (Chapter 24). The engine reuses
its evaluator for every authoritative result, and its compiled and vectorized
executors delegate value semantics to the same hooks, so semantics are identical by
construction.

---

# Part II — The storage substrate

## 3. The wire format

A query should not have to reconstruct a whole ClassAd object to test a predicate.
That principle drives the record representation (`collections/wire`).

An ad is encoded as a sequence of `(name, value-node)` entries. The value node is the
crux: **every attribute value is a self-delimiting expression node**:

- A literal integer is a one-byte tag plus a varint.
- A string is a tag plus a length plus bytes.
- A compound expression (arithmetic, a function call, a list) is its AST serialized
  node by node.

Because each node is self-delimiting, a reader can **skip** any value it does not care
about without parsing it (`skipNode`). A zero-copy `Ad` accessor can `Lookup` or
`ForEach` over attributes without allocating. This is what lets the query engine
(Chapter 11) read `TARGET.Memory` straight out of the bytes and ignore the other
forty attributes of a slot ad.

The wire form is the unit of the *row* representation: every record in an active
segment, every record of an in-memory collection, every ad on the network, and the
reassembled output of a columnar segment (Chapter 9). Sealed persistent segments store
most values in columns instead, and keep a wire-form *remnant* per record for whatever
the columns do not hold.

### Byte layout

An encoded ad is a 3-byte header, an optional hot-attribute directory, then the
attribute entries:

```
[magic:1=0xCA] [version:1=1] [flags:1]
(if flags has flagStandalone:  an embedded intern table — omitted for stored ads)
[hotCount: uvarint]   hotCount × ( key, uvarint(offset) )            // the hot directory
[attrCount: uvarint]  attrCount × ( key, value-node )                // the entries region
```

The flags byte has four bits in use:

| Bit | Name | Meaning |
|---|---|---|
| `0x01` | `flagStandalone` | an intern table is embedded, so the ad decodes on its own |
| `0x02` | `flagInlineNames` | keys are `uvarint(len)+bytes` names rather than intern ids; refs, funcs and selects use the inline-name node variants |
| `0x04` | `flagHotClosure` | the hot directory holds the *complete* match closure of this ad (Chapter 19) |
| `0x08` | `flagDelta` | the record holds only the attributes one write changed (Chapter 15) |

The `key` is a `uvarint(id)` for an interned ad, or `uvarint(len)+bytes` for an
inline-names ad. In the hot directory, an interned ad keys by id and an inline-names ad
keys by a 32-bit case-folded FNV-1a hash of the name. Each hot `offset` is **relative
to the start of the entries region**, not the ad. That avoids a chicken-and-egg problem:
an absolute offset would depend on the header's own size, which depends on how many
bytes those offsets take to encode.

The offset points at the *value node* for an interned ad and at the *entry* (name plus
node) for an inline-names ad. The encoder also writes an inline-names ad's hot entries
**first**, so the hot attributes form a physical prefix of the record. That lets a
reader decompress only that prefix (`Codec.DecompressPrefix`) when it needs nothing
else.

Value-node tags are a small closed set:

| Tag | Meaning | Payload |
|---|---|---|
| `0x00`–`0x03` | undefined / error / false / true | *(none — the tag is the value)* |
| `0x04` | integer | zigzag varint |
| `0x05` | real | 8 bytes IEEE-754 LE |
| `0x06` | string | `uvarint(len)` + bytes |
| `0x07` | attribute ref | scope byte + `uvarint(nameID)` |
| `0x08` / `0x09` | binary / unary op | op byte + operand node(s) |
| `0x0A`–`0x11` | list, nested record, function call, conditional, elvis, select, subscript, paren | children encoded inline, pre-order |
| `0x12` / `0x13` | binary / unary op by name | fallback for an operator outside the closed table |
| `0x14`–`0x16` | attribute ref, function call, select with an inline name | used in inline-names ads |
| `0x17` | encrypted value | `uvarint(len)` nonce + `uvarint(len)` AES-GCM ciphertext of the inner node (Chapter 10) |

Worked example: the ad `[ Arch = "INTEL"; Cpus = 4; Requirements = TARGET.Memory >= 2048 ]`,
interned with illustrative ids `Arch=10`, `Cpus=11`, `Requirements=12`, `Memory=13`, and
`Cpus` declared hot:

```
CA                        magic
01                        version 1
00                        flags = 0  (interned, not standalone)
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

`Cpus`'s hot offset is 9 because its value node begins 9 bytes into the entries region:
the `Arch` entry is 8 bytes (1 id + 7 node). A `Lookup(Cpus)` reads the node directly
at `entriesStart + 9`. A `Lookup(Arch)`, which is not hot, skips forward node by node
with `skipNode`. Entries are written in the ad's attribute order, with no sorting.

### Attribute names: three modes

Attribute names are stored in one of three ways, chosen by where a record lives:

- **Process-interned** (in-memory collections). A shared `InternTable` maps each name
  to a small integer id, case-folded but case-preserving. Records store the id, so a
  pool of ads that all have `Owner`, `JobStatus`, `RequestMemory`… pays for those
  strings once. These ids are **process-local**: they are assigned in first-seen order
  and differ from run to run, so nothing durable may embed them.
- **Inline** (the active segment of a persistent collection). Each record stores its
  names verbatim, so a record is self-contained and recoverable without any table.
- **Segment-interned** (sealed persistent segments). When a segment is rewritten —
  by compaction, by an archive retrain or rewrite, or at seal with `InternAtSeal` — its
  records are re-encoded against a **per-segment dictionary**. The dictionary is
  stored as a keyless record inside the segment itself (`segdict.go`). It holds a
  minimal perfect hash for name lookup, a sorted-id fallback, an id-to-offset table,
  and the name bytes, and it is probed zero-copy over the mmap.

Segment interning keeps the property that made inline names attractive in the first
place: a segment is self-contained, with no global, crash-fragile string table to
persist. It also recovers most of interning's density. On a measured job-ad corpus,
interned records are 56% smaller than inline ones *decompressed* and decode about 26%
faster. On disk, after ZSTD with a trained dictionary, the gain shrinks to 7–9%,
because the dictionary had already absorbed most of the repeated names.

Mutable persistent collections intern every compaction output. Append-only collections
intern when a segment is retrained or rewritten, or eagerly at seal if `InternAtSeal`
is set (off by default). The active segment always stays inline. Encryption composes
with interning: names stay in the clear in the dictionary, and values become sealed
nodes.

### The hot header

`Options.HotAttrs` names the "popular" attributes a workload filters on (e.g. `Cpus`,
`Memory`, `Arch`, `OpSys`, `State`). The encoder front-loads those, storing
`(key, entries-relative offset)` pairs, so `Lookup` of a hot attribute is O(1) instead
of a scan of the entry list. On the partial-decode path this is roughly a 2× win.
Popularity can be refreshed automatically from observed access frequency
(`RefreshHotSet`), computed by walking wire bytes with no decode.

`Options.MatchClosureRoots` (e.g. `Requirements`, `Rank`) asks for more. The encoder
computes the transitive set of attributes those expressions read from the ad itself and
puts all of them in the hot header. When that closure is statically determinable (no
`eval()`), it sets `flagHotClosure`, promising that every match-relevant attribute is
in the hot directory. A matcher can then read them with `ForEachHot` and stop
(Chapter 19).

### Streaming construction and direct ingest

A `StreamEncoder` builds a wire ad one attribute at a time; scalar setters write
literals with no AST at all. Ads arriving on a socket in HTCondor's old ClassAd text
form can therefore become wire bytes without an intermediate object. `UpdateOld`
parses that socket form with a conservative scalar fast path, which recognizes
non-negative ints, reals, escape-free ASCII strings, and the keyword literals. It falls
back to the full parser only for computed values or awkward inputs. End to end this is
about 3× faster, with 3× fewer allocations, than parse-then-`Put`. It is differentially
tested to produce identical results.

### Layout history: interleaved rows, then columns

The row body **interleaves** key and value — `(id, node)(id, node)…` — and the hot
directory is a *sparse* index over it, storing an offset only for the attributes a
workload touches. The original design weighed this against a **fully split** layout,
with a complete key-and-offset directory followed by all value nodes:

- A full directory gives O(1) access to *cold* attributes and contiguous name-only
  scans.
- The interleaved, sparse-hot layout is smaller and decodes a whole ad in one
  sequential pass.

The bet was that HTCondor's access pattern is predictable: queries filter on a small,
stable set of attributes, and output materializes the whole ad. That bet still governs
the *row* form.

What changed is that the dominant cost for large, sealed data turned out to be neither
cold-attribute lookup nor full decode. It was **compression across records**:
per-record compression cannot see that ad *n* and ad *n+1* share 500 attributes and
most of their values. The response was not a different row layout but **columnar
sealed segments** (Chapter 9). They store each common attribute once per row group,
are 5× denser than per-record compression, and are faster to scan and aggregate. The
interleaved row survives where it fits: in-flight writes, in-memory collections,
reassembled output, and per-record remnants.

Uncompressed, the wire form is roughly 64% of the ClassAd text size. The real wins
come from compression and columns, next.

## 4. Compression: ZSTD with trained dictionaries

Job and slot ads are extraordinarily repetitive: the same attribute names, the same
`Requirements` boilerplate, the same enumerated string values, ad after ad. General
compression captures some of this, but the redundancy is *across* ads and each ad is
small, so a per-ad ZSTD stream cannot see it. The answer for row records is a
**trained dictionary**.

The engine trains a ZSTD dictionary on the **wire bytes** of a sample of the collection,
not the text form: you compress the wire form, so that is what the dictionary must
model. Each record is then compressed against it. On a real OSPool sample, measured
when the row form was the only form:

| Encoding | Bytes/ad |
|---|---:|
| Identity (uncompressed wire) | 8681 |
| ZSTD, no dictionary | 4117 |
| **ZSTD + trained dictionary** | **1286 (6.5×)** |

Columnar segments improve on this again: 3445 → 673 B/record (0.195×) on OSPool machine
ads, measured against dictionary-compressed rows (Chapter 9).

Engineering points earned in practice:

- **Build the dictionary correctly.** The `klauspost/compress` `BuildDict` does not
  select dictionary content for you: you supply the content (`History`) and the repeat
  offsets. Training on the wrong bytes, or omitting the offsets, yields an empty or
  invalid dictionary — a subtle failure worth flagging to anyone reusing the library.
  Training can also hit a degenerate divide-by-zero inside the library on some samples,
  so it is wrapped to return an error and keep the prior codec rather than crash the
  process. The dictionary's frame ID is fixed, so a frame does not name its dictionary;
  the segment does.
- **Dictionaries are per segment.** A segment's file name records the dictionary it was
  written under (`seg-<n>.d<dictID>.dat`), and recovery reconstructs every registered
  dictionary, so records always decode with the codec that wrote them. Dictionary bytes
  live in `dicts/<id>.zst`. Dictionary 0 is the base codec, rebuilt from `Options`
  rather than stored.
- **Retraining differs by store type.** `RetrainDict` trains on a recent sample,
  registers the new dictionary, and points new writes at it.
  - In a **mutable** collection it then recompacts every shard under the new codec,
    since records are being rewritten anyway.
  - In an **append-only** collection it stops there. Recompressing a whole history
    archive is hours of I/O for a gain that is often small and sometimes *negative*: a
    dictionary trained on recent ads can compress older ones worse once the
    distribution has moved. Re-encoding old segments is a separate, budgeted pass
    (`UpgradeCodecPass`). It measures each segment, re-encodes only where the new
    dictionary is meaningfully better, and remembers "no" verdicts per dictionary
    generation in `upgrade.json` so they are not re-measured. `Rewrite` remains the
    explicit "re-encode everything now" escape hatch.
- **Automatic upkeep.** `StartAutoRetrain` retrains periodically (15 minutes by
  default) and refreshes the hot set at the same time. Dictionaries no segment
  references any longer are moved to `dicts/attic/` rather than deleted, and idle
  encoders are released. `CodecStats` reports per-codec usage.

Columnar row groups do **not** use the trained dictionary. Their regions are compressed
with a dictionary-less *region codec*, because a column of like values already has the
redundancy the dictionary would supply. Decompressed regions are held in a
process-global block cache (Chapter 9).

Because a stable job-ad schema is exactly the case a trained dictionary loves, this is
also why durable storage never needed a shared global intern table. The dictionary,
then the per-segment intern dictionary, then columns, each recovered density without
one.

## 5. The sharded MVCC store

The store (`store.go`, `shard.go`, `segment.go`) is where keys, versions, and bytes
live. Its structure follows from two requirements: concurrent writers must not
serialize on one lock, and scans must see a consistent, exactly-once view while writes
proceed.

**Sharding.** The keyspace is partitioned into independently locked *shards*. The count
is a power of two, 16 by default; an append-only collection always uses one. A key's
shard is chosen by hash — of the *family root* when parent/child chaining is on
(Chapter 17). Each shard owns its own directory, its own arena, and its own commit
sequence, so writers to different shards never contend.

**Arenas of append-only segments.** Within a shard, records live in *segments*:
append-only byte arenas, the engine's equivalent of SSTables, 8 MiB by default. New
versions are appended. Nothing is mutated in place except two small atomic fields.
Every record starts on an 8-byte boundary, so those fields are naturally aligned:

```
off+0   seq             u64   commit seq at which this version became current (immutable)
off+8   supersededBySeq u64   commit seq at which it was overwritten/deleted; max while current (atomic)
off+16  nextSeg         u32   hash-bucket chain: segment id of the next record
off+20  nextOff         u32   hash-bucket chain: offset of the next record
off+24  totalLen        u32   whole record length, a multiple of 8
off+28  keyLen          u32   low 28 bits: key length;  high 4 bits: record-kind flags
off+32  key             bytes
        adLen           u32
        ad              bytes the (usually compressed) wire ad
        crc32c          u32   over the immutable bytes (excludes supersededBySeq and the chain)
        padding               to totalLen
```

The four high bits of `keyLen` say what kind of record this is, readable without
decompressing anything:

| Bit | Flag | Record |
|---|---|---|
| 31 | `markerFlag` | a keyless *marker*, skipped by every key walk. Alone, it is a time-travel checkpoint (Chapter 15) |
| 30 | `dictFlag` (+ marker) | the segment's attribute dictionary (Chapter 3) |
| 29 | `colFlag` (+ marker) | the segment's columnar payload (Chapter 9) |
| 29 | `colFlag` (no marker) | a *remnant*: a keyed record whose schema'd values live in the columnar payload |
| 28 | `deltaFlag` | a delta record holding only changed attributes (Chapter 15) |

All four bits are now in use. New record kinds will need either a second header word or
a combination of existing bits.

**The directory.** A per-shard map, `keyhash → location`, points at the head of each
hash bucket's chain of records; lookups compare the full key inline to resolve
collisions. In a persistent collection the directory is **no longer complete**.
- When a segment seals and its key index is built (Chapter 8), the keys whose current
  record lives in that segment are evicted from the map.
- A lookup that misses the map probes the sealed segments' key indexes, which are
  mmapped and pageable and fronted by a Bloom filter.

Resident directory memory is therefore proportional to the keys in *active* segments,
not to the whole store. This works without any LSM-style merge because currency is a
local property of a record: at most one version of a key anywhere has
`supersededBySeq` = max, and finding it needs no cross-segment ordering.

**MVCC visibility.** Every record carries a commit sequence `seq`. When a newer version
(or a delete) supersedes it, its `supersededBySeq` is stamped atomically, wherever the
record lives; sealed segments stay mapped read-write for exactly this. A scan captures
the shard's current sequence `S0` at snapshot time and yields exactly the records with
`seq ≤ S0 < supersededBySeq`. This single interval rule gives:

- **Snapshot isolation**: a scan sees the store as of `S0`, ignoring later writes.
- **Exactly-once**: for any key that existed at `S0`, *precisely one* version
  satisfies the interval, so there are no duplicates and no skips.

This is the load-bearing invariant of the whole engine. It is what makes parallel scans
trivially correct (Chapter 13: disjoint segments, no cross-segment dedup). It makes
compaction safe against in-flight scans (Chapter 7), and it is what recovery
reconstructs (Chapter 8). It holds under the race detector with concurrent updates,
deletes, and compaction running together.

Two refinements sit on top of the rule:

- **Delta records.** With `DeltaMax` set, a version may hold only the attributes its
  write changed. The visible version is still chosen by the interval rule, but reading
  it walks back along the key's chain, merging deltas over the most recent full record
  (Chapter 15).
- **Read verification.** By default every record read off a persistent segment has its
  CRC checked. A record that fails is **skipped and counted** (`VerifyReadSkips`), never
  served (`Options.DisableReadVerification` turns this off).

**A delete writes no record.** It only stamps `supersededBySeq` on the current version,
an MVCC tombstone. Scans already in progress still see the pre-delete version; new
scans do not. This makes delete cheap, but the *evidence* of a delete is a stamp on an
old record, which compaction eventually reclaims. The durable path (Chapter 8) and the
Watch path (Chapter 16) must handle that. In an append-only collection `Delete` does
nothing; data leaves only by whole-segment rotation.

## 6. Durability and group commit

Strict durability means a `Put` does not return until its bytes are safely persisted:
for the persistent variant, `msync`'d to the mapped file. Done naively, that is one
`fsync`/`msync` per write, and throughput collapses to the disk's sync rate.

**Group commit** (`commit.go`) amortizes it in two layers.

*Applying writes.* Concurrent writers to a shard coalesce into one batch that commits
under a single sequence bump. The protocol is a leader/handoff:

- An uncontended writer becomes the flusher and applies its write directly. This fast
  path uses no channels and no allocation.
- Contended writers enqueue and block on a per-request signal. One of them is appointed
  leader, commits the whole queued batch at one fresh `seq`, and signals the rest. Each
  leader does at most one batch and then hands off, which bounds latency.

*Syncing.* The `msync` itself is coalesced separately (`syncFor`): committers that
arrive while a sync pass is running wait for the *next* pass rather than starting their
own, so N concurrent committers pay about two passes.

`Put`, `Update` and `UpdateOld` go through both layers. The other write paths share
only the sync layer:

- `Delete` applies under the shard lock, then syncs the tombstone page. It must sync,
  because recovery cannot re-derive a delete (Chapter 8).
- A transaction commit (Chapter 15) applies each shard's writes, then syncs all touched
  shards in parallel. `Txn.SetDurable(false)` defers that sync for callers that batch
  durability themselves.

The payoff is real. With a simulated 200 µs sync, ads per sync rise from 1 to 12.5 as
writers scale from 1 to 24 (≈12.6× throughput). With a real `msync` on the persistent
store, ads per sync rise from 1 to 7.9 at 16 writers. Benchmarking also showed that
write throughput was **not** limited by lock contention, which scaled with cores. It
was limited by per-call allocation, which is why `Put` encodes and commits directly
rather than going through the batch machinery of `Update`.

**Failure model.** A sync that fails with an I/O error is **sticky**: the first
EIO-class failure is latched, and every later write fails with it. That is fail-stop:
a commit whose pages may not have reached disk must never be reported as durable, and
a retried `msync` succeeding says nothing about the pages the failed one covered. Only
`Truncate` clears the latch. On Linux,
segment files are reserved with `fallocate` when created. A full disk therefore fails
cleanly at allocation, as a sticky write error, instead of surfacing later as a SIGBUS
when the kernel tries to write back a page of a sparse mapping.

## 7. Compaction and segment rewrites

Segments are append-only, and updates and deletes leave superseded records behind, so a
shard accumulates dead bytes. **Compaction** reclaims them. It is per-shard, driven by
each shard's garbage ratio, and caller-scheduled: the store runs no timer of its own.

The hard part is doing it without stopping the world or breaking an in-flight scan. The
compactor runs in three phases, and only the first and last hold the shard lock:

1. **Snapshot (locked, brief).** Record the set of source segments and *seal* the
   active segment, so concurrent writes go to fresh segments.
2. **Recompress (lock-free).** Copy each key's current record into new private
   destination segments. This is the expensive work, done without the lock so writers
   proceed. Destinations are interned against a fresh per-segment dictionary
   (Chapter 3). The sources are pinned for the duration, because a source may be
   reaped underneath an unpinned reader.
3. **Commit (locked, brief).** Transfer any supersession stamps that landed mid-copy
   onto the destination copies, so a record superseded during the copy is not
   resurrected. Then rebuild the directory with fresh chains and swap the new segments
   in.

Several variations ride on the same machinery:

- **Dead-segment fast path.** A segment in which every record is superseded is simply
  unlinked, with no copy.
- **Time-travel retain floor.** With time travel on, a superseded version newer than the
  retain floor is kept, copied into a separate history stream rather than dropped
  (Chapter 15).
- **Delta collapse.** Before a rewrite, any key whose current version is a delta is
  collapsed to a full record, so a rewrite never strands a delta without its base.
- **De-columnarization.** Compaction writes reassembled whole records. A later
  columnarization pass rewrites them into columns again (Chapter 9).
- **Interruptible.** `StopMaintenance` asks compaction and the other rewrite passes to
  stop at the next segment boundary, for shutdown.

### The rewrite family

Compaction is one of several operations that replace sealed segments with a new file:

| Operation | Applies to | Purpose |
|---|---|---|
| compaction | mutable | reclaim superseded records |
| columnarize / re-columnarize | persistent | store schema'd values in columns (Chapter 9) |
| merge | append-only | keep segment count under `vm.max_map_count` (Chapter 18) |
| codec upgrade / rewrite | append-only | re-encode under a newer dictionary (Chapter 4) |
| intern | persistent | re-encode with a per-segment dictionary (Chapter 3) |

The replace-N-with-1 operations share one crash-safe commit, `commitSegmentRewrite`:

1. stage the output under a `tmp-merge-*` name, which recovery ignores;
2. fsync it;
3. write and fsync a `.merging` intent marker naming every source and the target;
4. rename the target into place;
5. unlink the sources;
6. remove the marker.

A crash before step 3 leaves an ignored temp file. A crash after it leaves the marker,
and recovery finishes the remaining steps, each of which is idempotent. The target is
durable and named before any source is removed, so no window loses records.

### Reclaiming old segments

Reclaiming the *old* segments is a separate concern, with two backends:

- **Heap-backed segments** (in-memory collections) are simply dropped. The Go garbage
  collector frees them once no scan still holds them.
- **Memory-mapped segments** need explicit management, because the OS will not reclaim
  a mapping on its own. This covers both file-backed segments and in-memory segments
  whose sealed index has moved to an anonymous mmap sidecar. The segment backend adds
  pin/reap tracking:
  - A scan `pin()`s each mmap segment it reads.
  - Compaction, merge or rotation `retire()`s a segment from the live set but defers
    `munmap` and `unlink` until the pin count drains (`reap()`).
  - The same pins also guard compaction's lock-free copy phase and the `msync` pass.

This machinery governs *file lifecycle only*; MVCC visibility stays the `seq` rule for
both backends. A sealed segment is not alive merely because it is sealed. Code that
reads a segment's bytes outside the shard lock must hold a pin, or the mapping can be
reaped underneath it.

## 8. Persistence, recovery, and the on-disk layout

The persistent `Collection` is the same store with file-backed arenas. It is opened
with `Open(Options{Dir: …})`; an empty `Dir` gives the in-memory `New`. `Close` flushes
and unmaps. It is Unix-first (via `golang.org/x/sys/unix`).

It no longer assumes the working set fits in RAM. Values live in demand-paged
mappings, sealed indexes and the sealed part of the key directory live in pageable
sidecars, and each segment's file descriptor is closed as soon as it is mapped.
Resident memory is roughly the active segments, their indexes and directory entries,
the block cache, and whatever pages queries touch.

### Files

A persistent collection's directory holds one subdirectory per shard plus a handful of
store-wide files (Appendix C has the full inventory):

```
<Dir>/
  0/ … N-1/                         one directory per shard
     seg-<n>.d<dictID>.dat          a segment: records, no file header
     seg-<n>.d<dictID>.dat.idx      its sidecar container (indexes, keys, zones, extent)
     dir.snap                       key-directory snapshot from the last clean Close
  dicts/<id>.zst, dicts/attic/      trained ZSTD dictionaries; retired ones
  watch.epoch                       Watch identity (append-only collections)
  deltamode, demand.json, groupschemas.json, upgrade.json, sealed-attrs.done
```

**Segment files have no header.** Records start at offset 0. The only per-segment
metadata outside the records is the dictionary id in the file name. A segment file is
`fallocate`'d to its size on Linux (`ftruncate`'d elsewhere), mapped `MAP_SHARED`, and
written through the mapping. Group commit `msync`s the newly written pages once per
coalesced batch. On reopen, segments are ordered by their first record's `seq`, not by
file number, because a merged or rewritten segment takes over the slot of its oldest
source.

### Recovery

`Open` rebuilds each shard in this order:

1. **Enumerate and map** the segment files, and load every referenced dictionary.
2. **Find each segment's extent.** For a sealed segment it is read from its sidecar
   trailer. Otherwise it is found by walking records until the first bad CRC: a torn
   tail from a crash mid-write is ignored, and only the committed prefix recovers.
3. **Publish in-segment structures.** These are the attribute dictionary record
   (Chapter 3) and the columnar payload (Chapter 9). The payload is CRC-checked here,
   because step 2 skipped walking a sealed segment.
4. **Restore the directory**, by one of two routes:
   - **Fast path.** If the last shutdown was clean, `dir.snap` holds the directory, the
     commit sequence, and the live count. It is validated against the exact set of
     segment files and their lengths, plus a CRC, then consumed: it is deleted as it is
     read, so it is trusted for exactly one reopen. Any mismatch falls back to the slow
     path. Time-travel and chained collections always use the slow path.
   - **Slow path.** Replay records by `seq`: **the current version of a key is its
     record with the maximum `seq`**. Append-only collections skip the dedup below.
5. **Evict sealed keys** to the key indexes (Chapter 5).
6. **Map the sidecars.** Rebuild only the ones that are missing, fail validation, or
   lack coverage for the current index configuration, within the
   `IndexBackfillBytes` budget (Chapter 12).
7. **Rebuild derived in-memory state.** That means the ordered index (Chapter 20) and
   the time-travel checkpoint map (Chapter 15). Also check for delta records stranded
   in sealed segments, and collapse them.

Max-seq replay is what lets updates stay fast. An update's supersession of the *old*
record is an atomic write to a page that may not be synced yet. Recovery does not need
that stamp, because max-seq dedup yields the new value regardless. Two cases need care
and get it:

- **A single-current-version invariant.** After replay, any record still marked current
  that is not its key's max-seq winner is marked superseded. This covers two crash
  shapes. One is a crash after compaction, with source and destination files both on
  disk. The other is a non-durable supersession stamp that landed in a page that was
  already synced.
- **Deletes must sync.** A delete writes no new record, so the max-seq rule cannot
  re-derive it. The tombstoned record's page must reach disk, so `Delete` flushes it.
  Deletes are comparatively rare, so this does not slow the update path.

Replay costs about 19–27 µs per ad when it runs (50k ads in roughly a second, measured
before key eviction and sealed extents). After a clean shutdown it does not run at all.
`OpenIndexDiag` reports, per segment, whether each sidecar was adopted or rebuilt and
why.

### The sidecar container

Each sealed segment has **one** sidecar, `<segfile>.idx`, holding everything derived
from it. Sections are concatenated, and a fixed trailer at the end of the file records
their lengths:

```
[ attribute index (ARCX) ][ key index (KIDX) ][ columnar (COLX) ][ zones (ZNM3) ]
[ attrLen u32 | keyLen u32 | colLen u32 | zoneLen u32 | segUsed u64 | dictRec u32 | "SCN4" ]
```

- The **attribute index** is written first, so its roaring bitmaps stay aligned at
  offset 0. Its format (ARCX, currently version 9) is described in Chapter 12. It
  carries its own CRC-32C footer.
- The **key index** (KIDX v1) is a sorted array of `(keyhash u64, offset u32)`, one entry
  per record in the segment, behind a small header and an IEEE CRC. It is probed by
  binary search over the mapping.
- The **columnar section** (COLX v7) is a *derived* copy of the segment's columnar
  blocks. It is used only before the segment is columnarized in place, and by in-memory
  collections (Chapter 9).
- The **zone section** (`ZNM3`) holds per-attribute numeric ranges, keyed by attribute
  *name* (Chapter 18).
- **`segUsed`** is the sealed extent, which spares recovery the CRC walk. **`dictRec`**
  is the offset of the segment's dictionary record.

The trailer magic distinguishes four container versions, each a superset of the last:
`SCNT` (attr + key), `SCN2` (+ columnar), `SCN3` (+ zones and extent), and `SCN4`
(+ dictionary offset).

The sidecar is a **validated, disposable cache**. It is written once to a temp file and
renamed into place. On open it is checked against its CRCs, the segment's extent, and
the index configuration it covers. On any doubt — a mismatch, an unknown version, a read
error — it is ignored and rebuilt from the segment, so it can make startup faster but
can never yield a wrong index. It shares the segment's file name, so compaction,
merging and rotation drop it with the segment.

### Integrity tools

Read verification (Chapter 5) catches corruption in normal operation. Two offline tools
handle a store that may be damaged:

- **`Fsck(dir)`** is read-only. It walks every segment, resynchronizes past bad CRCs to
  count what survives, checks sidecars and the columnar payload, and names files whose
  names do not parse.
- **`Repair(dir, opts)`** moves damaged originals to `<Dir>/quarantine/` and drops
  stale `.idx` and `dir.snap` files, so the next open rebuilds them. It never deletes
  the only copy of data.


## 9. Columnar segments

Per-record compression has a ceiling. However good the dictionary, ad *n* is compressed
without seeing ad *n+1*, and on ads that share most of their attributes and many of
their values with their neighbours that is most of the redundancy left. Measured on
OSPool machine ads, both forms dictionary-compressed:

```
row form, per record      3445 B/rec
columnar, same records     673 B/rec      (0.195×)
```

A column also answers the questions an analytical query asks — how many, what range,
grouped by what — without reconstructing an ad at all. So sealed persistent segments are
rewritten into a **PAX layout**: row groups whose common attributes are stored
column by column, with each record keeping only what the columns do not hold. The
layout is implemented across `adschema.go`, `colblock.go`, `colnative*.go`,
`colgroup*.go`, `colpersist.go`, and `strdict.go`.

### The schema

ClassAds are schemaless, but a segment of like ads is nearly a schema: every slot ad has
`Memory:int`, `Cpus:int`, `OpSys:string`…. A schema is recovered by **sampling**:

- An attribute joins the schema when one kind (bool, int, real, or string) accounts for
  its value in at least 90% of sampled ads.
- An integer field gets the smallest width — 1, 2, 4, 6 or 8 bytes, signed or unsigned —
  that covers 95% of sampled values. Fields in the *hot set* use 99.9% instead, which
  caps an outlier's blast radius at 0.1% of records.
- The hot set is the top-N numeric fields by observed query demand. Hot fields stay
  uncompressed so a scan reads them with no decode.

A value that does not fit its slot — the wrong kind, an integer too wide, or an
expression rather than a literal — **escapes**. Escaped values are carried elsewhere in
the record, as described below.

The schema is fixed when columnar storage is first enabled on a collection. Routine
refreshes only extend it to new segments. `ReschemaScan` is the heavy operation that
rebuilds it. Each columnarized segment carries its own schema, and readers resolve the
schema per block.

### Group schemas

The base schema holds attributes present in nearly every ad. That threshold makes a slot
cheap, since it costs fixed bytes in every record, but it also makes coverage hostage to
the mix of ads.

Measured on a real access point, a history table of completed jobs has 154 base
attributes covering 90% of attribute occurrences. Mix in jobs that were removed before
they ever ran, and the base shrinks to 80 attributes and 51% coverage, because the
attributes a job acquires by running fall below the threshold together.

**Group schemas** (`colgroupschema.go`) fix this. A group is a set of attributes that
**co-occur**: in any ad they are all present or all absent. Such a set can be stored
columnar for the ads that have it, without costing a slot in the ads that do not. Groups
are derived by exact co-occurrence in the sample — a deterministic pass, with no
clustering, seeds or distance metric. That makes the expensive *partial* case (an ad
holding some but not all of a group) impossible within the sample. Partial ads still
arise from drift, and must fall back to row storage for those attributes. Policy:

- Up to 4 groups by default.
- A group must recur across 3 sampling runs before it is adopted.
- Near-identical groups merge at Jaccard ≥ 0.99.
- A group is rejected if more than 0.1% of ads would be partial.

A group block is a **selection** of one base block: a membership bitmap with a rank
table, plus an exception list. A record's index is therefore the same in both, and the
vectorized executor can scatter a group column into a full-length vector with
non-members undefined.

### Physical layout of a columnarized segment

A columnarized sealed segment holds three kinds of records:

1. **Remnants.** Each original record keeps its header, key, `seq`, supersession and
   chain fields, but its ad bytes now hold only what the columns cannot:
   - expressions;
   - lists and nested ads;
   - encrypted values;
   - attributes outside every schema;
   - escapes, i.e. schema values that did not fit their slot.

   A remnant has no hot header, is flagged `colFlag` without `markerFlag`, and is
   compressed per record with the segment codec.
2. **The dictionary record**, as in any interned segment (Chapter 3).
3. **The columnar payload**: one keyless `markerFlag|colFlag` record appended last.

The payload **is authoritative**. It is the only copy of most values in the segment,
not an index over them, and losing it is data loss rather than a rebuild. That is why
it lives *inside* the segment file rather than beside it: it shares the segment's
framing, its record CRC, its fsync, and its lifetime, so no segment can be opened and
found to be missing its columns. The payload begins
with an 8-byte header: the magic `COLN` and a u32 format version (currently 1). A
payload without the magic is a legacy, version-0 payload and reads unchanged. A legacy
payload begins with its schema's field count, whose fourth byte is always zero; the
magic's fourth byte is not. A payload in a version this build cannot read is **not**
treated as damage, because the payload is the only copy of its values:
- `Open` fails with `ErrUnsupportedColumnarVersion`, naming the segment.
- `Fsck` reports it.
- `Repair` leaves it untouched.

Refusing loudly is the only safe answer to a layout this build does not understand.

The payload holds, in order:

- the base schema, as name-keyed `(name, kind, width, signedness)` fields, plus the hot
  field indices;
- the cold-tail key names, unless the segment has its own dictionary (an unscoped
  payload cannot use process-local intern ids, which are renumbered at every open);
- the **row-group blocks**;
- the segment-wide **record-offset map**: one `u32` arena offset per record, ascending,
  binary-searched to go from a remnant to its row;
- the group schemas and each base block's group selections.

### Row groups

A segment is split into row groups so that touching one record does not mean
decompressing the whole segment. A group is sealed once its records reach **128 KiB of
uncompressed row bytes**, or 8192 rows, whichever comes first, aligned to a multiple of
8 records. `RowGroupBytes` tunes this. The bound is in bytes because what a block costs
to decompress, and whether it fits the cache, are properties of its size. A row count
was a byte budget in disguise that did not transfer across a 300× range in ad width.

Measured on 561-attribute slot ads, point-reading one ad costs 17× a row read at 128
rows per group and 319× at 1500, while full-scan cost is already saturated at 128.

Each block (`columnarBlock`) holds:

| Region | Form |
|---|---|
| escape bitmap + bool bitset | uncompressed, one fixed-stride row per record |
| hot numeric columns | uncompressed, **columnar** (each field contiguous, so SIMD can load it) |
| cold numeric columns | columnar, compressed |
| string region | per-record `uvarint(len)+bytes` for non-escaped string fields, compressed, with an `n+1` offset array |
| cold tail | per-record entries for escaped and non-schema literals, compressed, with an `n+1` offset array |
| string dictionaries | per field where values repeat: fold-sorted distinct values plus 1- or 2-byte code columns, compressed. **Authoritative**: a dictionary-coded string is not also written positionally |
| zones | per numeric field `[min, max]` plus an `escaped` flag |
| escape classes | per field: none, all-missing, all-exceptional, or mixed (with the exceptional record list); plus an all-absent bitmap |

Regions are compressed with a dictionary-less **region codec**, separate from the
segment's trained codec. Decompressed regions live in a process-global, size-bounded
block cache (ristretto, 512 MiB for mutable collections plus 512 MiB for archives by
default). This is why the block budget matters: a block too large for the cache would
be re-decompressed on every read.

The `escaped` zone flag is a correctness flag, not a hint. Min/max cover only values
stored in the column. If any record in the block has the field escaped — missing, the
wrong type, too wide, or an expression — the range cannot rule the block out, and
pruning on it would drop matching records.

**Held cells.** An expression-valued schema field is not in its column; it stays in the
remnant. To the block it looks the same as a missing value. Readers that need to tell
the two apart — any exact answer, such as a count or a column-decided match — consult a
lazily built per-segment index of which cells each remnant still holds (`colheld.go`).
Treating a held cell as UNDEFINED is precisely the class of bug Chapter 23 warns about.

### Two homes for columnar blocks

Columnar blocks live in one of two places:

- **Sidecar copy (derived).** Before a segment is columnarized in place, and always for
  in-memory collections, columnar blocks can be built *beside* the segment, in the
  sidecar's COLX section. This copy is an accelerator over whole records: versioned,
  checksummed, and rebuilt on any mismatch.
- **In-segment payload (authoritative).** Once a segment is columnarized, the sidecar
  omits COLX, and the payload inside the segment is the only copy.

### Lifecycle

Columnarization is **not** done at seal; the active segment is never columnar.

- **How it runs.** A maintenance pass (`BuildAndEnableSchemaScan`) installs the schema,
  then rewrites eligible sealed segments (`ColumnarizeSealed`) through the crash-safe
  `commitSegmentRewrite` (Chapter 7). It then builds sidecar blocks for whatever remains
  row-form.
- **Budget.** The rewrite is bounded at 64 segments per pass by default
  (`ColumnarSegmentBudget`; a negative value disables it).
- **Scope.**
  - Persistent collections only, including archives.
  - Encrypted values stay sealed in the remnant.
  - Delta records are materialized or dropped as they are superseded.
- **Who turns it on.** The `db` layer's maintenance enables the pass when its
  `SchemaScanHotTopN` options are set. htcondordb sets them for both tables and
  archives.
- **Undoing it.** Compaction and archive merging write whole, reassembled records,
  de-columnarizing their output. The next pass columnarizes it again. A change in the
  group set re-columnarizes stale segments the same way.

### Reassembly

Any reader that needs a whole ad from a columnarized record calls `recordWireIn`. It
splices the remnant's entries together with:

- the schema fields from the block,
- the block's cold-tail entries,
- any group values.

It translates segment-local ids to process ids through a precomputed map. Every generic
walk goes through this one function: serial, reverse, chained, parallel, Watch, and
point reads. The reassembled ad is valid wire form, but its attribute order is not the
original, and it has **no hot header**. Reassembly costs about 1.2× a whole-record
read, which is why the query engine tries hard not to do it (Chapter 11).

### Limits and known costs

- **Chaining.** Chained (parent/child) collections never use the column paths.
- **Remnants** are still compressed one record at a time. Grouping a few dozen per
  region measured 3–5× smaller; it is not yet implemented.
- **Schema freeze.** A schema fixed at first enable never adapts to drift short of
  `ReschemaScan`.
- **Error visibility.** A record whose payload cannot be read is skipped by scans and
  counted (`ColNativeCRCFailures`). The query does not fail. Damage is visible to
  operators but not to the query that missed it.

## 10. Encryption at rest

Some ClassAd attributes are secrets: claim ids, capabilities, transfer keys, and
anything `_condor_priv*`. They must not sit on disk in the clear, whatever the
deployment configured. The engine seals them at the *value* level (`encrypt.go`,
`crypt/`).

- **What is sealed.** An attribute is sealed when it is *private* or in an
  operator-toggled set (`EncryptedAttrs`, changeable at runtime). Private is a fixed,
  name-only property (`classad.IsPrivateAttribute`), memoized per name.
- **How.** The value's ordinary node encoding is AES-256-GCM-encrypted, and the result
  is stored as an `nEncrypted` node (tag `0x17`) carrying the nonce and ciphertext.
  Names, keys, dictionaries and every unsealed attribute stay in the clear, so the rest
  of the ad still indexes, matches and columnarizes normally.
- **One data key per database.** All values are sealed under a single data key. Because
  that key is shared across segments, ciphertext is portable: compaction, merging and
  columnarization move sealed values without re-encrypting them.
  - The data key is an HKDF-derived subkey of a per-database **master key**. It is
    distinct from the master, which never seals data itself.
  - The master is either wrapped once under each available pool key, in
    `masterkeys.json` (any one pool key opens the database, and a rotated-in key is
    added without re-encryption), or, with no pool keys, stored unwrapped in
    `masterkey.unprotected`.
  - In the unwrapped case values are still sealed, but that is not encryption at rest,
    and `EncryptionEnabled` reports false.
- **What it costs.** Sealed attributes are opaque to every fast path. They are never
  indexed, never hot, never in a column, and never zone-mapped. The `*Redacted` read
  variants (`QueryRedacted`, `BeginRedacted`, `WatchRedacted`, `OrderedRedacted`)
  decode with no key, so for a caller not entitled to secrets a sealed attribute reads
  as UNDEFINED. The secret is never opened only to be dropped later by a serializer.
- **Migration.** `MigrateSealedAttrs` re-encodes legacy stores written before sealing
  was mandatory, and writes `sealed-attrs.done` when finished.
- **Gap.** Archives opened through the `db` catalog are currently opened without a data
  key, so a history archive stores private attributes **in the clear**.
  `Archive.EncryptionEnabled` reports this rather than hiding it.

---

# Part III — Querying

## 11. The compiled query engine

A query is a ClassAd boolean expression — `Owner == "alice" && JobStatus == 1 &&
RequestMemory <= 4096`. Evaluating it against millions of ads is the hot path, and the
engine's central performance idea is: **do not build a ClassAd to reject one.**

The query is compiled once (`collections/vm`) from its AST into flat bytecode
(`[]Instr`). The interpreter runs native operations for the hot core: comparisons,
boolean logic, arithmetic, and attribute references. It delegates the actual value
semantics (three-valued logic, coercions) to the same exported hooks the tree-walking
`classad` evaluator uses, so results are identical by construction. Anything outside the
native core — a ternary, a list, a function call, a `select` or subscript — compiles to
a single `OpEvalNode` escape hatch that runs the full evaluator on that subtree.

### The tiers of a row scan

For each visible record, a scan uses the cheapest path that is safe (`refVisitor` in
`store.go`):

1. **Column decision.** In a columnarized segment, the *column prefilter*
   (`colprefilter.go`) evaluates the query from the record's columns first. It runs only
   when every attribute the query reads is a base-schema field. There are three outcomes:
   - A **decided non-match** is skipped without reassembling anything.
   - A **decided match** with a projection is built straight from the columns
     (`colproject.go`), never touching the remnant.
   - Anything the columns cannot settle — a held expression, an escaped value, a field
     outside the schema — is **undecided**, and falls through.

   The prefilter is a filter on the existing scan, not a second scan. It inherits
   visibility, ordering, limits and every fallback, so its only possible effect is not
   doing work. Being unsure is allowed; being wrong is not.
2. **Wire-native.** If the query is *native* (no `OpEvalNode`), it evaluates directly
   against the encoded bytes. A pluggable resolver reads scalar-literal attributes
   straight out of the wire (`wire.LiteralValue`), building no ClassAd. If a referenced
   attribute turns out to be a non-literal expression, the resolver signals `fellBack`,
   and the engine drops to the next tier for that ad. For a columnarized record, this and
   the next tiers run on the reassembled bytes.
3. **Partial decode.** Decode only the attributes the query reads — the transitive
   closure of referenced names, including `MY`-scope self-references — and evaluate
   against that. Everything else in the ad stays encoded.
4. **Full decode.** For queries that read attributes by a runtime-computed name
   (`eval()`), decode the whole ad.

The tiers have different soundness contracts (tenet 3):

- Tiers 2–4 are the evaluator itself, applied to more or less of the ad.
- Tier 1 is an **exact** fast path. It may decide only where its answer is provably the
  evaluator's, and must decline otherwise. The prefilter decides only on literal values
  in their slots; an expression-valued cell is "held" (Chapter 9) and undecided.

Only records that *match* are reassembled or fully decoded to be handed to the caller.
On selective queries over row-form records the effect is dramatic:

- A real-ad selective query drops from ~167 µs/ad (full decode of every ad) to
  ~3.5 µs/ad wire-native, about 48×.
- Allocations drop from ~10 per ad to ~2, with ~44× less scan memory.
- Partial decode alone is ~15×, and the hot header adds roughly another 2× on that path.

On columnarized segments, the prefilter took a 2%-selective scan from 35.9 ms to 6.3 ms.

### The vectorized executor

The scalar interpreter dispatches once per instruction per *record*. The vectorized
executor (`vm/vecexec.go`, driven by `colvec.go`) runs the **same compiled `Program`**
once per instruction per *batch*:

- An attribute reference becomes a column load.
- A binary operator becomes a kernel over two vectors.
- A logical combine becomes a three-valued elementwise merge.

It uses no second IR, so an expression is lowered in exactly one place, and the scalar
interpreter serves as the reference to test it against.

**Parity** is kept the same way the scalar interpreter keeps it. Each kernel has a fast
loop for the one dominant, unambiguous state — number op number, bool and bool — and
routes every other element through the *same* `ApplyBinaryOp` hook. Undefined, error,
mixed types and division by zero are therefore computed by the reference
implementation, not reproduced.

**Short-circuit.** Vectors cannot short-circuit per element. Both sides of `&&`/`||`
are evaluated, and the combine applies ClassAd's left-biased rules elementwise. That is
safe because evaluating an operand has no side effects; skipping it is only ever an
optimization.

**Strings** are compared through dictionary codes where a block has a string
dictionary.

**Where it declines.** The executor declines per *block*, not per query. A block with
little live data — less than a quarter of its records visible — goes back to per-record
column resolution, and a window with no block takes the row path.

**SIMD.** Optional SIMD compare kernels build only under `GOEXPERIMENT=simd`; the
default build uses the scalar width-native kernels.

Today the vectorized executor serves counts and grouped statistics (Chapter 14), not
row-returning queries. A vectorized count measured 5.84 ms → 0.76 ms.

### Read surfaces

Not every read wants ClassAd objects. The collection offers:

| Method | Returns | Builds a ClassAd? |
|---|---|---|
| `Query`, `Scan`, `QueryLimit` | `*classad.ClassAd` per match | for matches only |
| `QueryProject` | a `[]classad.Value` per match, for named attributes | no; from columns when decided there |
| `QueryRawProjected` / `ScanRawProjected` | wire bytes containing only the projection (optionally following references; optionally redacting private attributes) | no |
| `QueryRawWire` | each matching record's wire bytes (a projected subset) | no |
| `QueryRawFromSeq` | raw ads after a per-shard sequence cursor, plus the next cursor | no |
| `KeysWhere` | matching keys only | no |
| `QueryAsOf` | as `Query`, at a past instant (Chapter 15) | for matches only |

`QueryRawFromSeq` is the pagination primitive. A sequence cursor resumes exactly where
the last page stopped, even while the store is changing underneath, which a key- or
offset-based page cannot do.

Every tier, and every read surface, is **differentially tested to equal a full decode
plus tree-walk evaluation**. The bytecode interpreter is fuzzed against the tree-walker
over tens of millions of executions, and the vectorized executor against the scalar
interpreter on the same programs.

## 12. Indexing

A selective query should visit few ads, not all of them. The engine indexes attributes
two ways (`index.go`, `index_query.go`):

- **Value indexes** on numeric attributes, for equality and range (`Memory >= 4096`).
- **Categorical indexes** on string and boolean attributes, for equality and set
  membership (`Arch == "X86_64"`, `OpSys == "LINUX" || OpSys == "WINDOWS"`).

Indexes are **per segment**, stored as [roaring bitmaps](https://roaringbitmap.org/) of
record offsets. They are never maintained on the write path, so writes never block on
index maintenance. A segment's *tail*, past the point its index covers, is simply
full-scanned. When indexes are built depends on the store:

- **Mutable collections** build attribute indexes in `Reindex`. It runs at `Open`, after
  every compaction, and whenever maintenance calls it.
- **Append-only collections** index each segment **eagerly when it seals**. `Append`
  notices the seal and builds that segment's index off the shard lock. The active
  segment is never indexed: it is all tail.
- **Every persistent segment** gets its **key index** at seal, from the commit hook,
  so its keys can leave the directory (Chapter 5).

### Planning

The query planner is the reusable heart of the index path, and it embodies tenet 3's
*superset* form:

- **Probes.** `vm.Query.ProbePlan()` extracts the index-satisfiable structure of a
  query:
  - It constant-folds, then expands the expression into a disjunction of conjunctions
    (DNF, capped at 16 groups).
  - It classifies each conjunct as `attr OP literal`. The operators are comparisons,
    `in`, `is`/`isnt`, and present/absent.
  - Anything it does not recognize is omitted, which only widens the result.
- **Plans.** Within a group, probes on the same attribute are folded: ranges coalesce,
  and equalities intersect. A group's candidates are the intersection of its probes'
  bitmaps; the plan's candidates are the union of its groups'.
- **Cost bound.** A probe whose bitmap would cost more to materialize than the
  verifications it could save is skipped, which measured 14 ms → 0.76 ms.
  - In append-only collections, the whole plan is refused when it would hand over more
    than 95% of the records; a scan is cheaper.
- **Superset + re-verify.** The scan visits only candidates, and **the full query is
  re-verified against every candidate.** Candidates in columnarized segments pass
  through the column prefilter first (an indexed archive fetch went from 2.9 s to
  14 ms). A probe the planner drops, or an index covering only part of a segment, only
  costs selectivity — never a wrong answer.

`ExplainQuery` reports the plan: groups, probes, estimates, and which were skipped and
why. `ScanStats`, filled during a scan, counts records decided from columns versus
reassembled.

### Per-segment statistics

Each segment's index carries a compact, postings-free summary per indexed attribute
(`segstats.go`):

- the numeric **min/max**;
- a categorical **Bloom filter** over the value set;
- an exact distinct count plus a mergeable **HyperLogLog** sketch;
- the top-N heavy hitters;
- an **equi-depth histogram** of at most 64 buckets.

The summary earns its keep twice:

1. **Segment skip.** Before enumerating a segment's candidates, the planner checks each
   required equality or range conjunct. If the summary proves it unsatisfiable — a value
   outside `[min,max]`, or every probe value a Bloom miss — the whole indexed prefix is
   skipped. This holds only *provided* no exceptional (present but wrong-type or
   non-literal) record could still match; those are always re-verified.
2. **Probe ordering.** The planner estimates each probe's cardinality, using the
   histogram for ranges and the top-N and HLL for equality, and intersects the most
   selective first.

The sketches are bounded (≤8 KiB Bloom and 1 KiB HLL per attribute per segment). A
Bloom hit may be a false positive, which only forgoes a skip, so soundness is preserved
by construction.

### The sidecar index format (ARCX)

A sealed segment's attribute index lives in the ARCX section of its sidecar
(Chapter 8), queried **in place over the mmap**. Nothing is deserialized onto the heap.
For each indexed attribute, the section stores postings as a **sorted key run**: sorted
values, a parallel array of bitmap offsets, then the bitmaps. Lookups work directly on
it:

- Equality is a binary search.
- A range is a boundary scan over the key run.
- Roaring bitmaps are materialized lazily, via zero-copy `FromBuffer`, for only the
  postings a probe touches.

Resident memory is therefore proportional to the number of indexed attributes, not
their cardinality. The format has grown by versioned additions:

| Version | Adds |
|---|---|
| v5 | a categorical Bloom filter, so a value proven absent skips the key-run search entirely |
| v6 | a **minimal perfect hash** (BBHash) for categorical attributes with NDV ≥ 4096. A present-value lookup is a bit-test plus a popcount-rank, verified against the key, then a read of the bitmap offset — ~1 page instead of an O(log n) scattered search |
| v7 | the per-attribute statistics block (min/max, top-N, NDV, HLL) |
| v8 | a whole-section CRC-32C footer |
| v9 | the equi-depth histogram |

The MPH and Bloom are pure fast paths. A non-member is caught by the key verify, and
any miss falls back to the still-present sorted run, which stays authoritative. An
older version is rejected, and the segment re-indexed; there is no migration code.

The same format serves **every tier**:
- A persistent collection maps the sidecar file.
- An in-memory collection writes the same bytes into an **anonymous** mapping. This
  takes sealed-segment indexes off the Go heap, out of GC mark cost and GOGC pacing. It
  happens only if indexes were configured at construction.

Only the active segment's index (`segIndex`) lives on the heap.

### Index configuration over time

An index set is not fixed. `AddIndex` and `DropIndex` change it at runtime, and the
auto-tuner changes it from observed demand. Each sidecar records a **content signature**
of the index configuration it was built under, and coverage is decided **per probe**
(`coversProbe`). A sidecar built before an attribute was added still serves probes on
the attributes it does cover, and is not thrown away.

`reindexSealed` rebuilds a segment's sidecar and swaps the mapping without touching the
segment's data. `IndexBackfillBytes` bounds how far back a configuration change is
carried. With a 1 GiB horizon, only the newest 1 GiB of an archive is re-indexed for a
new attribute; older segments are scanned for it. `StaleIndexSegmentsByPolicy` reports
the backlog.

### Demand and auto-tuning

A demand tracker records which attributes queries and match probes filter on.
- Counts decay exponentially, with a 7-day half-life by default (`DemandHalfLife`), and
  are checkpointed to `demand.json`, so a restart does not forget a week of evidence.
- `SuggestIndexes` recommends additions.
- `SuggestDrops` recommends removals, guarded by drop-evidence and anti-thrash rules.
- `AutoTune` applies both against a memory watermark, recording which indexes it added
  (auto) versus which an operator added (manual). It never drops a manual index, and on
  an archive it never drops at all.

`IndexSizes` measures the heap-resident posting and sketch bytes of the active segments;
the budget is calibrated on those. `SidecarSizes` reports the sealed segments' mapped,
evictable sidecar bytes, with the MPH and Bloom portions broken out. The two are never
folded together.

## 13. Parallel query

A large full-scan query can fan out across segments (`parallel_scan.go`). Again, the
MVCC rule makes this safe and cheap: parallelism falls out of invariants already present
rather than needing new machinery.

1. **Segments are immutable once written, so reads are lock-free.** Workers share no
   mutable state.
2. **Exactly-once needs no coordination.** The version of a key visible at `S0` lives in
   *exactly one* segment, so workers can take disjoint segments and simply concatenate
   their outputs. There is no dedup and no shared "seen" set.
3. **Results are order-insensitive**, so there is no merge.

The unit of work is one segment window. Workers claim the next one with an atomic
counter; at segment granularity a task is milliseconds of scan, so the atomic is free.
Each worker does the whole per-record path — reassemble or decompress, match, and decode
matches — so the expensive half is parallelized; only the final `yield` is serial.
Matches flow back over one bounded, sub-batched channel. An early stop sets an atomic
flag and drains cleanly, so pins are always released and a mapping is never used after
it is unmapped.

Fan-out is **gated**:
- It engages only when the scan is large enough to amortize the setup and spans at least
  two segments.
- Automatic mode uses up to 6 workers, capped at `GOMAXPROCS`.
- Workers come from a **per-collection** budget (a semaphore sized to `GOMAXPROCS`),
  taken with a non-blocking greedy try.

The reasoning is that intra-query parallelism helps *latency* at low concurrency, but not
*throughput* when many queries already saturate the cores. Under load, queries decline to
fan out and fall back to serial rather than oversubscribing. The budget is per
collection, so a process with many busy tables can still oversubscribe. Measured on one
collection: a 40k-ad decode-heavy scan is ~8.2× faster at 8 workers, and under
`GOMAXPROCS` concurrent queries the budget keeps oversubscription to ~10–15% rather than
a collapse.

**What fans out.**
- Full scans, match scans, and raw-wire subset scans (`QueryRawWire`) fan out.
- Index-planned, newest-first (and therefore every archive query), chained, and
  point-in-time scans stay serial.
- Parallel workers do not use the column prefilter, whose per-block cache is not shared
  between workers.

## 14. Aggregates, projection, and top-K

Many questions asked of a job queue or a history table are not "which ads" but "how
many", "what range", "grouped by what", or "the top ten by". Answering them by
materializing every matching ad and folding in the caller is the most expensive possible
way. The engine answers them where the data is: in columns, in index postings, and in
zone maps.

These are **exact** fast paths (tenet 3). Each one is permitted only where its answer is
provably the evaluator's, and each declines — to a slower path, never to a guess —
otherwise. Two rules make that hold:

- **Exact probes.** A count derived from an index must use `vm.Query.ExactProbes`, not
  the superset `ProbePlan`. A superset of candidates is fine when every candidate is
  re-verified; it over-counts when it *is* the answer.
- **Expressions decline.** A held cell, an escaped value, or a field outside the schema
  sends the record, block or segment to the ordinary evaluator.

### Counting

`CountQuery` tries, in order:

1. **Column scans.** For one or more numeric comparisons on schema fields (a single
   field, or a conjunction across several), it narrows a candidate set one column at a
   time, skipping whole blocks by their zones (`colmulti.go`).
2. **Presence.** For `attr is undefined` and `attr isnt undefined`, it counts from the
   escape bitmaps and escape classes, declining on held expressions (`colpresence.go`).
   One production count went from 1.124 s to the cost of a bitmap read.
3. **Vectorized.** Any other native query, or a column scan that met an expression,
   runs through the vectorized executor block by block (Chapter 11). This step is
   skipped when an index could prune the query, because evaluating every visible record
   loses to a selective indexed scan; that routing mistake once measured 2.9× slower.
4. **Row scan.** Otherwise the caller uses the ordinary indexed or row scan.

`COUNT(*)` with no constraint is O(1) from the live-record counter.

### Numeric statistics and grouping

- **`NumStatsQuery`** computes MIN, MAX, SUM and AVG of a numeric attribute over
  matching records, from columns where possible (`colagg.go`).
- **`GroupCountQuery` / `GroupStatsQuery`** group by an attribute, under a constraint,
  including one evaluated by the vectorized executor (`colgroupcount.go`,
  `colvecgroup.go`). A GROUP BY that cost ~150× a plain count now costs ~3×; one
  measured case went from 8112 ms to ~38 ms.
- **Archive GROUP BY COUNT from postings.** On an archive, an unconstrained
  `COUNT(*) GROUP BY attr` on a categorical-indexed attribute is answered by summing
  posting-list cardinalities per segment (`archive_groupcount.go`, 16–62×). The total is
  checked against the record count, and any mismatch falls back.
- **Zone-verdict counts.** A constraint that is a pure conjunction of numeric ranges is
  decided per segment from zone maps: entirely in, entirely out, or must scan
  (`archive_groupcount_where.go`). A segment can be ruled out by its zone, but is
  counted wholesale only when its zone is `Full`, meaning every record held a number
  for the attribute. A record with no value fails the condition. An expression-valued
  record leaves the attribute with no zone at all, so the segment is scanned.

### Top-K

`TopKOrderThreshold` answers "the K records with the largest (or smallest) value of
`attr` matching `q`" in two passes (`coltopk.go`):

1. Find the K-th value from the column alone.
2. Re-run the projection with `attr >= threshold` added, which the index and zone paths
   can then prune hard.

The threshold pass reads only column values, never a record. A heap over every
matching row's projected values is never built. Server-side TopK in the `db` layer is
built on this.

### Projection checksums

`ProjectionChecksum` returns a stable 64-bit digest of selected attributes per ad, the
per-call counterpart to the ordered index's cluster signature (Chapter 20). A caller can
detect "these ads differ only in attributes I do not care about" without materializing
them.

---

# Part IV — Beyond key/value

## 15. Transactions, delta records, and time travel

Three features extend the MVCC store beyond last-write-wins point operations. They
share one idea: a version history the store already keeps can be put to more use.

### Optimistic transactions

`Put`, `Update` and `Delete` are unconditional and last-write-wins. Multiple writers
that must not lose each other's updates use **transactions** (`txn.go`,
`txnquery.go`):

```go
tx := c.Begin()
ad, _ := tx.Get(key)            // reads see the transaction's snapshot (and its own writes)
tx.Put(key, modified)           // writes are buffered
res := tx.Commit()              // {Committed, Conflicts, Unapplied, UnappliedReasons}
```

- **Snapshots are per shard and lazy.** A transaction captures a shard's sequence the
  first time it reads or writes a key in that shard. It also reads its own buffered
  writes. `tx.Query` and `tx.KeysWhere` scan at the transaction's snapshot.
- **Write-write conflicts only.** Reads are not tracked, so scans and constraint queries
  impose no bookkeeping. At commit, each buffered write applies only if its key was not
  modified by another committer since the snapshot.
- **Per-ad commit, not all-or-nothing.** HTCondor transactions treat each ad
  independently, so writes commit per key. Unaffected keys succeed even if others
  conflict. The caller retries just the `Conflicts`. A commit is not atomic across keys
  or shards.
- **Conflicts versus Unapplied.** A *conflict* is retryable. An *unapplied* write is not:
  it is a patch whose base can never be composed, for example a delta chain that no
  longer materializes. It is reported separately, with a reason, so a retrying caller
  can tell "try again" from "this will never work". A production mirror once looped for
  hours, replaying a 923 MB log 285 times, because the two were conflated.
- **Patches.** `PatchAttrs` and `PutPatch` write only changed and removed attributes,
  which is what feeds delta records.
- **Single-writer fast path.** When only one writer is active, commit performs no
  conflict checks at all (`ConflictChecks` counts them).
- **Durability.** Commit syncs every touched shard in parallel. `SetDurable(false)`
  defers that for callers that batch durability themselves.
- **Redaction.** `BeginRedacted` starts a transaction whose reads decode without the
  data key (Chapter 10).

### Delta records

Replaying a real OSPool access point's `job_queue.log`, 117,796 ad updates each rewrote
a ~7.3 KB ad, handing the encoder and compressor 861 MB — 43× the size of the log they
came from. The attributes one schedd transaction actually changes average ~150 bytes.

With `Options.DeltaMax` set, a write that changes only some attributes stores **only
those**, as a *delta record*. The record is flagged both in the record header
(`deltaFlag`, so a reader can tell without decompressing) and in the wire flags
(`flagDelta`).

- **Reads** walk back along the key's version chain, collecting deltas until they reach
  a full record, then merge oldest-first.
- **Bounding the chain.** A fresh full record is written once a key has accumulated
  `DeltaMax` deltas. The bound is advisory: correctness needs only that the walk
  eventually reaches a full record. A small per-key depth tracker, which forgets
  everything when it overflows and so forces a full write, keeps it approximate and
  self-healing.
- **The hard invariant.** Anything that can *drop* an older version must first
  materialize the deltas that depend on it:
  - Compaction collapses live deltas before a rewrite.
  - A segment that seals collapses deltas whose base would otherwise be stranded.
  - `Open` checks for, and repairs, live deltas stranded in sealed segments.
- **What it cannot combine with.** Delta records are refused together with time travel,
  since time travel retains bases that deltas depend on, under different rules. They are
  ignored on append-only collections.
- **Persistence marker.** A store that has ever written a delta records that in a
  `deltamode` marker, written durably before the first delta. An older build that
  cannot read deltas therefore refuses to open the store rather than misread it.
- **Cost under columnarization.** Superseded delta fragments still occupy rows in a
  columnarized segment until they are dropped (Chapter 9). One measured delta-heavy
  workload had 77% of its columnarized rows as dead fragments.

The `db` layer turns delta records on by default (`DefaultDeltaMax` = 16).

### Time travel

The store is already multi-version: every record carries the sequence at which it became
current and the one at which it was superseded. Time travel (`timeseq.go`) adds the two
missing pieces:

1. **A time-to-sequence map.** Each shard writes sparse `(seq, unixMillis)`
   **checkpoints**, at most one per `CheckpointInterval` (1 minute by default), on the
   first commit that crosses the boundary. A checkpoint is a marker record in the
   segment arena, so it rides the segment's existing `msync`, with no separate file. It
   is rebuilt into an in-memory index on recovery.
2. **Retention.** Compaction must not reclaim a superseded version still within the
   travel window. The *retain floor* is the sequence that was current at
   `now - MaxDistance`. Compaction keeps any version superseded after it, copying
   retained versions into a separate history stream.

`QueryAsOf(q, t)` resolves `t` to a vector of per-shard sequences — for each shard, the
last checkpoint at or before `t` — and runs an ordinary snapshot scan at that vector. A
point-in-time query is therefore consistent per shard and accurate to within one
checkpoint interval. With no `TimeTravel` option the mechanism is inert: no
checkpoints, and a retain floor equal to the current sequence. `SetTimeTravel` enables,
retunes or disables it at runtime. It is not retroactive.

## 16. Change data capture: Watch

Daemons often need to *follow* a collection, not just query it: rebuild a cache, feed a
dashboard, drive a listener. `Watch` (`watch.go`) delivers the **full ad** (not a
delta, even when the store holds deltas) for every add or update, and a **tombstone**
for every delete. A client can disconnect and later **resume** from an opaque cursor it
persisted. Semantics are **at-least-once**: over-delivery is fine, a missed change is
not.

**The cursor is per-shard, by design.** There is no global transaction counter, which
would reintroduce the write contention sharding removed. The cursor is opaque bytes
holding `{epoch, perShardSeq[]}`: one commit sequence per shard, plus a per-store
`epoch` identity.
- On resume, each shard replays records with `seq` greater than its cursor entry.
- A mismatched epoch (the store was rebuilt) forces a full replay.
- An append-only collection persists its epoch in `watch.epoch`, written only after a
  clean `Close`, so its cursors survive a clean restart.
- A mutable collection's epoch is still per-process, so a reopened mutable collection
  forces a full replay.

**Catch-up then live, with no gap.** The subtle part is joining a historical replay to
a live stream without missing or excessively duplicating events. Watch registers for
live delivery *first* and snapshots the per-shard sequence `S_reg`. It then does an
exactly-once catch-up scan filtered to `seq > cursor`, drains the events buffered during
catch-up, and streams live. The union of "catch-up up to `S_reg`" and "live beyond
`S_reg`" covers everything past the cursor with no gap; the overlap is a harmless
duplicate.

Live events are published only **after** the durability sync, so a watcher never sees
a change that a crash could take back. Sequence numbers are retired in order
(`pubOrder`), so a cursor never names a sequence whose events are not yet buffered.

**Deletes are the one genuinely hard case.** A delete leaves only a supersession stamp,
which compaction eventually reclaims. A client resuming from far enough back therefore
cannot be told precisely which keys vanished.
- The engine keeps a bounded per-shard **delete journal** (`Options.WatchHistory`
  entries). Within that window, deletes replay exactly.
- Beyond it, or on an epoch mismatch, Watch emits a `Reset` and replays all current ads.
  The client reconciles deletions by diffing the replay against the keys it holds.

This is the classic full-state-subscriber fallback, and it is the *only* inherent
limit.

The live path hooks the commit point, reusing the group-commit structure, so there is
one notification per coalesced batch. A slow client that overflows its bounded buffer
(`WatchBuffer`) is **demoted** — told to resync from its last cursor — rather than
allowed to block writers. The event protocol is small:
- `Upsert` and `Delete`: applied identically whether catching up or live.
- `Reset`: discard state; a full snapshot follows. This is the only control a client
  must honor.
- `Synced`: catch-up is done; here is a durable resume cursor.
- `Resync`: reconnect.

Refinements:

- **Coalescing.** `WatchCoalesce` delivers only the newest event per key in each
  window, for consumers that want state rather than history.
- **Starting at the head.** `WatchCursor` returns a cursor at the current head, for a
  consumer that wants only changes from now on.
- **Filtering.** `WatchFilter` filters a watch by a predicate. An `Upsert` that stops
  matching becomes a `Delete`, so a filtered consumer's state stays correct.
- **Redaction.** `WatchRedacted` decodes without the data key (Chapter 10).
- **Truncation.** `Truncate` empties a collection in place. It raises every shard's
  resume floor and sends live watchers a `Resync`, so older cursors get a `Reset`.

**Archives** use the same Watch. An append-only collection has no deletes, so catch-up
is an oldest-first replay of appends. A cursor older than the rotation floor (data
already rotated away) gets a `Reset`.

## 17. Parent/child chaining (cluster/proc)

HTCondor's job queue is not flat. A proc ad `cluster.proc` inherits shared attributes
from its cluster ad `cluster.-1`, which is stored once. A query like `DAGManJobId == 42`
must see the cluster's attribute on every proc. This is the one "join" the engine
supports, and it is built from scoped evaluation.

Two options enable it:
- `ParentKeyFor` maps a child key to its parent key. Bindings are immutable, because
  HTCondor forbids re-parenting.
- `IsStructural` marks parent-only ads (cluster ads). These are stored and used as
  parents, but **hidden** from Query, Scan and Watch results by default, exactly as
  `condor_q` omits cluster ads.

**Co-location dissolves the hard problem.** A whole family — a root and its
descendants — is routed to **one shard**, chosen by the family root. Because a parent
and its children share a shard, and therefore a single MVCC snapshot, chained evaluation
is consistent and lock-free, with **no cross-shard fetch**. Cross-shard consistency is
the hardest problem in a sharded store, and co-location makes it never arise here. A
transaction's `Get` resolves the parent at the same snapshot as the child.

**Evaluation and output are handled differently**, matching HTCondor's own behavior:

- *Filtering* (does this ad match?) chains at the **wire level**. The wire-native
  resolver, on a miss, falls through to the co-located parent's wire bytes, so a
  non-match never builds a ClassAd. The decode fallbacks `SetParent` the decoded ad, and
  the evaluator walks the parent chain for unscoped references.
- *Output* (the ad handed back) is **flattened**. The parent's attributes are
  materialized into the child, with the child overriding, so direct reads and
  serialization see the inherited attributes, like `condor_q` showing a job's full ad.

The scan is two-pass over one snapshot: collect the structural ads, then evaluate each
child with its parent chained, hiding structural ads and flattening matches.

Semantics mirror HTCondor:
- **No re-parenting.**
- **Auto-delete of an empty parent** when its last child leaves (HTCondor's
  `ClusterCleanup`).
- The archive stores **flattened** standalone ads, so a history record outlives its
  parent.
- Watch **fans out on a parent change**, re-emitting affected children.
  `ParentPrivateAttrs` names parent attributes, such as job-factory bookkeeping, that
  children do not inherit.

**The limitation** is that chained collections use only the serial two-pass scan.
Secondary indexes, query fan-out, parallel match, the column prefilter and the column
aggregates all bypass a chained collection. This is correct but slow, and is the
largest open performance gap for a schedd-shaped workload.

## 18. The history archive

The `condor_history` file is a different beast. It is write-once, rotated by age in
whole files, and **larger than RAM**. It is queried overwhelmingly for *recent* or
time-bounded ads: "the last K matching this constraint, newest first."

The engine serves it with an **append-only `Collection`**. `Archive` (`archive.go`) is
a thin facade that opens a `Collection` with `Options{AppendOnly: true, ReverseScan:
true}` and exposes archive-shaped methods. The policy differences are all expressed as
`Collection` rules.

### From sibling type to mode

The first archive was a separate store type, on the theory that bolting append-only
semantics onto `Collection` as flags would erode the live store's invariants. That
theory lost to duplication. The archive needed the segment backend, sidecars, pin/reap,
the wire-native evaluator, and later columnar blocks, interning, auto-indexing and
aggregates, and each of those had to be built twice. The append-only rules turned out
to be *subtractions* that are easy to state and test:

| Mutable `Collection` | Append-only `Collection` (archive) |
|---|---|
| sharded (default 16) | one shard — hash-sharding would destroy the time locality pruning and newest-first depend on |
| MVCC supersession, per-key `Delete` | none: every record is live from append to rotation; `Delete` is a no-op |
| in-RAM key directory | none: queries are constraint scans |
| compaction reclaims garbage | none needed: a sealed segment never holds garbage |
| index built by `Reindex` | index built **eagerly at seal**, off the write lock |
| forward scan | **newest-first** scan, with LIMIT pushdown |
| — | zone maps, retention rotation, segment merging, codec upgrade |

A sealed archive segment's *records* are never mutated. The *file* can still be
rewritten — merged, columnarized, interned, or re-encoded — through the crash-safe
rewrite of Chapter 7. Its index is derived and can be rebuilt or extended at any time.

### Segment lifecycle and recovery

Appends go to one active segment. A segment seals when the next ad will not fit, or when
the codec changes. At seal, `Append` builds the segment's index, key index and zone
map off the write lock. The active segment has no index and is always full-scanned.

There is **no catalog file**. Recovery is a directory listing of
`<Dir>/0/seg-*.dat`, with segments ordered by their first record's sequence and sealed
extents read from each sidecar's trailer. Restart is O(segments), not O(records). The
segment set on disk *is* the archive's state; structural changes commit through the
rename-and-marker protocol of Chapter 7.

### Zone maps

Append order is roughly time order, so a per-segment **min/max** of a few
time-correlated attributes (`CompletionDate`, `EnteredHistoryTime`, `ClusterId`…) lets a
time-bounded query **prune whole segments without opening their data or their index**.
`CompletionDate >= T` skips any segment whose max is below `T`. This turns "show me last
week's jobs" from a scan of 10⁴ segments into a scan of 3.

The zoned attributes are those named in `ZoneAttrs`, plus every `ValueAttrs` attribute
automatically. Zones are computed at seal (and at reopen for older segments), held in
memory keyed by intern id, and persisted in the sidecar's zone section **keyed by
attribute name**. Intern ids are process-local and shift when the configuration changes,
so a section keyed by id could apply one attribute's range to another and prune
segments that hold matches. A legacy id-keyed section is never trusted: it is
recomputed once at open and written back by name.

**A zone knows what it did not see.** When a zone is built, each record's value for
the attribute is classified:
- **A number** widens the range. Booleans count as 1 or 0, as they do to the
  comparison operators.
- **A value no numeric comparison can be true of** — absent, a string, undefined,
  error — is left out of the range, since it can never satisfy a numeric probe. It
  clears the zone's `Full` flag, so the zone no longer claims to cover every record.
- **An expression or an encrypted value**, whose value only evaluating the record can
  tell, removes the attribute's zone for that segment entirely.

A missing zone means "nothing known" to every reader: no pruning, no age-based
retention, and no wholesale counting.

The current section format is `ZNM3`: name-keyed, with flags. `ZNM2` sections were
built when expressions were silently skipped, and could prune a segment whose
expression-valued record matched. They are never adopted: they are recomputed once at
open and rewritten.

Columnar row groups carry their own per-block zones (Chapter 9), so pruning continues
*inside* a segment that survived the segment-level check.

### Querying

A query composes the layers already described:

1. **Prune** segments by zone map.
2. **Plan** each surviving segment through its index, or decline the plan when it would
   select more than 95% of the records. OR-shaped plans also decline, so that results
   stay newest-first.
3. **Filter** candidates through the column prefilter.
4. **Re-verify** with the full predicate.
5. **Decode** only true matches.

All of this runs newest-first, with early termination at `Limit(K)`. Counts, grouped
counts from postings, zone-verdict counts, numeric aggregates and top-K (Chapter 14)
all work on archives. They are what make `SELECT COUNT(*) … GROUP BY Owner` over a year
of history interactive.

### Retention

`Rotate(now)` drops whole oldest segments until the archive is within its bounds:

- **`MaxSegments`**, **`MaxBytes`**;
- **`MaxAgeAttr`/`MaxAge`**: drop a segment whose newest value of the age attribute is
  older than `now - MaxAge`. The age attribute must be zone-mapped.
- **`MinAgeAttr`/`MinAge`** with **`SetGCFloor`**: a change-feed source can reclaim
  segments every subscriber has acknowledged *before* `MaxAge`, but never anything
  younger than `MinAge`. The floor is runtime-only and re-asserted each pass, so a stale
  saved value can never collect data across a restart.

Rotation removes the segment from the live set, raises the append floor (a Watch cursor
behind it gets a `Reset`), and then retires the file. `munmap` and `unlink` wait for
in-flight scans' pins to drain. The active segment is never dropped.

### Merging

Every sealed segment costs about two memory mappings: the segment, plus its sidecar once
queried. Linux's default `vm.max_map_count` of 65530 therefore caps an unmerged archive
near 30k segments, about 240 GiB at 8 MiB each. Past that, the process *fails to start*.

`MergePass` keeps the count down by merging cold runs of adjacent sealed segments:
- It triggers at 1.25× a target count (8192 by default) and merges back down to the
  target.
- The newest 16 segments are left alone, because the open segment and its neighbours
  are rescanned by every query and should stay small.
- Run size is driven by bytes: segments grow toward `total / target`, up to 1 GiB.
- A pass rewrites at most 4 GiB of source data.
- Merges use the shared crash-safe rewrite (Chapter 7) and take the maintenance lock, so
  they never overlap a reseal.

### Other maintenance

- **Codec upgrade** (`UpgradeCodecPass`, Chapter 4): re-encode old segments under a
  newer dictionary, where measurement says it pays.
- **Auto-indexing** (Chapter 12). Demand is decayed and persisted. On an archive the
  tuner never drops an index, and the backfill horizon bounds how much history a new
  index rebuilds.
- **Columnarization** (Chapter 9), and interning at seal (`InternAtSeal`) or at retrain
  (Chapter 3).
- **Truncate** empties the archive in place and resets watchers.
- A process-global **block-cache budget** for archives, separate from the mutable
  tables' budget, so a history scan cannot evict a live table's working set.

---

# Part V — Negotiator and schedd operations

The engine's final layer implements the HTCondor operations that motivated it. This is
where "a ClassAd database" becomes "the negotiator's and schedd's data plane."
`docs/MATCH.md` carries the detailed proposal and measurements; this part is the
standalone summary.

## 19. Matchmaking: match and rank

The flagship operation: given a job ad, find the slots it *symmetrically matches* — the
job accepts the slot **and** the slot accepts the job — and rank them. This reuses the
standard `classad.MatchClassAd` for the authoritative bilateral test. Every optimization
below narrows which slots reach that test, or makes reaching it cheaper. None of them
changes its verdict: the authoritative match runs on the original job and slot ads and
always has the last word.

- **Parallel bilateral scan (A1).** Match fans out across segments like a query. One
  wrinkle: `MatchClassAd` *mutates* both ads' target fields, so a shared job cannot be
  matched concurrently. Each worker gets its own deep copy of the job. Results merge
  with no dedup, thanks to the MVCC exactly-once rule again.
- **Wire-native job-side reject (A3).** Most slots fail the job's `Requirements`, and
  fully decoding a slot only to reject it is wasteful.
  - The job's `Requirements` is compiled once and evaluated against each slot's *wire
    bytes*, with `TARGET` resolved from the slot.
  - If the job **definitely** rejects the slot — every referenced attribute resolves to
    a literal and the result is boolean false — the full decode and match are skipped.
  - Any uncertainty falls through to the full match: an undefined, a non-literal
    attribute, or a non-native `Requirements`.
  - For survivors whose job side is already known true, only the slot side is evaluated
    (`matchOneLeftKnown`).
  - On a 40k-slot pool with half the slots rejected, this takes serial matching from
    183 ms to 57.6 ms (3.2×); with fan-out, to 19.1 ms at 8 workers.
- **Index candidate pre-filter (A2).** When the slots are indexed and the job is
  selective, it is better not to *visit* a non-candidate at all.
  - The job's `Requirements` is rewritten into a predicate *over the slot*:
    `TARGET.attr` becomes the slot's own attribute, and every job reference is baked to
    its evaluated constant.
  - Undefined-guarded terms get `isnt undefined` exception disjuncts, so a slot missing
    the attribute is still a candidate when the job's expression would tolerate that.
  - Pure functions of low-cardinality categorical attributes are **materialized** into
    `in {…}` sets over the attribute's actual value domain (`materializeFinite`).
  - Anything target-dependent, non-scalar or opaque collapses to undefined, and its
    conjunct is dropped.
  - The result goes through the same DNF planner as a query (Chapter 12), with a
    selectivity gate that prefers a scan when the plan would not be selective. It is
    **sound by construction**: the rewrite only produces probes on the slot's own
    attributes against job constants, so a dropped or broadened probe merely widens the
    candidate set. On a 40k-slot pool with a ~2.5%-selective job it took the
    wire-reject scan from 5.78 ms to 1.09 ms.

Further refinements, each measured:

- **Hot-header match closure.** With `Options.MatchClosureRoots` set, every slot's hot
  header carries the full set of attributes its `Requirements` and `Rank` read, and is
  flagged complete (Chapter 3). The matcher reads that closure with `ForEachHot` instead
  of walking the ad. On OSPool slots, `MatchSorted(1)` went from 37.5 ms to 7.15 ms
  (5.2×).
- **Deferred materialization.** `MatchSorted(limit)` ranks survivors from their wire
  bytes and builds ClassAds only for the top `limit`: 5.47 ms → 2.78 ms for
  `MatchSorted(1)`.
- **Resource-side constraints.** `MatchSortedRankedFiltered` pushes an extra constraint
  on the slots — the negotiator's `WHERE TARGET` or a no-preemption filter — into the
  candidate plan instead of applying it afterwards.
- **Explain and demand.** `ExplainMatch` reports the plan a job would get. Match probes
  feed the demand tracker, so auto-tuning indexes the attributes jobs actually filter
  on.
- **Short-circuit operand order.** The matcher may reorder the job's **top-level
  `&&` conjuncts** by estimated cost and short-circuit probability. It does this only for
  the job-side verdict, which asks whether the job's `Requirements` is TRUE. For pure
  operands, whether a conjunction is TRUE does not depend on order. `||` chains, nested
  chains and chains with an impure operand keep their written order, because of the left
  bias of Chapter 2: `ERROR || TRUE` is not `TRUE || ERROR`. The job ad itself is never
  rewritten, so `Rank`, index planning and the slot's view of `TARGET.Requirements` all
  see the original expression. A differential test checks every match entry point
  against `MatchClassAd` on randomly generated chains that mix TRUE, FALSE, UNDEFINED,
  ERROR and non-boolean operands.

The tiers compose the way the query engine's do. An index narrows *which* slots are
visited (A2). A wire-native check rejects most of those without decoding (A3). The full
bilateral match, run in parallel (A1), is the authoritative arbiter for the survivors.
Chained collections skip all of this and match by serial scan (Chapter 17).

## 20. The maintained ordered index

The schedd walks its idle jobs in a fixed priority order — `JobPrio`, then submission
time, then the order the job entered the queue — per user, to decide what to run next.
Re-sorting that queue every negotiation cycle throws away work, because the ordering key
is stable across a job's lifetime. The engine offers a **maintained, filtered, ordered
index** instead: a secondary index the collection keeps sorted on the write path, so the
schedd iterates it directly. It is a priority queue that is always ready (`ordered.go`).

It is configured by an `OrderSpec{Partition, Where, Keys, Cluster}` and read via
`Ordered(i, partition, resume)`:

- **Partitioned** by an attribute (e.g. `Owner`): one independent ordered run per user.
- **Filtered** by a `Where` predicate (e.g. `JobStatus == 1`), so it holds only the ads
  that matter. This is the subtle part: membership churns as jobs move into and out of
  idle, so state transitions are inserts and deletes, not just "add on submit, remove on
  completion."
- **Ordered** by composite `Keys` (`SortKey{Expr, Desc}`), with a **stable insertion
  sequence as the final tiebreaker**: "the order the job entered the queue," surfaced so
  the application never has to reconstruct it.
- **Resumable.** Each step yields the ad plus a cursor that resumes strictly after it,
  so a negotiator can walk a partition across many calls. Chapter 21 covers what stays
  consistent during a long walk.
- **Cluster signature.** `OrderSpec.Cluster` names the attributes whose combined value
  forms a 64-bit signature, computed once at write time and surfaced with each ad. The
  schedd's resource-request-list construction then becomes a run-length fold over the
  ordered stream: start a new list when the signature changes. That is a stored-integer
  compare instead of re-hashing each ad's requirement attributes every cycle.
- **Redacted reads.** `OrderedRedacted` serves the same walk to callers not entitled to
  sealed values (Chapter 10). It is what the RPC layer exposes to unprivileged sessions.

**Why a B-tree and not a heap.** A heap gives the top element cheaply, but not
*resumable, non-destructive, ordered iteration*, and the schedd wants to walk *down* the
list cycle after cycle, not pop it. The structure is a
[copy-on-write B-tree](https://github.com/tidwall/btree). Writes mutate one master
under a lock: an O(log n) insert, delete or reposition per event, versus an
O(n log n) re-sort of the whole idle set every cycle. Reads take an O(1) copy-on-write
snapshot and iterate it lock-free. On a state transition the index does the minimum:

- Attribute churn that changes neither membership nor key is a no-op.
- A transition into or out of idle is one O(log n) insert or delete.
- A rare priority edit is one O(log n) reposition.

The index is maintained over the same inherited view the rest of the engine uses. A
structural (cluster) ad is never a member, and a child's `Where`, `Partition` and `Keys`
resolve parent attributes via the parent chain. A partition key like `Owner` that lives
on the cluster ad therefore still partitions the child procs. It is derived state,
held only in memory, and rebuilt from the recovered records on a persistent `Open`.

## 21. Consistency, MVCC, and concurrency

This chapter ties the guarantees together. "How consistent is a read?" has one answer
for the store and deliberately different ones for the ordered index, transactions and
time travel, and the differences are decisions worth explaining.

**The store: per-shard snapshot isolation, exactly-once.** A `Scan` or `Query` reads
each shard at a fixed `S0` and yields exactly one version per key (Chapter 5). This is
*per-shard*: a scan concatenates independent per-shard snapshots rather than taking one
global atomic snapshot, because a global snapshot would need a global sequence counter
and reintroduce write contention. This per-shard level is the engine's consistency
contract, and every other reader is defined relative to it:

- **Transactions** take each shard's snapshot lazily, on first touch (Chapter 15).
  Commits are per key: a transaction is not atomic across keys, and so not across
  shards.
- **Point-in-time queries** resolve a wall-clock time to a per-shard sequence vector
  through checkpoints. They are consistent per shard, and across shards only to within
  one `CheckpointInterval`.
- **Sequence-cursor pagination** (`QueryRawFromSeq`) pages per shard, so a page boundary
  is a per-shard position, not a global instant.
- **Parent/child families** live in one shard, so a family is always read consistently.

**The ordered index: a single mutable structure, not per-segment.** The value indexes
are per-segment and resolved at read time by the `seq` rule. The ordered index is
deliberately *not*: it is one in-memory copy-on-write B-tree holding one entry per key,
maintained imperatively. This is a genuine fork in the design. A reader steeped in
LSM/SSTable design would expect a per-segment index, and the reasons are:

- Per-run indexes are *forced* in an LSM because on-disk runs are immutable. This index
  is in-memory, derived, and rebuilt on `Open`, so it is free of that constraint. It is
  a mutable secondary index, not an LSM per-run index.
- Per-segment would also be the *wrong shape* for the access pattern. The schedd wants a
  **global** ordered walk. Per-segment ordered runs would force a k-way merge plus
  cross-segment version resolution on every negotiation cycle, reintroducing exactly the
  per-cycle merge cost the index exists to eliminate. One tree gives an O(1) snapshot
  and an O(k) walk, and stays **decoupled from segment lifecycle**: compaction,
  columnarization and merging never have to rebuild it.

**Concurrency control.** Two locks that never contend for long:
- `oi.mu` guards master mutation and the O(1) snapshot copy.
- Each B-tree carries its own `RWMutex`. A walk holds only the *snapshot's* read lock
  (a private tree, uncontended); writers take the *master's* lock. Different mutexes,
  no cross-blocking.

Copy-on-write makes shared nodes read-only after the copy (both trees copy before
writing). A reader traversing a node while a writer path-copies it is therefore
race-free, as verified under `-race`.

**What a long ordered walk sees.** Suppose the negotiator consumes the ordered stream
one entry at a time over many seconds, with network I/O between steps. Nothing blocks:
writers keep committing. The snapshot freezes *order and membership* at the call
instant, while each ad is fetched **live** per step. So:
- A job that leaves idle mid-walk is still visited. Its live ad reflects the new state,
  and the negotiator re-verifies.
- A deleted one is skipped.
- A repriced one appears at its old position, with current content.
- A newly idle one is not seen until a later snapshot.

A single long-held walk also pins the snapshot's nodes, so heavy churn accrues extra
memory until it ends. **For long feeds, chunk with the resume cursor**: each call
re-snapshots in O(1) and resumes after the cursor, bounding pinned memory and picking up
members added below the cursor between chunks.

**The write-consistency boundary, stated honestly.** Ordered-index maintenance runs
*after* the store commit, not in the same critical section.
- Under **serialized per-key writes** — the schedd's model, where the job queue is a
  single serialized log — the index is exactly consistent.
- Under genuinely concurrent writes to the *same* key from different goroutines, the
  index can converge to a stale version until that key's next write. The store keeps
  the authoritative version by max-`seq`, and the index self-heals.

Making it fully correct there would require per-segment indexes (the expensive read
path above) or version tombstones in the index (unbounded state for a filtered index
whose non-members dominate). Neither is warranted for the single-writer-per-key target.
This is a chosen boundary, not an oversight.

---

# Part VI — The database layer

## 22. The `db` package: tables, catalog, views, exporters

`collections` is an engine; `db` (`github.com/PelicanPlatform/classad/db`) is the
database built from it. htcondordb serves `db` over the network. Its model is HTCondor's
`ClassAdLog` (`classad_log.h`): a persistent key-to-ClassAd store with transactions.
Unlike `ClassAdLog`, it allows any number of concurrent transactions.

### A table: `DB`

A `DB` wraps one persistent `Collection` and adds:

- **Transactions in `ClassAdLog` style**, mapped onto `collections.Txn`, with optimistic
  retry, plus `Put`, `Delete`, `DeleteWhere` and `KeysWhere`.
- **Delta records on by default** (`DefaultDeltaMax` = 16).
- **Wire-native ingest and relay** (`rawwire.go`): old-ClassAd text and wire bytes in,
  wire bytes out, without building objects.
- **Query pushdown** of projections, counts, GROUP BY, numeric aggregates and TopK onto
  the column and index paths of Chapter 14 (`aggregate.go`, `colagg.go`, `topk.go`).
- **Time travel** (`QueryAsOf`), and **Watch**.
- **Maintenance** (`Maintain`): compaction, dictionary retraining, auto-indexing,
  columnarization, and archive merging and codec upgrade, on caller-controlled cadences.
- **Persisted configuration.** The index set, hot set, time-travel settings and base
  codec survive restart (`indexcfg.json`, `basecodec`).
- **Encryption**: the master-key lifecycle of Chapter 10. Sealing private attributes is
  always on; it counts as encryption at rest only when pool keys wrap the master.
- **Authorization helpers** (`authz.go`): a walk that collects every attribute an
  expression could reference, scoped or not, so access checks cannot be evaded by
  spelling.
- **Snapshot and restore** (`snapshot.go`): a streamed backup of every ad, as
  independently compressed and encrypted frames.
  - Under encryption, each snapshot has a fresh key, wrapped by the database's *backup*
    subkey (distinct from the data key).
  - The master-key envelope is embedded, so any pool key can restore it on another
    node.

### The catalog

A `Catalog` (`catalog.go`) is a set of named objects of four kinds, each in its own
namespace and its own directory:

| Kind | What it is | Directory |
|---|---|---|
| table | a mutable `DB` | `<root>/tables/<name>/` |
| archive table | an append-only `Archive` (history) | `<root>/archives/<name>/` |
| view | a materialized aggregate over a table's change stream | `<root>/views/<name>/` |
| exporter | the definition and resume state of an external change sink | `<root>/exporters/<name>/` |

Tables are isolated: their own keyspace, indexes, hot set and configuration, and no
joins. A catalog is recovered by enumerating those directories. Tables open
concurrently, with bounded parallelism, because a serial open was once the whole of a
15-second startup.

**Views** (`view.go`, `viewcontinuous.go`) are cardinality-limited aggregates —
`COUNT`, `SUM` and `AVG` only, the aggregates maintainable by delta — kept current from
a base table's Watch stream.
- They are intended for Prometheus: columns are typed by alias prefix (`label_*`,
  `metric_*`).
- The change stream carries no before-image, so a view keeps each key's last
  contribution in order to subtract it, which makes its memory O(base rows).
- The definition (`view.json`) is persisted; the data is rebuilt from the base table at
  reload.
- A view with a time-bucketed group column is a **continuous aggregate**: closed buckets
  are sealed into a per-view archive (`archive/`), with a persisted watermark
  (`state.json`).

**Exporters** (`exporter.go`) are a passive registry. The catalog stores a sink's
definition (`def.json`) and an opaque resume-state blob (`state.bin`) and never runs or
interprets either. The out-of-process exporter — a Kafka change-data exporter, say —
reads them over RPC and checkpoints after its data is accepted downstream, for
at-least-once delivery.

**Replication.** The `replicate` subpackage defines a transport-neutral `Change` and an
idempotent `Sink` that applies one to a table, with a file-backed cursor store. The RPC
layer exposes a propose hook through which a consensus layer can route writes.
htcondordb's consistent mode puts its Raft log and state machine there; neither lives in
this repository.

**Interfaces.**
- `dbrpc` serves a catalog over CEDAR, HTCondor's wire protocol, with a client. It
  covers queries, transactions, Watch, aggregates, TopK, views, exporters, snapshots
  and diagnostics, and gates private attributes per session.
- `db/capi` exports `cadb_*` C symbols (open, transactions, query, watch), so a C++
  daemon can embed the database directly.

### Coverage gaps

Two current gaps are worth knowing:

- **Snapshots cover mutable tables only.** A catalog snapshot omits archive tables and
  views. History must be backed up at the file level.
- **Archives are not encrypted.** An archive opened from a catalog has no data key, so
  it stores private attributes in the clear (Chapter 10).

### On-disk layout of a catalog

```
<root>/
  tables/<name>/                 one DB: a persistent Collection directory (Chapter 8) plus
      dbid                         stable database identity
      basecodec                    "zstd" for new stores, "identity" for legacy ones
      indexcfg.json                index set, hot set, time-travel settings, auto-index provenance
      masterkeys.json              master key wrapped under each pool key, or
      masterkey.unprotected        the master in the clear (no pool keys)
  archives/<name>/               one Archive: an append-only Collection directory plus
      archiveconfig.json           creation marker and configuration
  views/<name>/
      view.json                    definition
      state.json, archive/         continuous aggregates: watermark and sealed buckets
  exporters/<name>/
      def.json, state.bin          definition and opaque resume state
```

Appendix C lists every file in a collection directory.

---

# Part VII — Reflection

## 23. Design principles and decisions

Five ideas recur; the engine is largely their consequences.

1. **Reuse the substrate.** Every capability is the wire format, the `vm` engine, the
   segment backend and the reclamation machinery, plus a thin policy layer.
   - Matchmaking reuses the parallel scan and the wire-native evaluator.
   - The archive is the same `Collection` with append-only rules.
   - Columnar blocks reuse segment framing, the crash-safe rewrite and pin/reap.
   - The vectorized executor runs the scalar interpreter's own programs.

   There is one engine, not five. The archive's history is the cautionary tale: it began
   as a sibling type and was folded back once duplication outweighed separation.
2. **Soundness by construction, in one of two forms.** A fast path is either a
   *superset* filter with an authoritative re-verify behind it, or an *exact* answer
   that declines whenever it cannot prove itself.
   - Superset filters: index probes, the match pre-filter, zone maps and Bloom skips.
   - Exact answers: column-decided matches, column counts and aggregates, and posting-sum
     GROUP BY.

   The superset form is forgiving: a mistake costs selectivity. The exact form is not:
   a mistake is a wrong answer, delivered faster than the right one.

   Every soundness bug found so far has had the same shape. An exact or pruning path met
   a value it did not model — usually an attribute holding an *expression* rather than a
   literal — and treated it as absent:
   - a held expression read as UNDEFINED by a column count;
   - zone ranges built from literals only;
   - operand reordering that assumed `&&`/`||` commute.

   The rule that follows: **every summary must know what it did not see**. That means an
   `escaped` flag on a block zone, an exactness flag on a segment zone, and held cells
   for column reads. And every exact path needs a differential test whose fixtures put
   expressions where literals usually are.
3. **Consistency is the product.** The MVCC `seq ≤ S0 < supersededBySeq` interval is the
   single invariant under exactly-once scans, safe parallelism, safe compaction and
   rewrites, transactions, time travel, and deterministic recovery. Where a stronger
   guarantee (a global snapshot, a fully MVCC ordered index, all-or-nothing multi-key
   commits) would cost contention or the wrong access pattern, the weaker guarantee is
   chosen *explicitly* and its boundary documented (Chapter 21), not left implicit.
4. **Derived state is disposable; authoritative state is framed with the data.**
   Indexes, key indexes, zone maps, sidecar columnar copies, directory snapshots and
   demand counters are *persisted*, because rebuilding them made restarts of large
   stores take minutes. They are persisted only as checksummed, versioned, validated
   caches, ignored and rebuilt on any doubt, so persistence can make startup faster but
   never wrong. The one structure that is *not* derived — a segment's columnar
   payload — is therefore stored inside the segment file, under its CRC and fsync, and
   versioned so that a layout change is detected rather than misread. The ordered index
   is still rebuilt from records on every open.
5. **Gate the complexity; measure the gain.** Parallel fan-out, the match pre-filter,
   dictionary training, hot headers, columnarization, vectorization, the column
   prefilter, codec upgrades and segment merges all *engage only when they pay*. The
   gates are a size threshold, a worker budget, a selectivity estimate, a built index,
   a measured compression gain, or a mapping budget. Each shipped behind a benchmark
   that justified it. Complexity that does not pay for itself is left on the serial
   path.

A selection of concrete decisions and their rationale:

| Decision | Rationale |
|---|---|
| Self-delimiting wire value nodes | Skip or inspect any attribute without decoding it — the basis of the row fast paths |
| Process-interned names in memory, inline in the active segment, per-segment dictionaries when sealed | Density where cheap; self-contained segments with no global, crash-fragile string table |
| Per-segment trained ZSTD dictionaries for rows; dictionary-less region codec for columns | 6.5× density on repetitive row records; columns get their redundancy from layout |
| PAX columnar sealed segments, each value stored once | 5× denser than per-record compression; counts and aggregates read columns, not ads |
| Columnar payload inside the segment, not beside it | It is the only copy of the values; it must share the segment's framing, CRC, fsync and lifetime |
| Row groups bounded in bytes | Point-read and cache admission scale with block bytes, not rows |
| Group schemas by exact co-occurrence | Column coverage independent of the table's ad mix; no clustering, no tuning |
| Append-only segments + MVCC `seq` | Lock-free reads, exactly-once scans, trivial parallelism, max-`seq` recovery |
| Group commit with a coalesced sync layer | Amortize `msync` across concurrent writers (≈12× under a slow sync) without weakening durability |
| Sticky sync errors; fallocate'd segments | Never report a write durable after a failed sync; ENOSPC fails cleanly instead of SIGBUS |
| Indexes never on the write path | Writes never block on index maintenance; an unbuilt index degrades to a scan |
| One sidecar container per segment, validated and disposable | One file per segment for mappings; fast restart that can never yield a wrong index |
| Pageable key directory via per-segment key indexes | Resident memory tracks active keys, not total keys; no LSM merge, because currency is local |
| Co-locate a cluster/proc family on one shard | Chained evaluation is consistent and lock-free; the cross-shard fetch never arises |
| Archive as a `Collection` mode, not a sibling type | The append-only rules are subtractions; a second engine duplicated every feature |
| Ordered index as one COW B-tree, not per-segment | Matches the schedd's global ordered walk; O(1) snapshot, decoupled from rewrites |
| Per-ad transaction commit, conflicts separated from unapplied | Matches HTCondor's per-ad transaction semantics; a retry loop can tell "again" from "never" |
| Delta records bounded by an advisory chain length | ~49× less write volume on job-queue updates; correctness needs only a reachable full record |
| Wire-native reject + index pre-filter for Match, both re-verified | 9.6× then 5.3× on selective matchmaking, with the authoritative match always deciding |

## 24. Correctness and testing

The optimizations in this book are only tenable because correctness is pinned down
independently and continuously.

- **Differential fuzzing against HTCondor's C++ libclassad.** The ClassAd evaluator and
  parser are fuzzed against HTCondor's own libclassad, loaded in-process through cgo
  (`fuzz/`), on every pull request. The working assumption is that C++ is correct unless
  its behavior is "very strange". The divergence rate is down to roughly 5 per 360k
  random ads, all deeply pathological. Genuine C++ quirks that Go deliberately does not
  mirror are documented for upstream rather than copied. This is why "evaluate via the
  same hooks as the tree-walker" (Chapter 11) is a real guarantee.
- **Differential testing of every fast path against a brute-force oracle.** Each of the
  following is tested to produce exactly the result set of a full decode plus full
  evaluation over the source ads:
  - wire-native evaluation and partial decode;
  - the column prefilter and the vectorized executor;
  - the index candidate scan (`FuzzIndexMatchesFullScan`) and the parallel scan;
  - the match pre-filter, against `MatchClassAd` on the original ads;
  - zone pruning;
  - archive queries and archive aggregates (`db/archive_aggregate_diff_test.go`).

  Exact paths get fixtures that deliberately put expressions, wrong-typed values and
  absent attributes where literals usually are, since that is where they have failed.
- **Exactly-once under the race detector.** The MVCC invariant is stress-tested under
  `-race` with all of these running together: concurrent updates, deletes, compaction,
  dictionary retraining, columnarization, delta collapse, transactions, and indexed
  reads. The ordered index's copy-on-write model is included. CI runs the whole suite
  under `-race`.
- **Crash, corruption and resource exhaustion.**
  - Reopen tests cover updates, deletes, deltas, interning, columnar payloads, and
    every sidecar and container version.
  - A truncated (torn) tail recovers only the committed prefix, and a deleted key stays
    deleted.
  - Merges, columnarization and other rewrites are killed at each step of the intent
    protocol.
  - A corruption harness damages specific bytes and reports which keys survive.
  - Fault injection covers torn writes, bit rot, failed syncs and ENOSPC, the last also
    run nightly against a real full tmpfs.
  - `Fsck` and `Repair` are tested against the same damage.
- **Scale and soak.** A nightly soak drives a million-record store through sustained
  mixed load. Scale tests check that resident memory follows the active working set
  rather than the total.
- **A benchmark matrix.** A sweep over format × workload × query type × concurrency
  produces the performance tables, so a change that regresses a regime is visible.

---

## Appendix A: Performance summary

Representative figures from a 12-core development box on real OSPool ad corpora. They
are directional, not a spec sheet. Row-store figures date from July 2026 and were
measured before columnar segments existed. Later figures come from the commits that
introduced each optimization.

| Area | Baseline | Optimized | Factor |
|---|---|---|---|
| Density, row records (bytes/ad) | 8681 (uncompressed) | 1286 (ZSTD + dict) | 6.5× |
| Density, columnar segments (B/rec) | 3445 (row, ZSTD + dict) | 673 (columnar) | 5.1× |
| Selective query, row form | 167 µs/ad (full decode) | 3.5 µs/ad (wire-native) | ~48× |
| Query allocations | ~10/ad | ~2/ad | 5× |
| Column prefilter, 2%-selective scan | 35.9 ms | 6.3 ms | 5.7× |
| Indexed archive fetch | 2.9 s | 14 ms | ~200× |
| Vectorized COUNT | 5.84 ms | 0.76 ms | 7.7× |
| GROUP BY over columns | 8112 ms | ~38 ms | ~200× |
| Archive GROUP BY COUNT from postings | row scan | posting sums | 16–62× |
| Index probe cost bound | 14 ms | 0.76 ms | 18× |
| Full-ad scan on columnar segments | 1× (row) | ~1.2× (reassembly) | 0.8× |
| Group commit (200 µs sync) | 1 ad/sync | 12.5 ads/sync @ 24 writers | ~12.6× |
| Persistent recovery (crash) | — | ~19–27 µs/ad replayed | — |
| Parallel query (40k, decode-heavy) | 7038 ms (serial) | 855 ms @ 8 workers | 8.2× |
| Matchmaking (40k, half reject) | 183 ms (serial) | 19.1 ms (wire-native + 8 workers) | 9.6× |
| Matchmaking, selective + indexed | 5.78 ms (wire-reject scan) | 1.09 ms (index pre-filter) | 5.3× |
| `MatchSorted(1)`, hot-header closure | 37.5 ms | 7.15 ms | 5.2× |
| `MatchSorted(1)`, deferred materialization | 5.47 ms | 2.78 ms | ~2× |
| Old-ClassAd ingest | parse + Put | `UpdateOld` fast path | ~3× |

## Appendix B: API sketch

The primary surface, abbreviated. Types are illustrative, not exhaustive; the godoc is
authoritative.

```go
// Construction
func New(opts Options) *Collection                    // in-memory
func Open(opts Options) (*Collection, error)           // Dir set ⇒ persistent (recovers)
func (c *Collection) Close() error

type Options struct {
    // Layout
    Shards, SegmentSize int
    Dir                 string            // "" ⇒ in-memory; set ⇒ persistent
    AppendOnly, ReverseScan bool          // the archive mode
    Codec               Codec             // base compression (default identity)
    InternAtSeal        bool
    HotAttrs, MatchClosureRoots []string
    // Indexing and pruning
    CategoricalAttrs, ValueAttrs, ZoneAttrs []string
    IndexBackfillBytes  int64
    DemandHalfLife      time.Duration
    Ordered             []OrderSpec
    // Columnar
    GroupSchemaCount, GroupStabilityRuns, ColumnarSegmentBudget, RowGroupBytes int
    GroupMergeJaccard, GroupMaxPartialFrac float64
    MutatingBlockCacheBytes, ArchiveBlockCacheBytes int64   // process-global budgets
    // Versions and history
    DeltaMax            int               // delta records (not with TimeTravel)
    TimeTravel          *TimeTravelOptions // {MaxDistance, CheckpointInterval}
    Retention           Retention          // append-only rotation bounds
    // Watch
    WatchHistory, WatchBuffer int
    WatchCoalesce       time.Duration
    // Chaining
    ParentKeyFor        func(key []byte) []byte
    IsStructural        func(key []byte) bool
    ParentPrivateAttrs  []string
    // Encryption and integrity
    DataKey             []byte
    EncryptedAttrs      []string
    DisableReadVerification bool
    QueryParallelism    int               // 0 auto, 1 serial, N cap
    CommitSync          func()
}

// Point + batch
func (c *Collection) Put(key []byte, ad *classad.ClassAd) error
func (c *Collection) Update(batch []AdUpdate) error
func (c *Collection) UpdateOld(batch []OldAdUpdate) error   // socket-form fast path
func (c *Collection) Get(key []byte) (*classad.ClassAd, bool)
func (c *Collection) Delete(key []byte) bool

// Transactions
func (c *Collection) Begin() *Txn                            // + BeginRedacted
func (tx *Txn) Get/Has/Put/PutOld/PatchAttrs/PutPatch/Delete(...)
func (tx *Txn) Query(q *vm.Query) iter.Seq[*classad.ClassAd]
func (tx *Txn) KeysWhere(q *vm.Query) iter.Seq[string]
func (tx *Txn) SetDurable(bool)
func (tx *Txn) Commit() CommitResult   // {Committed, Conflicts, Unapplied, UnappliedReasons}

// Query and read surfaces
func (c *Collection) Scan() iter.Seq[*classad.ClassAd]
func (c *Collection) Query(q *vm.Query) iter.Seq[*classad.ClassAd]   // + QueryRedacted
func (c *Collection) QueryAsOf(q *vm.Query, t time.Time) (iter.Seq[*classad.ClassAd], error)
func (c *Collection) QueryProject(q *vm.Query, attrs []string) iter.Seq[[]classad.Value]
func (c *Collection) QueryRawProjected(q *vm.Query, proj []string, chaseRefs, redact bool) iter.Seq[RawAd]
func (c *Collection) QueryRawWire(q *vm.Query, proj []string, redact bool) iter.Seq[[]byte]
func (c *Collection) QueryRawFromSeq(q *vm.Query, proj []string, after SeqCursor, limit int) (iter.Seq[RawAd], *SeqPage)
func (c *Collection) ExplainQuery(q *vm.Query) QueryExplain

// Aggregates
func (c *Collection) CountQuery(q *vm.Query) (int, bool)
func (c *Collection) NumStatsQuery(q *vm.Query, attr string) (NumStats, bool)
func (c *Collection) GroupCountQuery(q *vm.Query, groupAttr string) ([]GroupCount, bool)
func (c *Collection) GroupStatsQuery(q *vm.Query, groupAttr string, aggAttrs []string) ([]GroupStats, bool)
func (c *Collection) TopKOrderThreshold(q *vm.Query, attr string, desc bool, k int, st *ScanStats) (float64, int, bool)

// Indexes and maintenance
func (c *Collection) Reindex()
func (c *Collection) AddIndex(categorical, value []string) bool
func (c *Collection) DropIndex(attrs ...string) bool
func (c *Collection) AutoTune(opts AutoTuneOptions) AutoTuneResult
func (c *Collection) Compact() int
func (c *Collection) RetrainDict(sampleMax int) (int, error)
func (c *Collection) BuildAndEnableSchemaScan(sampleMax, hotTopN int) bool  // columnarization pass
func (c *Collection) Truncate()
func (c *Collection) StopMaintenance()

// Matchmaking
func (c *Collection) Match(job *classad.ClassAd) iter.Seq[*classad.ClassAd]
func (c *Collection) MatchSorted(job *classad.ClassAd, limit int) []*classad.ClassAd
func (c *Collection) MatchSortedRanked(job *classad.ClassAd, limit int) []RankedMatch
func (c *Collection) MatchSortedRankedFiltered(job *classad.ClassAd, targetConstraint string, limit int) ([]RankedMatch, error)

// Ordered index (schedd priority queue)
type OrderSpec struct {
    Partition string     // e.g. "Owner"
    Where     string     // membership predicate, e.g. "JobStatus == 1"
    Keys      []SortKey  // composite order; SortKey{Expr, Desc}
    Cluster   []string   // attributes forming the RRL cluster signature
}
func (c *Collection) Ordered(index int, partition classad.Value, resume OrderCursor) iter.Seq[OrderedAd]
// + OrderedRedacted;  OrderedAd = { Ad *classad.ClassAd; Cursor OrderCursor; Signature uint64 }

// Change data capture
func (c *Collection) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error)
func (c *Collection) WatchCursor() ([]byte, error)           // + WatchRedacted
func WatchFilter(seq iter.Seq[WatchEvent], match func(*classad.ClassAd) bool) iter.Seq[WatchEvent]

// Archive: a facade over Collection{AppendOnly, ReverseScan}
func CreateArchive(opts ArchiveOptions) (*Archive, error)
func OpenArchive(opts ArchiveOptions) (*Archive, error)
func (a *Archive) Append(ad *classad.ClassAd) error
func (a *Archive) Query(q *vm.Query) iter.Seq[*classad.ClassAd]               // newest-first
func (a *Archive) QueryLimit(q *vm.Query, limit int) iter.Seq[*classad.ClassAd] // + LIMIT pushdown
func (a *Archive) Rotate(now float64) (int, error)
func (a *Archive) SetGCFloor(float64)
func (a *Archive) MergePass(MergeOptions) int
func (a *Archive) UpgradeCodecPass(UpgradeOptions) int
func (a *Archive) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error)
// + the Collection's projection, count, group, stats, top-K and index methods

// Offline integrity
func Fsck(dir string) (*FsckReport, error)
func Repair(dir string, opts RepairOptions) (*RepairReport, error)
```

## Appendix C: On-disk reference

### A collection directory

Persistent and append-only collections share one layout. Shard directories are numbered
`0` to `N-1`; an append-only collection has only `0`.

| Path | Content | Format |
|---|---|---|
| `<shard>/seg-<n>.d<dictID>.dat` | a segment: 8-byte-aligned records from offset 0, no file header | record layout in Chapter 5; ads in wire v1 (Chapter 3) |
| `<shard>/seg-<n>.d<dictID>.dat.idx` | sidecar container: attribute index, key index, columnar copy, zones, extent, dictionary offset | container `SCN4`; ARCX v9; KIDX v1; COLX v7; zones `ZNM3` (Chapter 8) |
| `<shard>/dir.snap` | key-directory snapshot from a clean `Close`; consumed on the next open | `DIRS` v1, CRC-checked |
| `<shard>/tmp-merge-*.dat`, `<shard>/*.merging` | a rewrite's staged output and its intent marker | transient; completed or ignored by recovery |
| `<shard>/*.idx.tmp` | a sidecar being written | transient |
| `dicts/<id>.zst` | trained ZSTD dictionary (id 0, the base codec, is never stored) | raw ZSTD dictionary |
| `dicts/attic/<id>.zst` | dictionaries no segment references any longer | raw ZSTD dictionary |
| `watch.epoch` | Watch epoch and per-shard high-water sequence, written on clean `Close` (append-only) | `EWCH` v1 |
| `deltamode` | the store contains delta records | text `2` |
| `demand.json` | decayed query-demand counters and index provenance | JSON |
| `groupschemas.json` | derived group schemas | JSON |
| `upgrade.json` | codec-upgrade verdicts per segment and dictionary generation (append-only) | JSON v1 |
| `sealed-attrs.done` | the private-attribute sealing migration has completed | text |
| `quarantine/` | originals moved aside by `Repair` | — |

### Inside a segment

| Record | `keyLen` flags | Body |
|---|---|---|
| ordinary record | none | key + compressed wire ad |
| delta record | `deltaFlag` | key + compressed wire ad with `flagDelta`: changed attributes only |
| remnant | `colFlag` | key + compressed wire ad holding only non-columnar values |
| time checkpoint | `markerFlag` | 8-byte unix-millis |
| dictionary | `markerFlag \| dictFlag` | the segment's attribute dictionary: MPH, sorted ids, id→offset table, names |
| columnar payload | `markerFlag \| colFlag` | `COLN` magic + u32 version (absent = version 0), then schema, cold-tail names, hot set, row-group blocks, record-offset map, group schemas |

### The sidecar container trailer

```
… sections …
attrLen u32 | keyLen u32 | colLen u32 | zoneLen u32 | segUsed u64 | dictRec u32 | magic "SCN4"
```

Older trailers are still read: `SCNT` (attrLen, keyLen), `SCN2` (+ colLen), `SCN3`
(+ zoneLen and segUsed).

### The key index (KIDX v1)

```
magic u32 | version u16 | pad u16 | upto u32 | count u32
count × { keyhash u64, offset u32 }        sorted by hash, then offset
crc32 u32                                  IEEE, over all preceding bytes
```

---

*This book describes the engine as built, as of October 2026. Where a capability is
partial or a follow-up is planned, the relevant chapter says so. The main open items:*

- *chaining-aware indexes, column paths and match fan-out (Chapter 17);*
- *a persisted Watch epoch for mutable collections (Chapter 16);*
- *dropping superseded delta fragments at seal (Chapter 15);*
- *grouping remnants for cross-record compression (Chapter 9);*
- *encryption and snapshot coverage for archive tables (Chapter 22).*

*Corrections and questions from the HTCondor community are the point of circulating it.*
