package db

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// Catalog snapshot: a backup of EVERY table, streamed as a sequence of named sections,
// each section being one table's self-delimiting DB snapshot. Because each table snapshot
// carries its own encryption envelope, a catalog backup is decryptable with the pool keys
// (or the per-table escrow keys), and it streams -- one table section at a time -- so an
// arbitrarily large multi-table database never buffers whole in memory.
//
//	[catMagic]
//	repeat: [uvarint(len name)][name][ table snapshot ]   // empty name terminates

var catMagic = []byte("CADBCAT1")

// Snapshot writes a backup of every table in the catalog to w. Each table is captured
// under its own DB-wide lock (consistent per table); the set of tables is the snapshot's
// membership at the moment each name is read.
func (cat *Catalog) Snapshot(w io.Writer) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.Write(catMagic); err != nil {
		return err
	}
	for _, name := range cat.Tables() {
		d, ok := cat.Table(name)
		if !ok {
			continue // dropped concurrently
		}
		if err := writeChunk(bw, []byte(name)); err != nil {
			return err
		}
		if _, err := d.snapshotTo(bw); err != nil {
			return fmt.Errorf("db: snapshotting table %q: %w", name, err)
		}
	}
	if err := writeChunk(bw, nil); err != nil { // empty name = end
		return err
	}
	return bw.Flush()
}

// Restore replaces each table named in the catalog snapshot with its snapshotted contents
// (creating the table if absent), each under that table's DB-wide lock. Tables present in
// the catalog but ABSENT from the snapshot are left untouched. Encrypted sections are
// opened with the catalog's pool keys (the tables share them).
func (cat *Catalog) Restore(r io.Reader) error {
	br := bufio.NewReader(r)
	magic := make([]byte, len(catMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return fmt.Errorf("db: reading catalog snapshot header: %w", err)
	}
	if !bytes.Equal(magic, catMagic) {
		return fmt.Errorf("db: not a catalog snapshot (bad magic)")
	}
	for {
		name, err := readChunk(br)
		if err != nil {
			return fmt.Errorf("db: reading catalog snapshot: %w", err)
		}
		if len(name) == 0 {
			return nil // end of catalog
		}
		d, err := cat.CreateTable(string(name))
		if err != nil {
			return fmt.Errorf("db: creating table %q for restore: %w", name, err)
		}
		if err := d.restoreFrom(br, SnapshotKeys{}); err != nil {
			return fmt.Errorf("db: restoring table %q: %w", name, err)
		}
	}
}
