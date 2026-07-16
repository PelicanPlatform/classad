package dbrpc

import (
	"encoding/json"

	"github.com/PelicanPlatform/classad/db"
)

// Archive (history) table RPCs. Create/append are unary mutating ops (handled in handle);
// query streams newest-first results like opQuery. Private attributes are stripped for a
// non-privileged reader, as everywhere.

// streamArchiveQuery streams an archive's newest-first, limit-capped matches.
func (sc *serverConn) streamArchiveQuery(reqID uint64, r *reader) {
	name := r.str()
	limit := int(r.i32())
	constraint := r.str()
	if r.err != nil {
		sc.write(respBad(reqID))
		return
	}
	a, ok := sc.s.cat.ArchiveTable(name)
	if !ok {
		sc.write(respErr(reqID, "no such archive: "+name))
		return
	}
	seq, err := a.QueryLimit(constraint, limit) // limit pushed down (newest-first)
	if err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	for ad := range seq {
		if cancelled(sc.ctx) {
			return // client gone
		}
		sc.write(putStr(respHead(reqID, stStream), adString(ad, sc.opts.IncludePrivate)))
	}
	sc.write(respHead(reqID, stStreamEnd))
}

// --- client ---

// CreateArchiveTable creates (or no-ops if present) an append-only history table. cfg
// configures indexes / zone maps / retention on first creation.
func (c *Client) CreateArchiveTable(name string, cfg db.ArchiveConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	status, body, err := c.call(func(id uint64) []byte {
		return putBytes(putStr(req(id, opArchiveCreate), name), data)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ArchiveAppend appends an ad (old-ClassAd text) to the named history table.
func (c *Client) ArchiveAppend(name, adText string) error {
	status, body, err := c.call(func(id uint64) []byte {
		return putStr(putStr(req(id, opArchiveAppend), name), adText)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ArchiveQuery returns up to limit (<= 0 = all) newest-first matches (old-ClassAd texts)
// from the named history table -- the condor_history "last K" pattern.
func (c *Client) ArchiveQuery(name, constraint string, limit int) ([]string, error) {
	return c.stream(func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opArchiveQuery), name), int32(limit)), constraint)
	})
}

// ArchiveRotate enforces the named archive's retention policy now (using the server's
// clock for age-based rules), returning how many sealed segments were dropped.
func (c *Client) ArchiveRotate(name string) (int, error) {
	status, body, err := c.call(func(id uint64) []byte {
		return putStr(req(id, opArchiveRotate), name)
	})
	if err != nil {
		return 0, err
	}
	if status != stOK {
		return 0, statusErr(status, body)
	}
	return int(body.i32()), nil
}

// ArchiveTables lists the history table names.
func (c *Client) ArchiveTables() ([]string, error) {
	status, body, err := c.call(func(id uint64) []byte { return req(id, opArchiveList) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	n := int(body.i32())
	out := make([]string, 0, n)
	for i := 0; i < n && body.err == nil; i++ {
		out = append(out, body.str())
	}
	return out, nil
}
