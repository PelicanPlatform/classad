package dbrpc

import (
	"context"
	"encoding/binary"
)

// Request extensions: per-request metadata that rides after a request's body.
//
// Bit 7 of the op byte marks a request carrying an extension block, so opcodes are
// limited to 0x00-0x7f. The block is a trailer:
//
//	[reqID u64][op|0x80 u8][body ...][entry]...[entry][extLen u16 LE]
//	entry = [type u8][len u8][value]
//
// where extLen counts the entry bytes and not itself. A trailer rather than a prefix to
// the body means the client appends to a frame its builder already produced, and the
// server hands each handler a body identical to the unflagged form.
//
// Every entry type is ADVISORY: it may change how the server records or schedules a
// request, never what the request returns. That is what makes it safe for the server to
// skip a type it does not know. Anything that changes results is an opcode or a request
// flag, which a server refuses when it does not know them.
//
// Only requests carry extensions; response frames are unchanged.

// opExtFlag is the op-byte bit marking a request with an extension block.
const opExtFlag = 0x80

// Extension entry types. The values are protocol: a released number never changes
// meaning.
const (
	// extTraceContext is a W3C trace context in binary form: trace ID (16 bytes), span
	// ID (8), trace flags (1).
	extTraceContext = 1

	// 2 is reserved for a request deadline (i64 nanoseconds remaining, relative to
	// receipt), so the server can stop work whose caller has already given up.
)

const traceContextLen = 16 + 8 + 1

// maxExtendedFrame is the largest request frame the client will extend. The CEDAR stream
// refuses a message over its MaxMessageSize (1 MiB), and an extension is advisory, so it
// must never be what pushes a request over. A frame this size or smaller has at least 4 KiB
// of room left, the same margin bulk writes keep (maxBatchFrameBytes); a larger one is sent
// without its extension.
const maxExtendedFrame = maxBatchFrameBytes

// TraceContext is a W3C trace context in binary form: the identity of the caller's span,
// carried with a request so the server's work can be recorded as its child.
//
// The fields are laid out as OpenTelemetry's trace.TraceID, trace.SpanID and
// trace.TraceFlags are, so converting in either direction copies arrays and allocates
// nothing. dbrpc does not import a tracing library; the embedder converts at the edge.
type TraceContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Flags   byte // W3C trace-flags; bit 0 is "sampled"
}

// IsValid reports whether t identifies a span: W3C treats an all-zero trace ID or span
// ID as absent.
func (t TraceContext) IsValid() bool {
	return t.TraceID != [16]byte{} && t.SpanID != [8]byte{}
}

// Sampled reports whether the caller chose to record this trace.
func (t TraceContext) Sampled() bool { return t.Flags&0x01 != 0 }

type traceContextKey struct{}

// ContextWithTraceContext returns ctx carrying t, as the server does for each request that
// arrives with a trace context.
func ContextWithTraceContext(ctx context.Context, t TraceContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, t)
}

// TraceContextFromContext returns the trace context carried by ctx, if any.
//
// On a server it reports what the request arrived with. Its signature matches
// WithTraceContext's argument, so a process that both serves and calls dbrpc can forward
// an incoming request's trace to its own calls with
// WithTraceContext(TraceContextFromContext) and no tracing library.
func TraceContextFromContext(ctx context.Context) (TraceContext, bool) {
	t, ok := ctx.Value(traceContextKey{}).(TraceContext)
	return t, ok
}

// WithTraceContext makes the client attach the caller's trace context to each request.
//
// fn is called once per request with the context the caller passed. The client attaches
// what it returns only when the context is valid and SAMPLED, and only when the server
// advertises FeatFrameExt; otherwise the request is sent exactly as it would be without
// this option. Sending only sampled contexts is enforced here rather than left to fn, so
// that an unsampled request costs no bytes and no allocation whatever fn does.
//
// fn runs on the caller's goroutine before the request is written, and must not block.
func WithTraceContext(fn func(context.Context) (TraceContext, bool)) ClientOption {
	return func(c *Client) { c.traceFn = fn }
}

// appendTraceExt flags frame as extended and appends a block holding t. frame must be a
// complete request frame without an extension block.
func appendTraceExt(frame []byte, t TraceContext) []byte {
	frame[8] |= opExtFlag
	frame = append(frame, extTraceContext, traceContextLen)
	frame = append(frame, t.TraceID[:]...)
	frame = append(frame, t.SpanID[:]...)
	frame = append(frame, t.Flags)
	return binary.LittleEndian.AppendUint16(frame, 2+traceContextLen)
}

// frameExt is what a request's extension block said. The zero value is a request with no
// block.
type frameExt struct {
	trace    TraceContext
	hasTrace bool

	// malformed is a flagged request whose trailer does not fit the frame. Its body
	// boundary is unknown, so it is answered as a bad request rather than served.
	malformed bool
}

// splitExt separates a flagged request's payload (everything after the op byte) into its
// body and its extension entries, and parses the entries. ok is false when the trailer
// does not fit: the frame is malformed, and the body boundary cannot be known.
//
// Entries of an unknown type are skipped, and an entry of a known type with the wrong
// length is ignored; both are advisory, so neither fails the request. A repeated type
// takes the last value.
func splitExt(payload []byte) (body []byte, ext frameExt, ok bool) {
	if len(payload) < 2 {
		return nil, frameExt{}, false
	}
	n := int(binary.LittleEndian.Uint16(payload[len(payload)-2:]))
	end := len(payload) - 2
	if n > end {
		return nil, frameExt{}, false
	}
	body, entries := payload[:end-n], payload[end-n:end]
	for len(entries) > 0 {
		if len(entries) < 2 {
			return nil, frameExt{}, false
		}
		typ, l := entries[0], int(entries[1])
		entries = entries[2:]
		if l > len(entries) {
			return nil, frameExt{}, false
		}
		val := entries[:l]
		entries = entries[l:]
		if typ == extTraceContext && l == traceContextLen {
			var t TraceContext
			copy(t.TraceID[:], val[:16])
			copy(t.SpanID[:], val[16:24])
			t.Flags = val[24]
			if t.IsValid() {
				ext.trace, ext.hasTrace = t, true
			} else {
				ext.trace, ext.hasTrace = TraceContext{}, false
			}
		}
	}
	return body, ext, true
}

// TracedRequest describes a request that arrived with a trace context, for
// ServeOptions.TraceRequest.
//
// It names the operation and the peer and nothing about the request's arguments: a
// constraint can name private attributes and their values, and a span is exported to
// wherever traces go. QueryLog carries the arguments for operators who opt in to it.
type TracedRequest struct {
	Op    string       // the opcode's name, as in QueryLog.Op
	Peer  string       // ServeOptions.Peer
	Trace TraceContext // the caller's span
}
