package dbrpc

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
)

// MsgConn is a reliable, ordered, bidirectional message transport: each WriteMsg /
// ReadMsg carries one discrete frame. CEDAR's stream.Stream satisfies it via an
// adapter (see cedaradapter); tests use a length-prefixed net.Pipe. WriteMsg may be
// called concurrently with ReadMsg, but not concurrently with itself -- the mux
// serializes writers.
type MsgConn interface {
	WriteMsg(b []byte) error
	ReadMsg() ([]byte, error)
	Close() error
}

// pipeConn frames discrete messages over a byte stream (a net.Pipe half in tests, or
// any io.ReadWriteCloser) with a u32 length prefix. Its ReadMsg returns a fresh slice
// per call.
type pipeConn struct {
	rw  io.ReadWriteCloser
	wmu sync.Mutex // serializes writers; the reader runs concurrently
	rmu sync.Mutex // serializes readers (typically a single reader goroutine)
}

// NewStreamConn frames messages over an ordered byte stream. Exposed so a caller can
// run dbrpc over any io.ReadWriteCloser (e.g. a plain TCP conn) without CEDAR.
func NewStreamConn(rw io.ReadWriteCloser) MsgConn { return &pipeConn{rw: rw} }

func (p *pipeConn) WriteMsg(b []byte) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(b)))
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if _, err := p.rw.Write(hdr[:]); err != nil {
		return err
	}
	_, err := p.rw.Write(b)
	return err
}

func (p *pipeConn) ReadMsg() ([]byte, error) {
	var hdr [4]byte
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if _, err := io.ReadFull(p.rw, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.rw, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (p *pipeConn) Close() error { return p.rw.Close() }

// netPipe returns two MsgConns connected to each other (for tests).
func netPipe() (MsgConn, MsgConn) {
	a, b := net.Pipe()
	return NewStreamConn(a), NewStreamConn(b)
}
