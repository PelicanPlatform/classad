package db

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/crypt"
	"github.com/klauspost/compress/zstd"
)

// Snapshot / restore: a self-contained, compressed, optionally-encrypted backup of the
// whole DB. When encryption is on, a fresh random snapshot key seals the body and is
// itself wrapped by the DB backup key (the master's BackupInfo subkey, distinct from the
// live-data key); the master envelope is embedded so ANY pool key can restore -- even on
// another node -- without the source DB's live keys. The body is written as independently
// compressed+encrypted frames (a bounded batch of ads each), so snapshot/restore stream
// with bounded memory rather than buffering the whole DB.

var snapMagic = []byte("CADBSNP1") // 8-byte magic + format version

const (
	snapFlagEncrypted = 1 << 0
	snapBatchAds      = 256 // ads per body frame
)

// package-level zstd codecs; EncodeAll/DecodeAll are safe for concurrent use.
var (
	snapZW, _ = zstd.NewWriter(nil)
	snapZR, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
)

// Snapshot writes a consistent backup of every ad to w. It holds the DB-wide lock shared,
// so it is consistent against writers without blocking other readers or snapshots.
func (db *DB) Snapshot(w io.Writer) error {
	db.snapMu.RLock()
	defer db.snapMu.RUnlock()

	bw := bufio.NewWriter(w)
	if _, err := bw.Write(snapMagic); err != nil {
		return err
	}

	enc := db.enc
	var flags byte
	if enc != nil {
		flags |= snapFlagEncrypted
	}
	if err := bw.WriteByte(flags); err != nil {
		return err
	}

	var snapKey []byte
	if enc != nil {
		// Embed the master envelope, then a fresh snapshot key wrapped by the backup key.
		rowsJSON, err := json.Marshal(enc.rows)
		if err != nil {
			return err
		}
		if err := writeChunk(bw, rowsJSON); err != nil {
			return err
		}
		if snapKey, err = crypt.NewDEK(); err != nil {
			return err
		}
		nonce, wrapped, err := crypt.Seal(enc.backupKey, snapKey)
		if err != nil {
			return err
		}
		if err := writeChunk(bw, nonce); err != nil {
			return err
		}
		if err := writeChunk(bw, wrapped); err != nil {
			return err
		}
	}

	// Body: frames of up to snapBatchAds ads, each zstd-compressed then (if keyed)
	// sealed. A frame is [uvarint adCount][chunk payload]; adCount 0 terminates.
	var batch []byte // accumulated plaintext for the current frame
	nInBatch := 0
	flush := func() error {
		if nInBatch == 0 {
			return nil
		}
		comp := snapZW.EncodeAll(batch, nil)
		payload := comp
		if snapKey != nil {
			nonce, ct, err := crypt.Seal(snapKey, comp)
			if err != nil {
				return err
			}
			payload = append(append([]byte{byte(len(nonce))}, nonce...), ct...)
		}
		if err := writeUvarint(bw, uint64(nInBatch)); err != nil {
			return err
		}
		if err := writeChunk(bw, payload); err != nil {
			return err
		}
		batch = batch[:0]
		nInBatch = 0
		return nil
	}

	var ferr error
	db.c.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		batch = appendChunk(batch, []byte(key))
		batch = appendChunk(batch, []byte(ad.MarshalOldWithPrivate()))
		if nInBatch++; nInBatch >= snapBatchAds {
			ferr = flush()
			return ferr == nil
		}
		return true
	})
	if ferr != nil {
		return ferr
	}
	if err := flush(); err != nil {
		return err
	}
	if err := writeUvarint(bw, 0); err != nil { // end-of-body marker
		return err
	}
	return bw.Flush()
}

// Truncate removes every ad from the DB, atomically against all writers (it takes the
// DB-wide lock exclusively). A DAEMON-level operation -- the first half of a restore, or
// a deliberate wipe.
func (db *DB) Truncate() {
	db.snapMu.Lock()
	defer db.snapMu.Unlock()
	db.c.Truncate()
	db.c.Reindex()
}

