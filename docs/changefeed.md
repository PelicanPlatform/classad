# Change Feed — transport-neutral, ACK-gated DB replication over HTTP

**Status:** design (pre-implementation)
**Package home:** `classad/changefeed` (new), consumed by htcondordb and by external sink projects
**Author:** (design)

---

## 1. Motivation & scope

htcondordb already replicates **htcondordb → htcondordb** over CEDAR — leader-follower
(`WatchTable` streams over a `DBSession`) and consistent/raft. **Those stay exactly as they are.**
This design does *not* touch them.

What's missing is a **toolkit for a standalone, non-CEDAR project** — possibly another language,
minimal dependencies — to:

- consume a continuous, **resumable**, **selective** change feed from one or more htcondordb
  **sources** over plain HTTP, and
- write the result into its own **`classad/db`** store (an "htcondordb" store — the same on-disk
  format), i.e. **fan-in / import multiple archives from several sources**,
- while letting each source **GC/rotate** safely, because every destination periodically **ACKs**
  the watermark it has durably persisted.

### In scope
- A transport-neutral (no CEDAR) HTTP change-feed **protocol** (NDJSON over SSE + a small ACK POST).
- A `classad/changefeed` package: the **source** handler (+ durable subscriber registry + GC-floor
  hook) and the **sink** client (+ ACK sender + apply-into-`classad/db`).
- htcondordb mounting the source handler and wiring the GC floor into archive retention.

### Out of scope
- Replacing CEDAR replication (unchanged).
- A general-purpose message broker. This is DB-change-feed-shaped, tuned for append-only archives.
- Exactly-once. We provide **at-least-once + idempotent apply**.

---

## 2. Roles & topology

```
   ┌─────────────┐   SSE: GET /subscribe (NDJSON events)     ┌──────────────────────────┐
   │  SOURCE     │ ────────────────────────────────────────▶ │  DESTINATION / SINK      │
   │ htcondordb  │                                            │  standalone, non-CEDAR   │
   │ (archive)   │ ◀──────────────────────────────────────── │  embeds classad/db       │
   └─────────────┘   POST /ack {subscriber, watermark}        └──────────────────────────┘
        │  retains data until min(ACK over all subscribers); GC below the floor
        │  (multiple concurrent sinks = multiple durable subscriptions)
```

- **Source** — an htcondordb (or any `classad/db` service) that *exposes* a table/archive's change
  stream over HTTP and coordinates GC with its subscribers.
- **Destination / Sink** — the standalone project. Pulls the SSE stream, applies events into its
  embedded `classad/db` catalog, and periodically ACKs how far it has durably persisted. **No CEDAR.**
- **Fan-in** — one sink runs N subscriptions (one per source). **Multiple concurrent sinks per
  source** are supported: each is an independent durable subscription with its own cursor + ACK.

---

## 3. Layering (what lives where)

The transport-neutral apply core lives in the **db module** so every transport builds on it without
depending on any other transport. The HTTP/SSE feed and a future CEDAR replicator are siblings over
`db/replicate`; neither imports the other.

```
classad/db/replicate  (the apply core; depends only on classad + db; NO net/http, NO CEDAR)
├── change.go       Change (a db.WatchEvent + {Src,Ver,TS}); ChangeFromWatch
└── sink.go         Sink + NewArchiveSink (append-only, at-least-once) / NewTableSink (LWW);
                    CursorStore (Mem/File); stamps Src

classad/changefeed    (the HTTP/SSE transport over db/replicate; adds net/http + collections/vm)
├── event.go        NDJSON wire Event{Kind,Src,Ver,Key,Ad,Cursor,TS}; Change<->Event codec
├── source.go       http.Handler over db.Watch: SSE /subscribe (constraint/project/heartbeat) + /ack
├── registry.go     durable per-subscriber ack watermark + lease; GC-floor computation
└── client.go       reconnecting Pull: resume cursor, periodic ACK (feeds a db/replicate.Sink)

htcondordb          mounts changefeed.Handler on its HTTP listener (token-gated); feeds the
                    subscriber GC floor into archive/table retention. A native-CEDAR selective /
                    fan-in replicator reuses db/replicate.Sink over dbrpc.WatchTable -- no changefeed
                    dependency. (Existing CEDAR replication paths untouched.)

sink project        embeds changefeed.Sink + classad/db. Imports classad ONLY. Fan-in = N pullers.
```

