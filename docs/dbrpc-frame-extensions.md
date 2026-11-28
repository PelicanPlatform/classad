# dbrpc request extensions and trace-context propagation

**Status:** design; trace context implemented, deadline reserved
**Package:** `dbrpc`

## Problem

A dbrpc request frame is `[reqID u64][op u8][body]`. Nothing else crosses the wire with a request, so two things a caller's `context.Context` carries are lost at the client:

- **Trace context.** A slow read in a front end (the HTCondor API server, a collector) cannot be followed into the htcondordb server that ran it. The time splits between the caller, the multiplexed connection and the store, and only the first is visible to the caller's tracer.
- **Deadline.** `callCtx` honours the context only on the client: when it ends, the pending entry is dropped and the late reply discarded. The server never learns, and finishes a scan nobody will read.

Both need a per-request slot. Adding one per concern means a protocol change per concern, so this adds one generic slot and defines the trace context as its first entry.

## Constraints

- **No cost when unused.** Most requests are not sampled. Such a request must produce the same bytes as today and allocate nothing extra on either end.
- **No observability dependency in dbrpc.** dbrpc already exposes timing through `Client.Observer` without importing a metrics library; tracing follows the same pattern. OpenTelemetry lives in the embedder (htcondordb, the API server), which converts at the edge.
- **No string metadata.** Key/value string headers allocate on decode, are unbounded, and invite callers to forward baggage, owners or tokens across a trust boundary. Entries are typed and fixed-layout.
- **Mixed versions.** Clients and servers are upgraded independently. A client must never send a frame an older server misreads.

## Wire format

Bit 7 of the op byte (`0x80`) marks a request that carries an extension block. Opcodes are therefore limited to `0x00`-`0x7f`; the highest assigned today is 88.

The block is a trailer, after the body:

```
[reqID u64][op|0x80 u8][body ...][entry]...[entry][extLen u16 LE]

entry = [type u8][len u8][value: len bytes]
extLen = total bytes of all entries (excludes the u16 itself)
```

The server reads the last two bytes, slices the entries off, and hands the handler a body identical to the unflagged form. A trailer, rather than a block between the op and the body, means no frame builder changes and the client never copies a frame to insert bytes ahead of its body; it appends.

Responses are unchanged. Only requests carry extensions.

### Parsing rules

- A frame whose flag is set but whose trailer does not fit (fewer than two bytes after the op, `extLen` overrunning the frame, an entry overrunning `extLen`) is malformed and answered `stBadReq`. The request ID is still known, so the client gets a reply rather than a hang.
- An entry of an unknown type is skipped.
- An entry of a known type with the wrong length is ignored.
- If a type repeats, the last one wins.

Skipping and ignoring are safe because of one rule that every entry type must obey: **an extension is advisory and never changes what a request returns.** It may change how the server records or schedules the work. Anything that changes results stays an opcode or a request flag, and those keep their refuse-if-unknown behaviour (`wireFlagsKnown`).

### Entry types

| Type | Name | Length | Value |
|---|---|---|---|
| 1 | trace context | 25 | trace ID (16) · span ID (8) · trace flags (1) |
| 2 | deadline (reserved) | 8 | i64 nanoseconds remaining, relative to receipt |

Type 1 is the W3C `traceparent` in binary form, without the version byte. A context whose trace ID or span ID is all zero is invalid and ignored.

Type 2 is relative rather than a wall-clock instant so that clock skew between hosts does not matter. It is reserved here so the slot is designed with two users in mind; it is a separate change, because cancelling server-side work early is a behaviour change that deserves its own review. That change has a prerequisite: stream handlers stop on a cancelled context without replying, because today that only happens when the connection is gone. A handler stopped by a deadline must send an error frame instead.

## Negotiation

Servers advertise features in the CEDAR post-authentication ad (`DBProtocolFeatures`). This adds `frameext`, and bumps `ProtocolVersion` to 11.

`frameext` postdates advertisement, so a peer that advertises nothing does not have it (`SupportsNewer` is false). A client attaches a block only when the server named the feature. An older server never sees the flag bit; it would reject an op of 128 or more as an unknown opcode.

