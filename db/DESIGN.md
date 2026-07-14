# ClassAd DB: embedded + client/server

Two standalone modules layered on the `collections` store (which already provides
memory-dense storage, indexing, wire-native match, and multi-writer MVCC
transactions). Each has its own `go.mod` so it can pull in additional dependencies
(notably CEDAR) without imposing them on `classad`/`collections`.

## Module 1 — embedded DB (this directory: `db/`) — SHIPPED

A Go core (`package db`) plus a cgo C-archive (`capi/`) that exports C symbols
mirroring HTCondor's `classad_log.h`, so a C++ interface built on `libcondor_utils`
sits on top. `go build -buildmode=c-archive ./capi` emits `libclassad_db.a` +
`capi.h`.

### Data model

`classad_log.h` is a `key -> ClassAd` table with an on-disk transaction log and four
operations (`LogNewClassAd`, `LogDestroyClassAd`, `LogSetAttribute`,
`LogDeleteAttribute`). This maps 1:1 onto a `collections.Collection` + `Txn`:

| classad_log.h            | db (Go)                         | collections            |
|--------------------------|---------------------------------|------------------------|
| the hash table           | `DB` / `Collection`             | key -> wire ad         |
| `BeginTransaction`       | `DB.Begin() *Txn`               | `Collection.Begin()`   |
| `NewClassAd`             | `Txn.NewClassAd(key, ad)`       | `Txn.Put`              |
| `DestroyClassAd`         | `Txn.DestroyClassAd(key)`       | `Txn.Delete`           |
| `SetAttribute`           | `Txn.SetAttribute(key,n,expr)`  | read-modify-write buf  |
| `DeleteAttribute`        | `Txn.DeleteAttribute(key,n)`    | read-modify-write buf  |
| `LookupInTransaction`    | `Txn.LookupAttr` / `LookupClassAd` | `Txn.Get` (RYW)     |
| the table lookup         | `DB.LookupClassAd`              | `Collection.Get`       |
| `CommitTransaction`      | `Txn.Commit() error`            | `Txn.Commit()` (OCC)   |
| `AbortTransaction`       | `Txn.Abort()`                   | drop the buffer        |

### Beyond classad_log.h

- **Multiple independent transactions.** `classad_log.h` allows only one active
  transaction; `DB.Begin` returns an independent `*Txn`, any number live at once,
  with snapshot-isolation write-write conflict detection between them
  (`Commit` returns `*ConflictError{Keys}` — the losers to retry; the rest committed).
  The C++ layer may still serialize if it wants, but the library does not require it.
- **Per-ad partial commit.** A large transaction (a constraint scan that edits many
  ads) commits each ad independently — matching how the schedd actually uses large
  transactions. No all-or-nothing rollback.

### C surface (capi/, cgo)

Opaque `uintptr_t` handles (`runtime/cgo.Handle`) for DB and transaction; C never
dereferences them. Returned strings are C-allocated, freed with `cadb_free`.
`cadb_open/close/begin/commit/abort/new_classad/destroy_classad/set_attribute/
delete_attribute/lookup_attr`. `cadb_commit` returns the conflicted-key count (0 =
success). Verified by a C smoke test linking the archive.

### Planned: zero-alloc PutClassAd via wire bytes

`cadb_new_classad` currently takes old-ClassAd text and parses it. The zero-alloc
path: add `collections.PutWire(key, wireBytes)` that stores pre-encoded inline-wire
bytes directly (no decode/re-encode), and `cadb_new_classad_wire(tx, key, ptr, len)`.
The C++ side serializes its ClassAd straight to the collections inline-wire format
(self-contained, no intern table) — or converts its CEDAR wire form to old-ClassAd
expression bytes without materializing a ClassAd. Requires publishing the
inline-wire format as a stable contract.

## Module 2 — client/server DB (planned: `dbrpc/`)

A CEDAR-based client/server so remote processes use the same log. Depends on
`collections`, `db`, and `github.com/bbockelm/cedar`.

### Wire protocol (raw bytes, no ClassAd framing)

Messages are built from raw bytes to avoid per-RPC allocation — not ClassAd-framed.
A request/response is a small fixed header + opaque payload:

```
request:  [u64 reqID][u8 op][u32 keyLen][key][u32 payloadLen][payload]
response: [u64 reqID][i32 status][u32 payloadLen][payload]
```

Ops: BEGIN, COMMIT, ABORT, NEW_AD, DESTROY_AD, SET_ATTR, DELETE_ATTR, LOOKUP_AD,
LOOKUP_ATTR, SCAN. Payloads are wire ads / expression bytes.

### No head-of-line blocking (out-of-order responses)

Each RPC carries a unique `reqID`; the server may answer in any order. On one CEDAR
connection: a writer goroutine muxes framed requests; a reader goroutine demuxes
responses by `reqID` to the waiting caller's channel. A single slow op (a big scan)
does not stall others on the same connection. This is the `database/sql` idea: the
connection is a mux, not a lock.

### Connection pool (database/sql-inspired)

The client holds a pool of CEDAR connections (`min`/`max`, idle reaping). A call
checks out a connection, sends its `reqID`, awaits its response, returns the
connection. Transactions pin one connection for their lifetime (a `Tx` bound to a
`Conn`, like `sql.Tx`), since a transaction handle is server-side state. Independent
transactions use independent connections — which the multi-writer OCC core already
supports.

### Clients

- **Go client**: `Open(addr) *Client`, `Begin() *Tx`, the same op set as `db`,
  returning `*ConflictError` on commit conflicts.
- **C++ client**: the framing is raw bytes over CEDAR, so a thin C/C++ client on
  `libcondor_utils` (which already speaks CEDAR) implements the same header +
  `reqID` demux. No golang-htcondor dependency; new CEDAR helper methods
  (send/recv raw framed bytes) are added as needed.

### Open questions

- Server-side transaction lifetime / GC of abandoned transactions (idle timeout).
- Scan/streaming: a SCAN response streams multiple frames under one `reqID`, or
  paginates with a cursor.
- Auth/authz: CEDAR provides the secure channel; map its authenticated identity to
  per-key access policy (out of scope for v1).