The **only** thing the standalone sink needs is `classad/changefeed` + `classad/db`. It never links
CEDAR, dbrpc, or htcondordb.

---

## 4. Wire format — NDJSON change events

One JSON object per line (SSE `data:` payload is one event). `ad` present only on upserts.

```json
{"kind":"upsert","src":"ap40","ver":812,"key":"12.0","ad":{"Owner":"alice","JobStatus":4},"cursor":"eyJ...=="}
{"kind":"delete","src":"ap40","ver":813,"key":"9.0","cursor":"eyJ...=="}
{"kind":"reset","src":"ap40","cursor":"eyJ...=="}
{"kind":"synced","src":"ap40","cursor":"eyJ...=="}
{"kind":"gap","src":"ap40","fromMillis":1700000000000,"toMillis":1700086400000}
```

| field   | meaning |
|---------|---------|
| `kind`  | `upsert \| delete \| reset \| synced \| gap` — 1:1 map of `db.Watch` kinds (+ `gap` advisory). |
| `src`   | source identity, set by the server from auth/config (so a fan-in sink can stamp/route/dedup). |
| `ver`   | strictly monotonic **per-source** version (the exporter `ExportSeq` pattern) — the idempotency key. |
| `key`   | source storage key (addressed the `QueryKeys` way — never a self-reported attribute). |
| `ad`    | the record as JSON (upsert only); projected if `project=` was requested. |
| `cursor`| opaque resume/ACK token (base64 of the `db.Watch` cursor). |

- `reset` — discard derived state for `src`; a fresh snapshot follows (first sync, or retention gap).
- `synced` — caught up to head; `cursor` is a safe resume/ACK point.
- `gap` — **advisory**: records lost to source retention before this sink caught up (like the
  archivedropbox loss report). Not silently swallowed.

Rationale for NDJSON: language-neutral and self-describing, so a non-Go sink parses it trivially.

---

## 5. HTTP protocol

### 5.1 Subscribe (SSE stream, source → sink)
```
GET /changefeed/v1/subscribe
    ?table=history               # source table or archive (required)
    &subscriber=<stable-id>      # durable subscription id (required; identifies this sink)
    &cursor=<opaque>             # resume point; empty = from the source's current floor; "@begin" = full replay
    &constraint=<classad expr>   # optional server-side filter (selective)
    &project=Attr1,Attr2         # optional attribute projection (trim payload)
Accept: text/event-stream
Authorization: Bearer <token>    # non-CEDAR auth; token → src label + authorization

200 text/event-stream:
    : heartbeat                  # comment lines keep idle connections alive
    data: {"kind":"upsert",...}
    data: {"kind":"synced","cursor":"..."}
    ...                          # unbounded until the client disconnects
```

### 5.2 ACK (small POST, sink → source) — **the GC signal**
```
POST /changefeed/v1/ack
Authorization: Bearer <token>
{"subscriber":"central-sink-1","table":"history","ack":"<cursor>"}

200 {"floor":"<cursor>","retainedFromMillis":...}   # the source's current global GC floor, for observability
```
- `ack` = "I have **durably persisted** every event up to and including this cursor; you need not
  retain anything at or before it **on my behalf**."
- Sent **periodically** (e.g. every few seconds / every N events) and on clean shutdown.
- The ACK cursor is the *same* value the sink will pass as `cursor=` on reconnect (its durable
  commit point serves both resume and GC-floor).

### 5.3 (Optional) explicit lifecycle
- Auto-register on first `subscribe`/`ack`. `DELETE /changefeed/v1/subscriber?subscriber=<id>` to
  retire a subscription and release its retention hold immediately. (Otherwise the lease TTL reclaims
  it — see §7.)

