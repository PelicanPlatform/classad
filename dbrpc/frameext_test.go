package dbrpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
	"github.com/htcondor/cedar/stream"
)

func sampledTrace() TraceContext {
	var t TraceContext
	for i := range t.TraceID {
		t.TraceID[i] = byte(i + 1)
	}
	for i := range t.SpanID {
		t.SpanID[i] = byte(0xa0 + i)
	}
	t.Flags = 0x01
	return t
}

// recordConn copies every frame the client writes, so a test can read the wire.
type recordConn struct {
	MsgConn
	mu     sync.Mutex
	frames [][]byte
}

func (r *recordConn) WriteMsg(b []byte) error {
	r.mu.Lock()
	r.frames = append(r.frames, append([]byte(nil), b...))
	r.mu.Unlock()
	return r.MsgConn.WriteMsg(b)
}

func (r *recordConn) written() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.frames...)
}

// tracePair serves a fresh in-memory database with sopts and returns a client built with
// copts whose writes are recorded.
func tracePair(t *testing.T, sopts ServeOptions, copts ...ClientOption) (*Client, *recordConn) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, sopts) }()
	rec := &recordConn{MsgConn: cconn}
	c := NewClient(rec, copts...)
	t.Cleanup(func() { c.Close(); s.Close(); d.Close() })
	return c, rec
}

// featuresWithout is the server's advertisement minus one feature: a server built before
// that feature existed.
func featuresWithout(drop Feature) map[string]string {
	var feats []Feature
	for _, f := range serverFeatures {
		if f != drop {
			feats = append(feats, f)
		}
	}
	return sessionAttrsFor(feats)
}

// The end-to-end contract: a sampled trace context the client was given reaches the
// server's hook, on the request's context and in the request description, and the hook's
// finish function runs once per request.
func TestTraceContextReachesServer(t *testing.T) {
	type seen struct {
		req   TracedRequest
		onCtx TraceContext
		ok    bool
	}
	var mu sync.Mutex
	var calls []seen
	finished := make(chan struct{}, 16)
	sopts := ServeOptions{
		Peer: "alice@example",
		TraceRequest: func(ctx context.Context, req TracedRequest) func() {
			onCtx, ok := TraceContextFromContext(ctx)
			mu.Lock()
			calls = append(calls, seen{req, onCtx, ok})
			mu.Unlock()
			return func() { finished <- struct{}{} }
		},
	}
	c, rec := tracePair(t, sopts,
		WithPeerSessionAttrs(ServerSessionAttrs()),
		WithTraceContext(TraceContextFromContext))

	want := sampledTrace()
	ctx := ContextWithTraceContext(context.Background(), want)

	// One unary request (dispatched through handle) and one streamed query.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := c.QueryTable(ctx, DefaultTable, "true", 0); err != nil {
		t.Fatalf("QueryTable: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatalf("finish function ran %d times, want 2", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("hook called %d times, want 2 (Abort carried no trace context and must not call it)", len(calls))
	}
	for i, s := range calls {
		if s.req.Trace != want {
			t.Errorf("call %d: Trace = %+v, want %+v", i, s.req.Trace, want)
		}
		if !s.ok || s.onCtx != want {
			t.Errorf("call %d: TraceContextFromContext = %+v, %v; want %+v, true", i, s.onCtx, s.ok, want)
		}
		if s.req.Peer != "alice@example" {
			t.Errorf("call %d: Peer = %q, want the ServeOptions peer", i, s.req.Peer)
		}
		if s.req.Op == "op(unknown)" || s.req.Op == "" {
			t.Errorf("call %d: Op = %q, want the opcode's name", i, s.req.Op)
		}
	}
	if calls[0].req.Op != "Begin" {
		t.Errorf("first traced op = %q, want Begin", calls[0].req.Op)
	}

	flagged := 0
	for _, f := range rec.written() {
		if f[8]&opExtFlag != 0 {
			flagged++
		}
	}
	if flagged != 2 {
		t.Errorf("%d frames carried an extension block, want 2", flagged)
	}
}

