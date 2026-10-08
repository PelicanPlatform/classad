package dbrpc

import "strconv"

// readOnlyTablePrefix opens the message of every per-table write refusal
// (ServeOptions.TableWritable). The client recognizes it to report ErrTableReadOnly, and it
// is deliberately not "read-only connection": that one means the peer lacks WRITE
// authorization, this one that the table belongs to another writer.
const readOnlyTablePrefix = "read-only table "

// respTableReadOnly is the refusal for a write to a table TableWritable rejected. It is an
// stErr, so every client -- including one that predates ErrTableReadOnly -- sees a
// *ServerError, which the error taxonomy already marks as deterministic (not retried).
func respTableReadOnly(reqID uint64, table, what string) []byte {
	return respErr(reqID, readOnlyTablePrefix+strconv.Quote(table)+": "+what+" not permitted")
}

// tableWritable reports whether this connection may modify table.
func (sc *serverConn) tableWritable(table string) bool {
	return sc.opts.TableWritable == nil || sc.opts.TableWritable(table)
}

// tableWriteTarget names the table request o would modify, when that modification is
// subject to ServeOptions.TableWritable, plus a label for the refusal. It peeks at body
// without consuming it. ok=false means o is not gated (a read, an exporter op, a
// row-preserving admin action), or the frame is too short or names no live transaction --
// the handler then answers that itself.
//
// This switch is the inventory of table writes; a new opcode that modifies a table must be
// added here. Commit is not listed: whether it writes depends on what the transaction
// accepted, so the handler checks it (refuseCommit). Restore is not listed either: it is
// handled inline in the read loop, before dispatch, and checks in restoreStart/restoreEnd.
func (sc *serverConn) tableWriteTarget(o op, body *reader) (table, what string, ok bool) {
	r := *body
	switch o {
	case opNewAd, opNewAdBatch, opDestroyAd, opSetAttr, opDeleteAttr, opCommitIdem:
		// A transaction is bound to one table at opBegin, so its table is the target of
		// every write through it -- there is no way to address a second table from one
		// transaction. Begin itself is not gated: a transaction is also how a peer gets a
		// stable snapshot to read from.
		id := r.u64()
		if r.err != nil {
			return "", "", false
		}
		v, _ := sc.s.txns.Load(id)
		st, found := v.(*serverTxn)
		if !found {
			return "", "", false
		}
		return st.table, o.String(), true
	case opDeleteWhere, opArchiveAppend, opArchiveRotate,
		opCreateTable, opCreateTableMem, opDropTable, opArchiveCreate,
		opCreateView, opDropView:
		// Creating is gated as well as dropping: a peer that creates the owned name first
		// (say, as a RAM-only table) decides what the owner then writes into.
		table = r.str()
		return table, o.String(), r.err == nil
	case opTableToMemory:
		// DAEMON-only; leave an unprivileged peer the authorization refusal it gets today.
		if !sc.opts.Privileged {
			return "", "", false
		}
		table = r.str()
		return table, o.String(), r.err == nil
	case opAdmin:
		// Admin is DAEMON-only and authorizes before looking at the action; keep that order
		// so an unprivileged peer cannot probe which tables are owned.
		if !sc.opts.Privileged {
			return "", "", false
		}
		table, action := r.str(), r.str()
		if r.err != nil || !adminRemovesData(action) {
			return "", "", false
		}
		return table, "admin action " + strconv.Quote(action), true
	}
	return "", "", false
}

// adminRemovesData reports whether an admin action (mutable or archive table) removes rows
// or changes which rows are kept. Those are gated by TableWritable even for a Privileged
// peer: a table's owner decides what it holds. The rest of the admin table retunes layout
// and keeps every row, so an operator may still tune an owned table.
func adminRemovesData(action string) bool {
	switch action {
	case "truncate", "rotate", "retention.set":
		return true
	}
	return false
}

// refuseCommit returns the refusal for committing st when it carries writes to a table this
// connection may no longer modify, aborting st; nil means the commit may proceed. The write
// ops are gated as they arrive, so this only trips when TableWritable changed its answer
// after writes were accepted -- without it those buffered writes would land on a table the
// owner had already taken over. A transaction with no writes (a read snapshot) commits.
func (sc *serverConn) refuseCommit(reqID uint64, st *serverTxn) []byte {
	if sc.opts.TableWritable == nil {
		return nil
	}
	st.mu.Lock()
	wrote := st.wrote
	st.mu.Unlock()
	if !wrote || sc.opts.TableWritable(st.table) {
		return nil
	}
	st.mu.Lock()
	st.tx.Abort()
	st.mu.Unlock()
	return respTableReadOnly(reqID, st.table, opCommit.String())
}
