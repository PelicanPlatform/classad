package db

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PelicanPlatform/classad/collections/crypt"
)

// Master-key lifecycle. Every database has a master key, because sealing private attributes is not
// optional -- a ClaimId must not sit on disk in the clear whatever the deployment configured.
//
// POOL KEYS decide whether that master is PROTECTED. With them the master is wrapped once per available
// pool key (a KEK) into masterkeys.json, so any one pool key opens the DB and a rotated-in
// key can be added without re-encrypting. On open we recover the master and derive the
// DB data key (the DataInfo subkey -- distinct from the master, which only wraps keys);
// the collection uses the data key to seal the configured attributes. See collections.

// KEK is a pool / signing key that can wrap the DB master key. Re-exported so db callers
// need not import crypt.
type KEK = crypt.KEK

// masterKeysFile is the persisted set of master-key wrappings (one per pool key).
const masterKeysFile = "masterkeys.json"

// dbCrypto holds a store's derived encryption state: the data key (seals attributes in
// the live store), the backup key (wraps a snapshot's key -- see Snapshot), and the
// master-key envelope rows (embedded in a portable snapshot so any pool key can restore
// it). All are derived from the master, which is never retained in the clear. A nil
// *dbCrypto means encryption is disabled.
type dbCrypto struct {
	dataKey   []byte
	backupKey []byte
	rows      []crypt.MasterKeyRow
	poolKeys  []KEK // retained so Restore can open a snapshot's embedded master envelope
	// atRest reports whether the master is PROTECTED -- wrapped under pool keys. Without them the
	// database still seals private attributes (that is not optional), but its master sits beside the
	// data in the clear, so nothing here may claim encryption at rest.
	//
	// Every "is this encrypted" decision has to pick one of these two meanings deliberately. Sealing is
	// now always on, so a check that means "values are sealed" is a constant; a check that means
	// "protected from someone holding the disk" is this field.
	atRest bool
}

// protected reports whether the master is wrapped under pool keys. Snapshot protection follows THIS,
// not the presence of a data key: a snapshot sealed under a key whose envelope no pool key can open
// would be unrestorable, and an unprotected database has nothing to gain by pretending otherwise.
func (e *dbCrypto) protected() bool { return e != nil && e.atRest }

func (e *dbCrypto) data() []byte {
	if e == nil {
		return nil
	}
	return e.dataKey
}

// resolveCrypto derives a store's encryption state, or nil if encryption is not
// configured (no pool keys). For a persistent store it loads (or, on first use, mints
// and persists) the master wrapped under the given pool keys, recovers it with whichever
// key matches, and lazily adds a wrapping for any pool key not yet represented (rotation).
// For an in-memory store it mints an ephemeral master (encryption works but is not
// persisted). It errors if a persisted master exists but no available pool key can open
// it -- refusing to silently run unencrypted or lose access to sealed data.
// resolveCrypto always produces a data key, because sealing private attributes is not optional: a
// ClaimId must never sit on disk in the clear, whatever the deployment configured.
//
// Pool keys decide only whether the MASTER is protected. With them, it is wrapped under each key
// (masterkeys.json) as before. Without them it is stored beside the data in the clear -- which is not
// encryption at rest, is not claimed to be, and is reported as false by DB.EncryptionEnabled.
func resolveCrypto(dir string, poolKeys []KEK) (*dbCrypto, error) {
	if len(poolKeys) == 0 {
		master, err := loadOrMintUnprotectedMaster(dir)
		if err != nil {
			return nil, err
		}
		return newDBCrypto(master, nil, nil, false)
	}
	master, rows, err := loadOrMintMaster(dir, poolKeys)
	if err != nil {
		return nil, err
	}
	return newDBCrypto(master, rows, poolKeys, true)
}

// newDBCrypto derives the data and backup subkeys from a master. atRest records whether that master is
// protected (see dbCrypto.atRest).
func newDBCrypto(master []byte, rows []crypt.MasterKeyRow, poolKeys []KEK, atRest bool) (*dbCrypto, error) {
	dataKey, err := crypt.Subkey(master, crypt.DataInfo)
	if err != nil {
		return nil, err
	}
	backupKey, err := crypt.Subkey(master, crypt.BackupInfo)
	if err != nil {
		return nil, err
	}
	return &dbCrypto{dataKey: dataKey, backupKey: backupKey, rows: rows, poolKeys: poolKeys, atRest: atRest}, nil
}