// An unsampled request is byte-for-byte what a client without the option sends, and the
// server's hook never hears of it.
func TestUnsampledRequestIsUnchanged(t *testing.T) {
	var called atomic.Bool
	sopts := ServeOptions{TraceRequest: func(context.Context, TracedRequest) func() {
		called.Store(true)
		return nil
	}}
	unsampled := sampledTrace()
	unsampled.Flags = 0
	ctx := ContextWithTraceContext(context.Background(), unsampled)

	traced, trec := tracePair(t, sopts,
		WithPeerSessionAttrs(ServerSessionAttrs()),
		WithTraceContext(TraceContextFromContext))
	plain, prec := tracePair(t, sopts, WithPeerSessionAttrs(ServerSessionAttrs()))

	for _, c := range []*Client{traced, plain} {
		if _, err := c.QueryTable(ctx, DefaultTable, "true", 0); err != nil {
			t.Fatal(err)
		}
	}
	tf, pf := trec.written(), prec.written()
	if len(tf) != 1 || len(pf) != 1 || !bytes.Equal(tf[0], pf[0]) {
		t.Fatalf("unsampled frame differs from an untraced client's:\n traced %x\n plain  %x", tf, pf)
	}
	if called.Load() {
		t.Fatal("hook called for an unsampled request")
	}
}

// A client never sends the flag to a server that did not advertise the block: an older
// server reads op|0x80 as an opcode it does not know.
func TestNoExtensionWithoutAdvertisement(t *testing.T) {
	ctx := ContextWithTraceContext(context.Background(), sampledTrace())
	for name, opts := range map[string][]ClientOption{
		"silent peer":           {WithTraceContext(TraceContextFromContext)},
		"peer without frameext": {WithPeerSessionAttrs(featuresWithout(FeatFrameExt)), WithTraceContext(TraceContextFromContext)},
	} {
		t.Run(name, func(t *testing.T) {
			c, rec := tracePair(t, ServeOptions{}, opts...)
			if c.SupportsNewer(FeatFrameExt) {
				t.Fatal("SupportsNewer(FeatFrameExt) = true; the block postdates advertisement, so silence must be a no")
			}
			if _, err := c.QueryTable(ctx, DefaultTable, "true", 0); err != nil {
				t.Fatal(err)
			}
			for _, f := range rec.written() {
				if f[8]&opExtFlag != 0 {
					t.Fatalf("frame %x carries the extension flag", f)
				}
			}
		})
	}
}

// A flagged frame whose trailer does not fit is answered as a bad request, not dropped:
// the request id is still readable, so the client must not be left waiting.
func TestMalformedExtensionIsBadRequest(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()
	cconn, sconn := netPipe()
	defer cconn.Close()
	go func() { _ = s.ServeConn(sconn) }()

	base := func(id uint64, o op) []byte {
		f := req(id, o)
		f[8] |= opExtFlag
		return f
	}
	cases := map[string][]byte{
		"no trailer":           base(1, opListTables),
		"one byte":             append(base(2, opListTables), 0),
		"extLen overruns":      binary.LittleEndian.AppendUint16(base(3, opListTables), 40),
		"entry overruns":       binary.LittleEndian.AppendUint16(append(base(4, opListTables), extTraceContext, 25, 1, 2), 4),
		"half entry header":    binary.LittleEndian.AppendUint16(append(base(5, opListTables), 7), 1),
		"restore op":           base(6, opRestore),
		"restore chunk op":     base(7, opRestoreChunk),
		"restore end op":       base(8, opRestoreEnd),
		"unary through handle": base(9, opBegin),
	}
	for name, frame := range cases {
		id := binary.LittleEndian.Uint64(frame)
		if err := cconn.WriteMsg(frame); err != nil {
			t.Fatal(err)
		}
		resp, err := cconn.ReadMsg()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		gotID, status, _, ok := respHeader(resp)
		if !ok || gotID != id || status != stBadReq {
			t.Errorf("%s: reply id=%d status=%d ok=%v, want id=%d status=stBadReq", name, gotID, status, ok, id)
		}
	}
}

