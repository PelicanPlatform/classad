// Package dbrpc is the client/server DB: it serves an embedded ClassAd log
// (package db) over a message transport (CEDAR in production), with out-of-order
// request multiplexing so a slow call never head-of-line-blocks others on the same
// connection. See DESIGN.md.
package dbrpc

import (
	"encoding/binary"
	"errors"
)

// op is the request opcode (first byte of a request frame's body).
type op uint8

const (
	opBegin      op = 1 // -> txnID
	opCommit     op = 2 // [txnID] -> status; payload = conflicted keys
	opAbort      op = 3 // [txnID]
	opNewAd      op = 4 // [txnID][key][adText]
	opDestroyAd  op = 5 // [txnID][key]
	opSetAttr    op = 6 // [txnID][key][name][expr]
	opDeleteAttr op = 7 // [txnID][key][name]
	opLookupAd   op = 8 // [txnID][key] -> [adText]
	opLookupAttr op = 9 // [txnID][key][name] -> [value]

	// Streaming reads (no txnID: they read the committed store). Each streams zero or
	// more result frames (stStream) then a terminator (stStreamEnd), all under one id.
	opQuery       op = 10 // [limit i32][constraint] -> stream of [adText] (limit<=0 = all)
	opMatchSorted op = 11 // [limit i32][jobText] -> stream of [adText] (ranked)
	opWatch       op = 12 // [cursor] -> stream of [eventType u8][key][adText]; long-lived
	opWatchStop   op = 13 // [watchReqID u64] -> cancels a running opWatch
	opOrdered     op = 14 // [index i32][partition] -> stream of [signature u64][adText]

	// opAggregate is a server-side GROUP BY: the server scans the constraint
	// match, buckets rows by the group-by column tuple in a hash map, and streams
	// one row per group. Request: [constraint][nGroup i32]{[col]}[nAgg i32]
	// {[func u8][arg]}. Response: stream of a frame per group, each carrying the
	// nGroup group values then the nAgg aggregate values (all as strings, in
	// order), then a terminator.
	opAggregate op = 15

	// Diagnostics and index/hot-set management. Each carries a leading [table].
	opDiag    op = 16 // [table] -> [json Diagnostics] (storage stats, hot attrs, indexes, suggestions)
	opExplain op = 17 // [table][constraint] -> [json db.QueryExplain] (query access path)
	opAdmin   op = 18 // [table][action][nArgs i32]{[arg]} -> [message]; mutating (refused read-only)

	// Table catalog management.
	opCreateTable op = 19 // [table] -> status; mutating
	opDropTable   op = 20 // [table] -> status; mutating
	opListTables  op = 21 // -> [n i32]{[name]}

	// opMatchTables is cross-table matchmaking (bilateral Requirements/Rank):
	// [reqTable][resTable][keyAttr][reqWhere][targetWhere][limit i32] -> stream of
	// [requestKey][resourceKey][rank] (best-ranked first, up to limit per request).
	opMatchTables op = 22

	// opMatchExplain explains the match plan for one request against a resource table:
	// [reqTable][jobSelector][resTable] -> [json db.MatchExplain]. The server takes the
	// first request in reqTable matching jobSelector and reports how matchmaking it
	// against resTable would execute (slot-side probe rewrite + index pushdown).
	opMatchExplain op = 23
)

// String names an opcode for diagnostics (e.g. the read-only rejection message).
func (o op) String() string {
	switch o {
	case opBegin:
		return "Begin"
	case opCommit:
		return "Commit"
	case opAbort:
		return "Abort"
	case opNewAd:
		return "NewClassAd"
	case opDestroyAd:
		return "DestroyClassAd"
	case opSetAttr:
		return "SetAttribute"
	case opDeleteAttr:
		return "DeleteAttribute"
	case opLookupAd:
		return "LookupClassAd"
	case opLookupAttr:
		return "LookupAttr"
	case opQuery:
		return "Query"
	case opMatchSorted:
		return "MatchSorted"
	case opWatch:
		return "Watch"
	case opWatchStop:
		return "WatchStop"
	case opOrdered:
		return "Ordered"
	case opAggregate:
		return "Aggregate"
	case opDiag:
		return "Diagnostics"
	case opExplain:
		return "Explain"
	case opMatchExplain:
		return "MatchExplain"
	case opAdmin:
		return "Admin"
	case opCreateTable:
		return "CreateTable"
	case opDropTable:
		return "DropTable"
	case opListTables:
		return "ListTables"
	case opMatchTables:
		return "MatchTables"
	}
	return "op(unknown)"
}

