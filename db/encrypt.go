package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PelicanPlatform/classad/collections/crypt"
)

// Encryption-at-rest master-key lifecycle. A persistent DB roots its encryption in a
// random master key that is never stored in the clear: it is wrapped once per available
// pool key (a KEK) into masterkeys.json, so any one pool key opens the DB and a rotated-in
// key can be added without re-encrypting. On open we recover the master and derive the
// DB data key (the DataInfo subkey -- distinct from the master, which only wraps keys);
// the collection uses the data key to seal the configured attributes. See collections.

// KEK is a pool / signing key that can wrap the DB master key. Re-exported so db callers
// need not import crypt.
type KEK = crypt.KEK

// masterKeysFile is the persisted set of master-key wrappings (one per pool key).
const masterKeysFile = "masterkeys.json"

// resolveDataKey returns the DB data key for a store, or nil if encryption is not
// configured (no pool keys). For a persistent store it loads (or, on first use, mints
// and persists) the master wrapped under the given pool keys, recovers it with whichever
// key matches, and lazily adds a wrapping for any pool key not yet represented (rotation).
// For an in-memory store it mints an ephemeral master (encryption works but is not
// persisted). It errors if a persisted master exists but no available pool key can open
// it -- refusing to silently run unencrypted or lose access to sealed data.
func resolveDataKey(dir string, poolKeys []KEK) ([]byte, error) {
	if len(poolKeys) == 0 {
		return nil, nil // encryption disabled
	}
	if dir == "" {
		master, err := crypt.NewMaster() // ephemeral; in-memory DB
		if err != nil {
			return nil, err
		}
		return crypt.Subkey(master, crypt.DataInfo)
	}

	path := filepath.Join(dir, masterKeysFile)
	rows, err := loadMasterRows(path)
	if err != nil {
		return nil, err
	}

	var master []byte
	if len(rows) == 0 {
		// First use: mint a master and wrap it under every available pool key.
		if master, err = crypt.NewMaster(); err != nil {
			return nil, err
		}
		for _, k := range poolKeys {
			row, werr := crypt.WrapMaster(master, k)
			if werr != nil {
				return nil, fmt.Errorf("db: wrapping master under key %q: %w", k.ID, werr)
			}
			rows = append(rows, row)
		}
		if err := saveMasterRows(path, rows); err != nil {
			return nil, err
		}
	} else {
		if master, err = crypt.OpenMaster(rows, poolKeys); err != nil {
			return nil, fmt.Errorf("db: opening encrypted database: %w", err)
		}
		// Rotation: add a wrapping for any available pool key not yet on file, so a
		// newly-provisioned key can open the DB on the next start.
		if added := addMissingWraps(&rows, master, poolKeys); added {
			if err := saveMasterRows(path, rows); err != nil {
				return nil, err
			}
		}
	}
	return crypt.Subkey(master, crypt.DataInfo)
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