// unprotectedMasterFile holds the master key IN THE CLEAR for a database with no pool keys. Named so
// that finding it on disk tells the reader exactly what it is.
const unprotectedMasterFile = "masterkey.unprotected"

// loadOrMintUnprotectedMaster recovers, or on first use mints and persists, the master for a database
// with no pool keys. An in-memory database keeps it in memory only.
//
// The file is 0600 and carries no wrapping: with no pool key there is nothing to wrap it with. It exists
// so that sealed values survive a restart -- without it a reopen could not read its own data.
func loadOrMintUnprotectedMaster(dir string) ([]byte, error) {
	if dir == "" {
		return crypt.NewMaster() // in-memory: nothing to persist, nothing to reload
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("db: creating database directory: %w", err)
	}
	path := filepath.Join(dir, unprotectedMasterFile)
	switch b, err := os.ReadFile(path); {
	case err == nil:
		master, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(master) == 0 {
			return nil, fmt.Errorf("db: %s is unreadable: %v", unprotectedMasterFile, derr)
		}
		return master, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("db: reading %s: %w", unprotectedMasterFile, err)
	}
	master, err := crypt.NewMaster()
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(master)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(enc+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("db: writing %s: %w", unprotectedMasterFile, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("db: installing %s: %w", unprotectedMasterFile, err)
	}
	return master, nil
}

// loadOrMintMaster recovers (or, on first use, mints and persists) the master key and
// its envelope rows for the given pool keys.
func loadOrMintMaster(dir string, poolKeys []KEK) (master []byte, rows []crypt.MasterKeyRow, err error) {
	if dir == "" {
		// Ephemeral master for an in-memory DB. Still wrap it under the pool keys so a
		// snapshot carries a pool-key-openable envelope (the rows just are not persisted).
		if master, err = crypt.NewMaster(); err != nil {
			return nil, nil, err
		}
		for _, k := range poolKeys {
			row, werr := crypt.WrapMaster(master, k)
			if werr != nil {
				return nil, nil, fmt.Errorf("db: wrapping master under key %q: %w", k.ID, werr)
			}
			rows = append(rows, row)
		}
		return master, rows, nil
	}
	// Ensure the directory exists: resolveCrypto runs before the collection creates it,
	// and (for a catalog) the per-table directory may not exist yet.
	if err = os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("db: creating database directory: %w", err)
	}
	path := filepath.Join(dir, masterKeysFile)
	if rows, err = loadMasterRows(path); err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		// First use: mint a master and wrap it under every available pool key.
		if master, err = crypt.NewMaster(); err != nil {
			return nil, nil, err
		}
		for _, k := range poolKeys {
			row, werr := crypt.WrapMaster(master, k)
			if werr != nil {
				return nil, nil, fmt.Errorf("db: wrapping master under key %q: %w", k.ID, werr)
			}
			rows = append(rows, row)
		}
		if err = saveMasterRows(path, rows); err != nil {
			return nil, nil, err
		}
		return master, rows, nil
	}
	if master, err = crypt.OpenMaster(rows, poolKeys); err != nil {
		return nil, nil, fmt.Errorf("db: opening encrypted database: %w", err)
	}
	// Rotation: add a wrapping for any available pool key not yet on file, so a
	// newly-provisioned key can open the DB on the next start.
	if addMissingWraps(&rows, master, poolKeys) {
		if err = saveMasterRows(path, rows); err != nil {
			return nil, nil, err
		}
	}
	return master, rows, nil
}

// addMissingWraps appends a wrapping row for each pool key whose ID is not already
// represented in rows. Reports whether it changed rows.
func addMissingWraps(rows *[]crypt.MasterKeyRow, master []byte, poolKeys []KEK) bool {
	have := make(map[string]struct{}, len(*rows))
	for _, r := range *rows {
		have[r.KeyID] = struct{}{}
	}
	changed := false
	for _, k := range poolKeys {
		if _, ok := have[k.ID]; ok {
			continue
		}
		if row, err := crypt.WrapMaster(master, k); err == nil {
			*rows = append(*rows, row)
			changed = true
		}
	}
	return changed
}

func loadMasterRows(path string) ([]crypt.MasterKeyRow, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []crypt.MasterKeyRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("db: parsing %s: %w", masterKeysFile, err)
	}
	return rows, nil
}

func saveMasterRows(path string, rows []crypt.MasterKeyRow) error {
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file holds only wrapped ciphertext + salts (opening still needs a pool
	// key), but there is no reason to make it world-readable.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
