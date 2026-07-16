// Package crypt is the encryption-at-rest key hierarchy for a ClassAd database.
//
// A random 256-bit DB master key roots the hierarchy. It is never stored in the clear:
// it is wrapped once per available pool key (a KEK), so any one pool key opens the DB and
// the DB survives key rotation (add a row per new key). Purpose-specific subkeys are
// derived from the master via HKDF -- the data key (wraps each segment's DEK) and the
// backup key (wraps a snapshot's decryption key). Each segment gets its own random DEK,
// wrapped by the data key and stored with the segment; the DEK encrypts that segment's
// encrypted attributes with AES-256-GCM. A stolen database is useless without a pool key.
//
// This mirrors golang-htcondor's session-cache envelope, generalized for the DB.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

const (
	// KeySize is the AES-256 / master-key / DEK size in bytes.
	KeySize   = 32
	saltSize  = 16
	nonceSize = 12 // AES-GCM standard nonce
	kekInfo   = "classad-db-kek-v1"
)

// Purpose labels for master-key subkeys (HKDF context). Distinct labels keep the derived
// keys independent, so the same master protects multiple uses without key reuse.
const (
	// DataInfo derives the data key, which wraps each per-segment DEK.
	DataInfo = "classad-db-data-v1"
	// BackupInfo derives the backup key, which wraps a snapshot's decryption key.
	BackupInfo = "classad-db-backups-v1"
)

// KEK is a key-encryption key: a pool / HTCondor signing key. ID names it (e.g. "POOL");
// Material is the raw key bytes.
type KEK struct {
	ID       string
	Material []byte
}

// MasterKeyRow is the DB master key wrapped by one KEK, persisted so any available pool
// key can recover the master.
type MasterKeyRow struct {
	KeyID   string
	Salt    []byte
	Nonce   []byte
	Wrapped []byte // AES-GCM(KEK, master)
}

// NewMaster returns a fresh random 256-bit master key.
func NewMaster() ([]byte, error) { return randBytes(KeySize) }

// WrapMaster wraps master with the KEK derived from k (via a fresh salt), producing a
// persistable row.
func WrapMaster(master []byte, k KEK) (MasterKeyRow, error) {
	if len(k.Material) == 0 {
		return MasterKeyRow{}, fmt.Errorf("crypt: KEK %q has no key material", k.ID)
	}
	salt, err := randBytes(saltSize)
	if err != nil {
		return MasterKeyRow{}, err
	}
	kek, err := deriveKEK(k.Material, salt)
	if err != nil {
		return MasterKeyRow{}, err
	}
	nonce, wrapped, err := Seal(kek, master)
	if err != nil {
		return MasterKeyRow{}, err
	}
	return MasterKeyRow{KeyID: k.ID, Salt: salt, Nonce: nonce, Wrapped: wrapped}, nil
}

// ErrNoKey reports that none of the available pool keys could unwrap the master key.
var ErrNoKey = errors.New("crypt: no available pool key can decrypt the database")

// OpenMaster recovers the master key from the first row whose KeyID matches an available
// KEK and whose wrapping decrypts. Returns ErrNoKey if none match.
func OpenMaster(rows []MasterKeyRow, keys []KEK) ([]byte, error) {
	byID := make(map[string]KEK, len(keys))
	for _, k := range keys {
		byID[k.ID] = k
	}
	for _, row := range rows {
		k, ok := byID[row.KeyID]
		if !ok {
			continue
		}
		kek, err := deriveKEK(k.Material, row.Salt)
		if err != nil {
			continue
		}
		master, err := Open(kek, row.Nonce, row.Wrapped)
		if err != nil {
			continue // wrong material for this id, or tampering: try the next row
		}
		return master, nil
	}
	return nil, ErrNoKey
}

// Subkey derives a 256-bit purpose-specific key from master via HKDF-SHA256 with the
// given context label (use DataInfo / BackupInfo).
func Subkey(master []byte, info string) ([]byte, error) {
	return hkdf.Key(sha256.New, master, nil, info, KeySize)
}

// NewDEK returns a fresh random 256-bit data-encryption key (one per segment).
func NewDEK() ([]byte, error) { return randBytes(KeySize) }

// WrapDEK wraps a DEK under key (the data key); UnwrapDEK reverses it.
func WrapDEK(dek, key []byte) (nonce, wrapped []byte, err error) { return Seal(key, dek) }

// UnwrapDEK recovers a DEK wrapped by WrapDEK.
func UnwrapDEK(key, nonce, wrapped []byte) ([]byte, error) { return Open(key, nonce, wrapped) }

// Seal encrypts plaintext with AES-256-GCM under key, returning a fresh nonce and the
// ciphertext (with the GCM tag appended).
func Seal(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	g, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce, err = randBytes(nonceSize)
	if err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, plaintext, nil), nil
}

// Open decrypts a Seal result, verifying the GCM tag (returns an error on tampering or a
// wrong key).
func Open(key, nonce, ciphertext []byte) ([]byte, error) {
	g, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != g.NonceSize() {
		return nil, fmt.Errorf("crypt: bad nonce size %d", len(nonce))
	}
	return g.Open(nil, nonce, ciphertext, nil)
}

func deriveKEK(material, salt []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, material, salt, kekInfo, KeySize)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}
