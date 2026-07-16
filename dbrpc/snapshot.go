package dbrpc

import (
	"fmt"
	"io"
	"os"
)

// Chunked snapshot / restore (DAEMON-only). Snapshot streams out (stStream frames, like a
// query); restore streams in as chunk frames the server spools to a temp file and then
// restores from. Neither side ever holds the whole database in memory.

// snapChunk is the target payload of a streamed snapshot/restore chunk.
const snapChunk = 64 * 1024

// --- server: snapshot streams out ---

// streamSnapshot runs on a dispatch goroutine: it writes the backup as a sequence of
// stStream frames (via the serialized sc.write, so they are ordered) then a terminator.
func (sc *serverConn) streamSnapshot(reqID uint64, r *reader) {
	if !sc.opts.Privileged {
		sc.write(respErr(reqID, "snapshot requires DAEMON authorization"))
		return
	}
	table := r.str()
	d, ok := sc.s.tableOr(reqID, table, sc.write)
	if !ok {
		return
	}
	w := &chunkWriter{sc: sc, reqID: reqID}
	if err := d.Snapshot(w); err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	w.flush()
	sc.write(respHead(reqID, stStreamEnd))
}

// chunkWriter batches db.Snapshot's writes into ~snapChunk stStream frames.
type chunkWriter struct {
	sc    *serverConn
	reqID uint64
	buf   []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) >= snapChunk {
		w.emit()
	}
	return len(p), nil
}

func (w *chunkWriter) emit() {
	// putBytes copies buf into the frame, so buf is safe to reset.
	w.sc.write(putBytes(respHead(w.reqID, stStream), w.buf))
	w.buf = w.buf[:0]
}

func (w *chunkWriter) flush() {
	if len(w.buf) > 0 {
		w.emit()
	}
}

// --- server: restore streams in, spooled to a temp file ---

// restoreUpload is a restore in progress on a connection: chunks are appended to a temp
// file, which is restored from at opRestoreEnd. Touched only by the read-loop goroutine.
type restoreUpload struct {
	reqID uint64
	table string
	f     *os.File
	path  string
	err   error // sticky spool error
}

// handleRestore processes a restore-upload frame inline in the read loop (preserving
// chunk order). At most one restore is active per connection.
func (sc *serverConn) handleRestore(reqID uint64, o op, r *reader) {
	switch o {
	case opRestore:
		sc.restoreStart(reqID, r)
	case opRestoreChunk:
		sc.restoreChunk(reqID, r)
	case opRestoreEnd:
		sc.restoreEnd(reqID)
	}
}

func (sc *serverConn) restoreStart(reqID uint64, r *reader) {
	sc.abortRestore() // discard any half-finished prior upload
	if !sc.opts.Privileged {
		sc.write(respErr(reqID, "restore requires DAEMON authorization"))
		return
	}
	if sc.opts.ReadOnly {
		sc.write(respErr(reqID, "read-only connection: restore not permitted"))
		return
	}
	table := r.str()
	if r.err != nil {
		sc.write(respBad(reqID))
		return
	}
	if _, ok := sc.s.cat.Table(table); !ok {
		sc.write(respErr(reqID, "no such table: "+table))
		return
	}
	f, err := os.CreateTemp("", "htcondordb-restore-*.cadb")
	if err != nil {
		sc.write(respErr(reqID, "restore spool: "+err.Error()))
		return
	}
	sc.restore = &restoreUpload{reqID: reqID, table: table, f: f, path: f.Name()}
	// No reply until opRestoreEnd.
}

func (sc *serverConn) restoreChunk(reqID uint64, r *reader) {
	ru := sc.restore
	if ru == nil || ru.reqID != reqID {
		return // stray chunk (rejected start, or wrong id): ignore
	}
	chunk := r.bytesRef()
	if r.err != nil {
		ru.err = fmt.Errorf("malformed restore chunk")
		return
	}
	if ru.err == nil {
		if _, err := ru.f.Write(chunk); err != nil {
			ru.err = err
		}
	}
}

func (sc *serverConn) restoreEnd(reqID uint64) {
	ru := sc.restore
	if ru == nil || ru.reqID != reqID {
		sc.write(respErr(reqID, "no restore in progress"))
		return
	}
	sc.restore = nil
	defer os.Remove(ru.path)
	if cerr := ru.f.Close(); cerr != nil && ru.err == nil {
		ru.err = cerr
	}
	if ru.err != nil {
		sc.write(respErr(reqID, "restore spool: "+ru.err.Error()))
		return
	}
	d, ok := sc.s.cat.Table(ru.table)
	if !ok {
		sc.write(respErr(reqID, "no such table: "+ru.table))
		return
	}
	f, err := os.Open(ru.path)
	if err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	defer f.Close()
	if err := d.Restore(f); err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	sc.write(resp(reqID, stOK))
}

// abortRestore discards an in-progress upload (connection closed, or a new one started).
func (sc *serverConn) abortRestore() {
	if sc.restore != nil {
		_ = sc.restore.f.Close()
		_ = os.Remove(sc.restore.path)
		sc.restore = nil
	}
}

// --- client ---

// SnapshotTable streams a consistent backup of the named table to w. DAEMON-level: a
// snapshot carries every attribute, including private ones. The server streams it in
// chunks, so neither side buffers the whole database.
func (c *Client) SnapshotTable(table string, w io.Writer) error {
	_, ch, err := c.callStream(func(id uint64) []byte {
		return putStr(req(id, opSnapshot), table)
	})
	if err != nil {
		return err
	}
	for frame := range ch {
		_, status, body, ok := respHeader(frame)
		if !ok {
			drain(ch)
			return errShort
		}
		switch status {
		case stStream:
			if _, err := w.Write(body.bytesRef()); err != nil {
				drain(ch) // let the read loop finish delivering so the channel closes
				return err
			}
		case stErr:
			return statusErr(status, body)
		}
	}
	return nil
}

// RestoreTable replaces the named table with the snapshot read from r. DAEMON-level and
// destructive: the server spools the upload and restores under the DB-wide lock. The
// upload streams in chunks, so the client does not buffer the whole snapshot.
func (c *Client) RestoreTable(table string, r io.Reader) error {
	id := c.nextReq.Add(1)
	ch, err := c.sendID(id, func(reqID uint64) []byte {
		return putStr(req(reqID, opRestore), table)
	}, false)
	if err != nil {
		return err
	}
	buf := make([]byte, snapChunk)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if werr := c.conn.WriteMsg(putBytes(req(id, opRestoreChunk), buf[:n])); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if werr := c.conn.WriteMsg(req(id, opRestoreEnd)); werr != nil {
		return werr
	}
	frame, ok := <-ch
	if !ok {
		return c.closeErr
	}
	_, status, body, ok := respHeader(frame)
	if !ok {
		return errShort
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// Snapshot / Restore operate on the default table.
func (c *Client) Snapshot(w io.Writer) error { return c.SnapshotTable(DefaultTable, w) }
func (c *Client) Restore(r io.Reader) error  { return c.RestoreTable(DefaultTable, r) }

// drain discards any remaining frames until the stream channel closes, so the client's
// read loop is never blocked writing to a channel no one is reading.
func drain(ch <-chan []byte) {
	go func() {
		for range ch {
		}
	}()
}
