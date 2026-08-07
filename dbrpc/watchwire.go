package dbrpc

import (
	"context"
	"errors"
	"sync"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// The change feed in WIRE FORM rather than ClassAd text.
//
// A watch event's ad leaves storage as wire bytes, is decoded to a ClassAd by the watch
// layer, rendered to text here, and parsed back by every consumer -- and a change feed has
// many consumers, each paying that parse on every event forever. Shipping the wire form
// instead costs the server slightly less (encode rather than render) and costs the consumer
// about a sixth (see BenchmarkWatch* in collections/wire).
//
// This is an additive opcode: the text watch is unchanged, and a client whose server does
// not know the new one falls back to it.

// ErrWatchWireUnsupported reports that the server does not implement the wire-form watch,
// so the caller should use WatchTable. It is returned before any event.
var ErrWatchWireUnsupported = errors.New("dbrpc: server does not support the wire-form watch")

// WireWatchEvent is a WatchEvent whose ad is the wire form. Ad is nil for a delete or a
// reset, exactly as AdText is empty for those.
type WireWatchEvent struct {
	Kind   uint8 // 0 upsert, 1 delete, 2 reset (see db.WatchKind)
	Key    string
	Ad     []byte // wire-form ad; decode with DecodeWatchAd
	Cursor []byte
}

// DecodeWatchAd turns a wire-form watch ad into a ClassAd. It returns nil for an event
// carrying no ad (a delete or a reset).
func DecodeWatchAd(w []byte) (*classad.ClassAd, error) {
	if len(w) == 0 {
		return nil, nil
	}
	node, err := wire.DecodeInline(w)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(node), nil
}

// WatchWire is WatchWireTable on the default table.
func (c *Client) WatchWire(ctx context.Context, cursor []byte) (<-chan WireWatchEvent, func(), error) {
	return c.WatchWireTable(ctx, DefaultTable, cursor)
}

// WatchWireTable is WatchTable with each event's ad in wire form. Against a server that
// does not implement the opcode it returns ErrWatchWireUnsupported, before any event, and
// the caller falls back to WatchTable.
func (c *Client) WatchWireTable(ctx context.Context, table string, cursor []byte) (<-chan WireWatchEvent, func(), error) {
	id, frames, err := c.callStream(func(rid uint64) []byte {
		return putBytes(putStr(req(rid, opWatchWire), table), cursor)
	})
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_, _, _ = c.callCtx(context.Background(), func(rid uint64) []byte { return putU64(req(rid, opWatchStop), id) })
		})
	}
	// A server too old to know the opcode rejects the request rather than streaming, so the
	// rejection arrives as the first frame. Read it here, before handing back a channel, so
	// the caller learns it needs the text watch synchronously instead of seeing a stream
	// that simply never produces anything.
	select {
	case <-ctx.Done():
		stop()
		drain(frames)
		return nil, nil, ctx.Err()
	case first, ok := <-frames:
		if !ok {
			return nil, nil, ErrWatchWireUnsupported
		}
		_, status, body, hok := respHeader(first)
		if !hok {
			stop()
			drain(frames)
			return nil, nil, errShort
		}
		switch status {
		case stBadReq:
			drain(frames)
			return nil, nil, ErrWatchWireUnsupported
		case stErr:
			drain(frames)
			return nil, nil, statusErr(status, body)
		}
		events := make(chan WireWatchEvent, 64)
		go func() {
			defer close(events)
			// The frame already read is the stream's first event; deliver it, then continue.
			if status == stStream {
				if !deliverWireWatch(ctx, events, body, stop, frames) {
					return
				}
			}
			for {
				select {
				case <-ctx.Done():
					stop()
					drain(frames)
					return
				case frame, ok := <-frames:
					if !ok {
						return
					}
					_, st, b, ok := respHeader(frame)
					if !ok || st != stStream {
						continue
					}
					if !deliverWireWatch(ctx, events, b, stop, frames) {
						return
					}
				}
			}
		}()
		return events, stop, nil
	}
}

// deliverWireWatch parses one event frame and sends it, reporting whether to keep going.
func deliverWireWatch(ctx context.Context, events chan<- WireWatchEvent, body *reader, stop func(), frames <-chan []byte) bool {
	ev := WireWatchEvent{Kind: body.u8()}
	ev.Key = body.str()
	ev.Ad = append([]byte(nil), body.bytesRef()...)
	ev.Cursor = append([]byte(nil), body.bytesRef()...)
	select {
	case events <- ev:
		return true
	case <-ctx.Done():
		stop()
		drain(frames)
		return false
	}
}

// watchAdWire encodes a watch event's ad for the wire, dropping private attributes unless
// the connection is privileged -- the same rule adString applies to the text form. A
// private attribute must not reach an unprivileged watcher just because the encoding
// changed.
func watchAdWire(ad *classad.ClassAd, includePrivate bool) []byte {
	if ad == nil {
		return nil
	}
	node := ad.AST()
	if node == nil {
		return nil
	}
	if !includePrivate {
		kept := make([]*ast.AttributeAssignment, 0, len(node.Attributes))
		for _, a := range node.Attributes {
			if classad.IsPrivateAttribute(a.Name) {
				continue
			}
			kept = append(kept, a)
		}
		if len(kept) != len(node.Attributes) {
			node = &ast.ClassAd{Attributes: kept}
		}
	}
	return wire.EncodeInline(nil, node)
}