---

## 6. Delivery semantics (at-least-once, idempotent)

1. Sink subscribes from its stored `cursor` (empty / `@begin` first run).
2. For each event: **apply** to the target `classad/db`, advance the in-memory cursor.
3. Periodically (and on `synced`) **commit** the cursor durably (sink-side), **then** `ack` it.
4. On disconnect: reconnect from the last committed cursor → the tail re-delivers → **idempotent
   apply** makes replays no-ops:
   - **archive sink** (the primary case): dedup by `(src, ver)` — a re-append is skipped.
   - **mutable sink**: external-version upsert — keep the highest `ver` per key; a stale `ver` is ignored.
5. `reset` → drop the sink's derived state for that `src`, rebuild from the following snapshot.
6. `gap` → record the lost window; surface it (metric + a loss ClassAd), don't hide it.

Ordering: events for a given `src` are delivered in commit order; `ver` is monotonic within `src`.

---

## 7. Durable subscriptions & ACK-gated GC  *(the core new mechanism)*

The source keeps a **durable subscriber registry** (survives source restart): per `(table,
subscriber)` it stores the **ACK watermark** and a **lease**.

- **GC floor** for a table = `min(ack)` over its **live** subscribers. The source must not GC/rotate
  data at or below the floor.
- **Multiple concurrent readers**: each subscriber contributes its own ack; the slowest live reader
  holds the floor. A new/lagging reader that needs data already GC'd receives a `reset` (+ `gap`).

### 7.1 Archives make this cheap (why this fits the user's case)
For an **append-only archive**, the archive's **own retained records _are_ the durable log** — a
subscriber resuming at cursor `C` simply reads archive records after `C`. So the change feed needs
**no separate change-log storage**: the ACK floor becomes just **another input to the archive's
existing retention** (rotation keeps `max(configured retention, data-needed-by-slowest-live-ACK)`).
This bounds storage by data you already keep and is the natural fit for "import multiple archives."

For a **mutable table**, retain-until-ACK would require unbounded watch-history retention. So mutable
feeds instead use **resync-on-fall-behind** (standard `Watch` semantics: a slow reader gets `reset`),
and the ACK is advisory only. **Recommendation:** ship the full ACK/GC contract for **archives**;
mutable-table feeds are resumable-but-resync (no hard GC gating).

### 7.2 Liveness / dead-reader eviction (bounding storage)
Each subscriber has a **lease TTL** (config, e.g. 1h). An active SSE connection or an `ack` renews
it. If a subscriber goes silent past its TTL, the source **evicts** it — drops its floor
contribution so GC proceeds. On return, an evicted subscriber that resumes from a GC'd cursor gets a
`reset` (+ `gap`). This prevents one dead sink from pinning source storage forever; the lease is the
knob that trades "how long a sink may be offline" against "max retention a stalled sink can force."

---

## 8. Selectivity
Server-side `constraint` (a ClassAd expression evaluated per event; non-matching events skipped) and
`project` (emit only the named attributes). Keeps the payload — and the sink's store — to what the
subscriber actually wants.

---

## 9. Toolkit API sketch (`classad/changefeed`)