// status codes returned in a response frame.
const (
	stOK        int32 = 0
	stErr       int32 = -1 // generic error; payload is a UTF-8 message
	stMissing   int32 = -2 // key/attribute absent
	stConflict  int32 = -3 // commit had write-write conflicts; payload = conflicted keys
	stBadReq    int32 = -4 // malformed request
	stStream    int32 = 1  // one streamed result frame; more may follow
	stStreamEnd int32 = 2  // end of a stream (no payload)
)

// frameStatus reads the status field of a response frame (bytes 8..12).
func frameStatus(frame []byte) int32 {
	if len(frame) < 12 {
		return stErr
	}
	return int32(binary.LittleEndian.Uint32(frame[8:]))
}

// A request frame is [reqID u64][op u8][fields...]; a response frame is
// [reqID u64][status i32][payload...]. Every field written by putBytes is a u32
// length prefix followed by the bytes, so frames parse without a schema.

var errShort = errors.New("dbrpc: short frame")

// --- frame builders (raw bytes; no ClassAd framing, minimal allocation) ---

func putU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func putI32(b []byte, v int32) []byte  { return binary.LittleEndian.AppendUint32(b, uint32(v)) }
func putU8(b []byte, v byte) []byte    { return append(b, v) }

// req starts a request frame: [reqID u64][op u8].
func req(reqID uint64, o op) []byte {
	return putU8(putU64(make([]byte, 0, 32), reqID), byte(o))
}

// frameReqID reads just the leading request/response id (for demux routing).
func frameReqID(frame []byte) (uint64, bool) {
	if len(frame) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(frame), true
}

func putBytes(b, v []byte) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(v)))
	return append(b, v...)
}
func putStr(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// reader consumes a frame body left to right.
type reader struct {
	b   []byte
	err error
}

func (r *reader) u64() uint64 {
	if r.err != nil || len(r.b) < 8 {
		r.fail()
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}
func (r *reader) i32() int32 {
	if r.err != nil || len(r.b) < 4 {
		r.fail()
		return 0
	}
	v := int32(binary.LittleEndian.Uint32(r.b))
	r.b = r.b[4:]
	return v
}
func (r *reader) u8() uint8 {
	if r.err != nil || len(r.b) < 1 {
		r.fail()
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

// bytesRef returns the next length-prefixed field as a sub-slice of the frame (no
// copy); callers that retain it past the frame's lifetime must copy.
func (r *reader) bytesRef() []byte {
	if r.err != nil || len(r.b) < 4 {
		r.fail()
		return nil
	}
	n := int(binary.LittleEndian.Uint32(r.b))
	r.b = r.b[4:]
	if n < 0 || len(r.b) < n {
		r.fail()
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
func (r *reader) str() string { return string(r.bytesRef()) }
func (r *reader) fail()       { r.err = errShort }

// reqHeader / respHeader parse the fixed prefixes.
func reqHeader(frame []byte) (reqID uint64, o op, body *reader, ok bool) {
	r := &reader{b: frame}
	reqID = r.u64()
	o = op(r.u8())
	if r.err != nil {
		return 0, 0, nil, false
	}
	return reqID, o, r, true
}

func respHeader(frame []byte) (reqID uint64, status int32, body *reader, ok bool) {
	r := &reader{b: frame}
	reqID = r.u64()
	status = r.i32()
	if r.err != nil {
		return 0, 0, nil, false
	}
	return reqID, status, r, true
}
