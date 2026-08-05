package dbrpc

import (
	"context"
	"errors"
)

// ErrTxnReadUnsupported is returned by the Tx read methods against a server too old to
// implement the transaction-scoped read opcodes (it rejects the request). A caller falls
// back to the connection-level Query/QueryKeys, which read the committed store and so do
// not see the transaction's own uncommitted writes.
var ErrTxnReadUnsupported = errors.New("dbrpc: server does not support transaction-scoped reads")

// Query returns the ads matching the constraint as this transaction sees them: the
// committed rows with the transaction's own uncommitted writes overlaid. Client.Query
// reads the committed store and cannot see them, which is the whole reason this exists --
// a caller that has staged writes and wants to read them back must come through here.
//
// The transaction's table is implied (opBegin bound it), so unlike QueryTable there is no
// table argument. A limit <= 0 returns every match. Against a server without the opcode it
// returns ErrTxnReadUnsupported.
func (t *Tx) Query(ctx context.Context, constraint string, limit int) ([]string, error) {
	rows, err := t.c.streamCtx(ctx, func(id uint64) []byte {
		return putStr(putI32(putU64(req(id, opTxnQuery), t.id), int32(limit)), constraint)
	})
	return rows, txnReadErr(err)
}

// QueryStream is Query delivering each ad's text to yield as it arrives, rather than
// collecting the whole result. yield returns false to stop early.
func (t *Tx) QueryStream(ctx context.Context, constraint string, limit int, yield func(row string) bool) error {
	return txnReadErr(t.c.streamEach(ctx, func(id uint64) []byte {
		return putStr(putI32(putU64(req(id, opTxnQuery), t.id), int32(limit)), constraint)
	}, yield))
}

// KeysWhere returns the storage keys of the rows matching the constraint as this
// transaction sees them. It is what lets a caller address rows -- for a subsequent
// SetAttribute or DestroyClassAd in the same transaction -- including rows the
// transaction itself created, which Client.QueryKeysTable cannot see.
//
// Against a server without the opcode it returns ErrTxnReadUnsupported.
func (t *Tx) KeysWhere(ctx context.Context, constraint string) ([]string, error) {
	keys, err := t.c.streamCtx(ctx, func(id uint64) []byte {
		return putStr(putU64(req(id, opTxnQueryKeys), t.id), constraint)
	})
	return keys, txnReadErr(err)
}

// txnReadErr maps the server's rejection of an unknown opcode to ErrTxnReadUnsupported,
// so a caller can tell "this server is too old" from "this query was wrong" without
// matching on message text.
func txnReadErr(err error) error {
	if errors.Is(err, ErrBadRequest) {
		return ErrTxnReadUnsupported
	}
	return err
}

// streamTxnQuery is the server side of opTxnQuery: the transaction's own view of a
// constraint query, streamed like opQuery.
func (s *Server) streamTxnQuery(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	id := r.u64()
	limit := int(r.i32())
	constraint := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	s.withTxnStream(reqID, id, write, func(st *serverTxn) {
		seq, err := st.tx.Query(constraint)
		if err != nil {
			write(respErr(reqID, err.Error()))
			return
		}
		n := 0
		for ad := range seq {
			if cancelled(ctx) {
				return // client gone: stop the scan
			}
			write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
			n++
			if limit > 0 && n >= limit {
				break
			}
		}
		write(respHead(reqID, stStreamEnd))
	})
}

// streamTxnQueryKeys is the server side of opTxnQueryKeys.
func (s *Server) streamTxnQueryKeys(ctx context.Context, reqID uint64, r *reader, write func([]byte)) {
	id := r.u64()
	constraint := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	s.withTxnStream(reqID, id, write, func(st *serverTxn) {
		seq, err := st.tx.KeysWhere(constraint)
		if err != nil {
			write(respErr(reqID, err.Error()))
			return
		}
		for key := range seq {
			if cancelled(ctx) {
				return
			}
			write(putStr(respHead(reqID, stStream), key))
		}
		write(respHead(reqID, stStreamEnd))
	})
}

// withTxnStream resolves a transaction id and runs a streaming read under its lock.
//
// It is withTxn's shape for a streamer: withTxn returns one response frame, while these
// ops write many. The lock is held for the whole stream because db.Txn is not safe for
// concurrent use -- a write op pipelined onto the same transaction mid-scan would race
// the scan's read of the write buffer.
func (s *Server) withTxnStream(reqID, id uint64, write func([]byte), fn func(*serverTxn)) {
	v, ok := s.txns.Load(id)
	if !ok {
		write(respErr(reqID, "no such transaction"))
		return
	}
	st := v.(*serverTxn)
	st.lastTouch.Store(nowNano())
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(st)
}
