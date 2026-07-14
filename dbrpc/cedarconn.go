package dbrpc

import (
	"context"

	"github.com/bbockelm/cedar/stream"
)

// cedarConn adapts a CEDAR stream to MsgConn: each dbrpc message is one CEDAR
// message. Crypto and framing are CEDAR's; dbrpc adds only the request-id mux on top.
type cedarConn struct {
	s   *stream.Stream
	ctx context.Context
}

// NewCedarConn wraps an established CEDAR stream (from a client dial, or a server
// HandlerFunc's Conn.Stream) as a dbrpc transport. ctx bounds the connection's I/O.
func NewCedarConn(ctx context.Context, s *stream.Stream) MsgConn {
	return &cedarConn{s: s, ctx: ctx}
}

func (c *cedarConn) WriteMsg(b []byte) error { return c.s.SendMessage(c.ctx, b) }

func (c *cedarConn) ReadMsg() ([]byte, error) {
	// A dbrpc message is one CEDAR SendMessage, but read defensively across frames to
	// end-of-message; ReadFrame reuses its buffer, so copy each frame.
	var msg []byte
	for {
		data, eom, err := c.s.ReadFrame(c.ctx)
		if err != nil {
			return nil, err
		}
		msg = append(msg, data...)
		if eom {
			return msg, nil
		}
	}
}

func (c *cedarConn) Close() error {
	if conn := c.s.GetConnection(); conn != nil {
		return conn.Close()
	}
	return nil
}