// Restore replaces the entire DB with the contents of the snapshot in r. It holds the
// DB-wide lock exclusively: all writers are blocked and the truncate+reload is atomic.
// An encrypted snapshot is opened with this DB's pool keys against the snapshot's embedded
// master envelope, so a snapshot taken by any DB sharing a pool key can be restored.
func (db *DB) Restore(r io.Reader) error {
	db.snapMu.Lock()
	defer db.snapMu.Unlock()

	br := bufio.NewReader(r)
	magic := make([]byte, len(snapMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return fmt.Errorf("db: reading snapshot header: %w", err)
	}
	if !bytes.Equal(magic, snapMagic) {
		return fmt.Errorf("db: not a snapshot (bad magic)")
	}
	flags, err := br.ReadByte()
	if err != nil {
		return err
	}
	encrypted := flags&snapFlagEncrypted != 0

	var snapKey []byte
	if encrypted {
		rowsJSON, err := readChunk(br)
		if err != nil {
			return err
		}
		var rows []crypt.MasterKeyRow
		if err := json.Unmarshal(rowsJSON, &rows); err != nil {
			return fmt.Errorf("db: parsing snapshot key envelope: %w", err)
		}
		if db.enc == nil {
			return fmt.Errorf("db: snapshot is encrypted but this database has no keys")
		}
		master, err := crypt.OpenMaster(rows, db.enc.poolKeys)
		if err != nil {
			return fmt.Errorf("db: cannot open snapshot: %w", err)
		}
		backupKey, err := crypt.Subkey(master, crypt.BackupInfo)
		if err != nil {
			return err
		}
		nonce, err := readChunk(br)
		if err != nil {
			return err
		}
		wrapped, err := readChunk(br)
		if err != nil {
			return err
		}
		if snapKey, err = crypt.Open(backupKey, nonce, wrapped); err != nil {
			return fmt.Errorf("db: unwrapping snapshot key: %w", err)
		}
	}

	// Point of no return: empty the store, then load the frames. Under the exclusive
	// lock, no writer observes the intermediate empty state.
	db.c.Truncate()
	for {
		nAds, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("db: reading snapshot frame: %w", err)
		}
		if nAds == 0 {
			break // end of body
		}
		payload, err := readChunk(br)
		if err != nil {
			return err
		}
		comp := payload
		if encrypted {
			if len(payload) < 1 || int(payload[0])+1 > len(payload) {
				return fmt.Errorf("db: malformed encrypted frame")
			}
			nl := int(payload[0])
			nonce, ct := payload[1:1+nl], payload[1+nl:]
			if comp, err = crypt.Open(snapKey, nonce, ct); err != nil {
				return fmt.Errorf("db: decrypting snapshot frame: %w", err)
			}
		}
		plain, err := snapZR.DecodeAll(comp, nil)
		if err != nil {
			return fmt.Errorf("db: decompressing snapshot frame: %w", err)
		}
		if err := db.loadFrame(plain, nAds); err != nil {
			return err
		}
	}
	db.c.Reindex() // rebuild indexes over the loaded ads
	return nil
}

// loadFrame parses nAds (key, ad-text) pairs from a decompressed frame and inserts them.
// It writes through the collection directly (not a DB transaction), so it does not
// re-enter the DB-wide lock Restore already holds; ads re-encode under the current
// encryption policy.
func (db *DB) loadFrame(plain []byte, nAds uint64) error {
	rd := &chunkReader{b: plain}
	for i := uint64(0); i < nAds; i++ {
		key, ok := rd.chunk()
		if !ok {
			return fmt.Errorf("db: truncated snapshot frame (key %d/%d)", i, nAds)
		}
		adText, ok := rd.chunk()
		if !ok {
			return fmt.Errorf("db: truncated snapshot frame (ad %d/%d)", i, nAds)
		}
		ad, err := classad.ParseOld(string(adText))
		if err != nil {
			return fmt.Errorf("db: parsing snapshot ad %q: %w", string(key), err)
		}
		if err := db.c.Put(key, ad); err != nil {
			return fmt.Errorf("db: loading snapshot ad %q: %w", string(key), err)
		}
	}
	return nil
}

// --- length-prefixed chunk helpers ---

func writeUvarint(w *bufio.Writer, v uint64) error {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	_, err := w.Write(tmp[:n])
	return err
}

func writeChunk(w *bufio.Writer, b []byte) error {
	if err := writeUvarint(w, uint64(len(b))); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func appendChunk(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// snapMaxChunk bounds a single length-prefixed chunk read from a snapshot, so a corrupt
// or hostile length cannot force a huge allocation.
const snapMaxChunk = 1 << 30 // 1 GiB

func readChunk(r *bufio.Reader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if n > snapMaxChunk {
		return nil, fmt.Errorf("db: snapshot chunk too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// chunkReader reads length-prefixed chunks from an in-memory frame.
type chunkReader struct {
	b   []byte
	pos int
}

func (r *chunkReader) chunk() ([]byte, bool) {
	n, w := binary.Uvarint(r.b[r.pos:])
	if w <= 0 {
		return nil, false
	}
	start := r.pos + w
	end := start + int(n)
	if end > len(r.b) {
		return nil, false
	}
	r.pos = end
	return r.b[start:end], true
}