## Client API

```go
type TraceContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Flags   byte // W3C trace-flags; bit 0 is "sampled"
}

func WithTraceContext(fn func(context.Context) (TraceContext, bool)) ClientOption
```

`fn` is called once per request with the request's context. The client attaches the entry only when `fn` reports a valid, **sampled** context and the server advertises `frameext`. The sampled-only rule is enforced in dbrpc, not left to `fn`, so the zero-cost path does not depend on each embedder getting it right.

An embedder using OpenTelemetry passes a three-line adapter over `trace.SpanContextFromContext`; `TraceID` and `SpanID` are byte arrays of the same size.

`TraceContextFromContext` has the same signature as `fn`, so a process that both serves and calls dbrpc (a hub forwarding to spokes) can forward an incoming context with `WithTraceContext(dbrpc.TraceContextFromContext)` and no tracing library at all.

A frame larger than 1 MiB − 4 KiB is never extended: CEDAR refuses a message over 1 MiB, and an advisory entry must not be what pushes a request over. Bulk writes already cut their frames at that size. Locating the trailer costs the same at any size: the transport delivers the whole message as one buffer, and the server reads its last two bytes.

Two kinds of request are never extended. Restore uploads (`opRestore`, `opRestoreChunk`, `opRestoreEnd`) are handled inline in the server's read loop, outside the dispatch path where the hook runs. Watches (`opWatch`, `opWatchWire`) last as long as their subscription, often the life of the process, and a span held open that long is held in the tracer that long too.

## Server API

```go
type TracedRequest struct {
	Op    string       // opcode name, as in QueryLog.Op
	Peer  string       // ServeOptions.Peer
	Trace TraceContext
}

// in ServeOptions:
TraceRequest func(ctx context.Context, req TracedRequest) func()
```

For a request that carries a valid trace context, the server stores it on the request's context (readable with `TraceContextFromContext`), then calls `TraceRequest` if set. The returned function is called when the handler returns, after its last frame is written. A request without a trace context costs one branch.

The hook does not get to replace the handler's context. Stream handlers read their context's cancellation as the connection closing and return without a terminator, so a context the hook could cancel on its own would leave the client waiting forever. The context handlers receive differs from the connection's only by the trace-context value.

An embedder using OpenTelemetry starts a server span there with `trace.ContextWithRemoteSpanContext` and ends it in the returned function. Because the span's context does not reach the handler, dbrpc-internal child spans would need a different hook; none exist today. Its sampler should be `ParentBased(NeverSample())` for dbrpc spans: the client sends only sampled contexts, so a request without one is unsampled, and a root sampler on the server would record orphan spans for requests the caller chose not to trace.

The hook sees the opcode and peer only. It does not see the table or constraint: a constraint can name private attributes and values, and putting it in a span exports it to wherever traces go. `QueryLog` already carries both for operators who opt in to it.

## Cost

| Path | Wire | Client | Server |
|---|---|---|---|
| not sampled, or `fn` unset, or peer lacks `frameext` | unchanged | one call to `fn`, or none | one bit test |
| sampled | +29 bytes | append to the frame; reallocates only when the builder left under 29 bytes of capacity | trailer parse (no allocation), `context.WithValue`, plus whatever the hook allocates |

Building `TraceContext` from an OpenTelemetry `SpanContext`, and back, copies arrays and allocates nothing. The allocations on the sampled path are the context value and the embedder's span.

## Not covered

- **Span status.** The hook's finish function takes no argument, so a span cannot record whether the request failed. Responses are written from many places through the connection's writer; capturing the status means wrapping that writer for traced requests. A later change, once there is a span to put it on.
- **Trace IDs in `QueryLog`.** Stamping the trace ID onto each query log record would let a slow-query line be looked up in the tracing backend. Every query handler builds its own record, so this touches each of them.
- **The CEDAR leg to HTCondor daemons.** A trace that reaches the schedd ends at the client span around the call; the C++ daemons do not take a trace context.