```go
// ---- SOURCE (mounted by htcondordb or any classad/db service) ----

type Authorizer func(r *http.Request) (src string, sub string, ok bool) // token -> identity

type ServerOptions struct {
    Auth       Authorizer
    Registry   Registry        // durable subscriber store (see below)
    LeaseTTL   time.Duration
    Heartbeat  time.Duration
}
func Handler(cat *db.Catalog, opts ServerOptions) http.Handler // serves /subscribe + /ack

// Registry persists per-subscriber ACK + lease and computes the GC floor. A default
// implementation stores rows in a reserved catalog table; a service may supply its own.
type Registry interface {
    Ack(table, subscriber string, cursor []byte, now time.Time) error
    Floor(table string, now time.Time) ([]byte, error) // min live ack; drives retention
    Evict(now time.Time) (int, error)                  // reap expired leases
}

// htcondordb wires Floor() into ArchiveTable retention: rotation keeps everything after Floor().

// ---- SINK (used by the standalone project) ----

type Sink interface {
    Apply(ev Event) error   // idempotent ((src,ver) / external-version)
    Commit(cursor []byte) error
    Cursor() []byte
}
func NewArchiveSink(a *db.ArchiveTable, src string) Sink // (src,ver) dedup
func NewTableSink(d *db.DB, src string) Sink             // external-version upsert

type PullConfig struct {
    URL, Table, Subscriber, Constraint string
    Project    []string
    Token      string
    AckEvery   time.Duration // how often to POST /ack
}
func Pull(ctx context.Context, cfg PullConfig, sink Sink) error // reconnecting, resumable, ACKs
```

The standalone sink is then ~"for each source: `go changefeed.Pull(ctx, cfg, changefeed.NewArchiveSink(archive, src))`".

---

## 10. htcondordb integration
- Mount `changefeed.Handler(svc.Catalog(), …)` on the daemon HTTP listener (the existing
  `HTCONDORDB_METRICS_ADDRESS` mux or a dedicated `HTCONDORDB_CHANGEFEED_ADDRESS`), **token-gated**.
- Provide the default `Registry` (a reserved catalog table) and wire its `Floor()` into each
  archive's retention so rotation respects live subscribers.
- Config: enable flag, listen address, token(s) → `src`, `LeaseTTL`, per-table opt-in.
- **CEDAR replication paths are untouched.**

## 11. Standalone sink project (separate)
- Config: list of `{source URL, table, subscriber-id, target table, token, constraint/project}`.
- Starts one `changefeed.Pull` per source into an embedded `classad/db` catalog (per-source archive
  or one shared table with `Src` stamped — a naming choice, same mechanism).
- Exposes its own query surface. Imports **classad only**; no CEDAR anywhere.

---

## 12. Phasing
1. `classad/changefeed`: `event` codec + `Pull` client + `NewArchiveSink` (dedup) + a **round-trip
   test** (in-process source `Watch` → NDJSON → archive sink; resume-after-disconnect proven).
2. `classad/changefeed`: `Handler` (SSE `/subscribe`, constraint/project, heartbeats) + `/ack` +
   default `Registry` + GC-floor; server↔client round-trip + eviction test.
3. htcondordb: mount handler, token auth, wire `Floor()` into archive retention, config, docs.
   → htcondordb-as-source works end to end; a `curl`-able feed for external sinks.
4. Standalone sink project scaffold → **fan-in demo** (2 sources → 1 sink store), ACK-drives-GC test.
5. (later) mutable-table feeds (resync-on-slow); mTLS auth; snapshot bootstrap for `@begin`.

---

## 13. Open questions / risks
- **`ver` for mutable tables** with no `ExportSeq`: synthesize a per-feed monotonic counter at the
  server, or rely on cursor ordering + last-write-wins. (Archives already carry the export seq.)
- **Registry durability & HA**: where the subscriber table lives; how it behaves under raft/leader-
  follower (a follower shouldn't independently GC). Likely: only the primary serves feeds / owns the
  registry.
- **`@begin` snapshot bootstrap** for a huge archive: chunked snapshot before switching to live tail.
- **Auth**: bearer token per source (token → `src`) first; mTLS (cert CN → `src`) later.
- **Storage risk**: a slow-but-leased sink forces retention growth; the lease TTL bounds it — pick a
  sane default and expose it.
- **Clock/lease**: eviction uses wall-clock; document the lease as "max offline before a forced
  reset", not a correctness knob (idempotent apply keeps correctness regardless).
- **Backpressure**: TCP flow-control throttles the SSE writer; a persistently slow archive sink
  simply holds the GC floor (bounded by lease) rather than being force-resynced — the deliberate
  difference from raw `Watch`.
```