// A well-formed block is removed from the body whatever it holds, and its entries are
// advisory: unknown types and malformed known ones are passed over, not refused.
func TestSplitExtEntries(t *testing.T) {
	tc := sampledTrace()
	other := tc
	other.SpanID[0] ^= 0xff
	traceEntry := func(t TraceContext) []byte {
		e := []byte{extTraceContext, traceContextLen}
		e = append(e, t.TraceID[:]...)
		e = append(e, t.SpanID[:]...)
		return append(e, t.Flags)
	}
	body := []byte("the body, untouched")
	cases := []struct {
		name      string
		entries   []byte
		wantTrace bool
		want      TraceContext
	}{
		{"empty block", nil, false, TraceContext{}},
		{"trace", traceEntry(tc), true, tc},
		{"unknown type skipped", append([]byte{200, 3, 9, 9, 9}, traceEntry(tc)...), true, tc},
		{"short trace ignored", append([]byte{extTraceContext, 24}, make([]byte, 24)...), false, TraceContext{}},
		{"zero trace ignored", traceEntry(TraceContext{Flags: 1}), false, TraceContext{}},
		{"repeat: last wins", append(traceEntry(tc), traceEntry(other)...), true, other},
	}
	for _, c := range cases {
		payload := append(append([]byte(nil), body...), c.entries...)
		payload = binary.LittleEndian.AppendUint16(payload, uint16(len(c.entries)))
		gotBody, ext, ok := splitExt(payload)
		if !ok {
			t.Errorf("%s: reported malformed", c.name)
			continue
		}
		if !bytes.Equal(gotBody, body) {
			t.Errorf("%s: body = %q, want %q", c.name, gotBody, body)
		}
		if ext.hasTrace != c.wantTrace || ext.trace != c.want {
			t.Errorf("%s: trace = %+v (has %v), want %+v (has %v)", c.name, ext.trace, ext.hasTrace, c.want, c.wantTrace)
		}
	}
}

// appendTraceExt and reqHeaderExt are inverses, and the op the server reads is the op the
// client built, not the flagged byte.
func TestTraceExtRoundTrip(t *testing.T) {
	tc := sampledTrace()
	plain := putStr(req(77, opQueryRaw), "jobs")
	frame := appendTraceExt(append([]byte(nil), plain...), tc)

	id, o, body, ext, ok := reqHeaderExt(frame)
	if !ok || ext.malformed {
		t.Fatalf("ok=%v malformed=%v", ok, ext.malformed)
	}
	if id != 77 || o != opQueryRaw {
		t.Fatalf("id=%d op=%d, want 77, %d", id, o, opQueryRaw)
	}
	if !bytes.Equal(body.b, plain[9:]) {
		t.Fatalf("body = %x, want %x", body.b, plain[9:])
	}
	if !ext.hasTrace || ext.trace != tc {
		t.Fatalf("trace = %+v, want %+v", ext.trace, tc)
	}
	if fo, _ := frameOp(frame); fo != opQueryRaw {
		t.Fatalf("frameOp = %d, want %d", fo, opQueryRaw)
	}
	if len(frame)-len(plain) != 29 {
		t.Fatalf("block is %d bytes, want 29", len(frame)-len(plain))
	}
}

// Restore uploads are handled in the server's read loop, outside the dispatch path, and a
// watch would hold its span open for the life of the subscription, so the client extends
// neither.
func TestUntracedOpsNeverExtended(t *testing.T) {
	c := &Client{traceFn: func(context.Context) (TraceContext, bool) { return sampledTrace(), true }}
	for _, o := range []op{opRestore, opRestoreChunk, opRestoreEnd, opWatch, opWatchWire} {
		f := req(1, o)
		if got := c.attachExt(context.Background(), f); len(got) != len(f) || got[8] != byte(o) {
			t.Errorf("%s was extended", o)
		}
	}
	if got := c.attachExt(context.Background(), req(1, opBegin)); got[8]&opExtFlag == 0 {
		t.Error("Begin was not extended")
	}
}

