package collections

import (
	"strings"

	"github.com/PelicanPlatform/classad/collections/crypt"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Encryption at rest. A single DB-wide data key seals the designated attributes'
// value nodes (the wire nEncrypted node); the rest of an ad stays in the clear so it
// still indexes and matches. The data key is the HKDF-derived DataInfo subkey of the
// DB master key -- it is deliberately DISTINCT from the master (the master only wraps
// pool keys and derives subkeys; it never seals attribute data). Because one key
// serves the whole DB, ciphertext is portable across segments, so compaction moves
// encrypted records unchanged (no re-encryption, no per-segment key). Encrypted
// attributes are never indexed and never hot -- they are opaque to the fast path.

// dataKeySealer adapts the DB data key to wire.Sealer via crypt's AES-256-GCM.
type dataKeySealer struct{ key []byte }

func (s dataKeySealer) Seal(pt []byte) (nonce, ct []byte, err error) { return crypt.Seal(s.key, pt) }
func (s dataKeySealer) Open(nonce, ct []byte) ([]byte, error) {
	return crypt.Open(s.key, nonce, ct)
}

// newDataKeySealer builds the DB-wide Sealer from a data key. The key must be the
// master's DataInfo subkey (crypt.Subkey(master, crypt.DataInfo)), never the master
// itself. A nil/empty key returns nil (encryption disabled).
func newDataKeySealer(dataKey []byte) wire.Sealer {
	if len(dataKey) == 0 {
		return nil
	}
	return dataKeySealer{append([]byte(nil), dataKey...)}
}

// encSetHolder is the immutable, atomically-swappable set of case-folded attribute
// names to encrypt at rest. A meta-command that toggles an attribute installs a new
// holder so the write path reads it lock-free.
type encSetHolder struct {
	set map[string]struct{}
}

func foldedSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = struct{}{}
	}
	return m
}

// encryptedAttrs returns the current case-folded encrypted-attribute set, or nil if
// none / encryption is disabled.
func (c *Collection) encryptedAttrs() map[string]struct{} {
	h := c.encAttrs.Load()
	if h == nil {
		return nil
	}
	return h.set
}

// EncryptionEnabled reports whether this collection has a data key (encryption at
// rest is active). Without it, EncryptedAttrs is inert.
func (c *Collection) EncryptionEnabled() bool { return c.sealer != nil }