// A frame near the transport's message limit is sent without its extension: the extension
// is advisory, so it must not be what makes CEDAR refuse a request that fits without it.
func TestFrameNearMessageLimitNotExtended(t *testing.T) {
	c := &Client{traceFn: func(context.Context) (TraceContext, bool) { return sampledTrace(), true }}
	build := func(size int) []byte {
		f := req(1, opQueryRaw)
		return append(f, make([]byte, size-len(f))...)
	}
	if got := c.attachExt(context.Background(), build(maxExtendedFrame)); got[8]&opExtFlag == 0 {
		t.Error("a frame at maxExtendedFrame was not extended")
	} else if len(got) > stream.MaxMessageSize {
		t.Errorf("extended frame is %d bytes, over the CEDAR limit of %d", len(got), stream.MaxMessageSize)
	}
	if got := c.attachExt(context.Background(), build(maxExtendedFrame+1)); got[8]&opExtFlag != 0 {
		t.Error("a frame over maxExtendedFrame was extended")
	}
}

// The cost the design promises: nothing on the unsampled path, nothing to parse a block,
// and nothing to attach one when the frame has room.
func TestExtensionAllocations(t *testing.T) {
	ctx := context.Background()
	unsampled := &Client{traceFn: func(context.Context) (TraceContext, bool) {
		u := sampledTrace()
		u.Flags = 0
		return u, true
	}}
	sampled := &Client{traceFn: func(context.Context) (TraceContext, bool) { return sampledTrace(), true }}
	frame := make([]byte, 0, 128)
	frame = putStr(append(frame, req(1, opQueryRaw)...), "jobs")
	n := len(frame)

	if a := testing.AllocsPerRun(100, func() { _ = unsampled.attachExt(ctx, frame[:n]) }); a != 0 {
		t.Errorf("unsampled attachExt: %v allocs, want 0", a)
	}
	if a := testing.AllocsPerRun(100, func() {
		f := frame[:n]
		f[8] &^= opExtFlag
		_ = sampled.attachExt(ctx, f)
	}); a != 0 {
		t.Errorf("sampled attachExt into a frame with room: %v allocs, want 0", a)
	}
	ext := appendTraceExt(append([]byte(nil), frame[:n]...), sampledTrace())
	if a := testing.AllocsPerRun(100, func() { _, _, _ = splitExt(ext[9:]) }); a != 0 {
		t.Errorf("splitExt: %v allocs, want 0", a)
	}
	plainHdr := testing.AllocsPerRun(100, func() { _, _, _, _ = reqHeader(frame[:n]) })
	extHdr := testing.AllocsPerRun(100, func() { _, _, _, _, _ = reqHeaderExt(ext) })
	if extHdr > plainHdr {
		t.Errorf("reqHeaderExt on an extended frame: %v allocs, plain header %v", extHdr, plainHdr)
	}
}

var opConstRE = regexp.MustCompile(`(?m)^\s+(op[A-Z]\w*)\s+op\s*=\s*(\d+)`)

// Every opcode stays below the extension bit, and every one has a name: the name is what
// the trace hook and the read-only refusal report, and op(unknown) in a span says nothing.
func TestOpcodesFitBelowExtFlagAndAreNamed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if regexp.MustCompile(`_test\.go$`).MatchString(f) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range opConstRE.FindAllStringSubmatch(string(src), -1) {
			v, _ := strconv.Atoi(m[2])
			seen++
			if v >= opExtFlag {
				t.Errorf("%s = %d collides with the extension bit; opcodes must stay below %d", m[1], v, opExtFlag)
			}
			if name := op(v).String(); name == "op(unknown)" {
				t.Errorf("%s (%d) has no name in op.String", m[1], v)
			}
		}
	}
	if seen < 80 {
		t.Fatalf("found only %d opcode declarations; the pattern no longer matches how they are written", seen)
	}
}
