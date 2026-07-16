package collections

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/crypt"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// errEncryptionDisabled is returned by SetEncryptedAttrs when the collection has no
// data key (encryption at rest is off), so there is nothing to seal with.
var errEncryptionDisabled = errors.New("collections: encryption at rest is not enabled (no data key)")

// Encryption at rest. A single DB-wide data key seals the designated attributes'
// value nodes (the wire nEncrypted node); the rest of an ad stays in the clear so it
// still indexes and matches. The data key is the HKDF-derived DataInfo subkey of the
// DB master key -- it is deliberately DISTINCT from the master (the master only wraps
// pool keys and derives subkeys; it never seals attribute data). Because one key
// serves the whole DB, ciphertext is portable across segments, so compaction moves
// encrypted records unchanged (no re-encryption, no per-segment key). Encrypted
// attributes are never indexed and never hot -- they are opaque to the fast path.
//
// An attribute is encrypted when encryption is enabled AND it is either private (an
// HTCondor secret -- claim ids, capabilities, transfer keys, _condor_priv*; always
// encrypted, non-negotiable) or in the human-toggled explicit set. Privateness is a
// compiled-in, name-only property, so it is memoized: classad.IsPrivateAttribute is
// consulted once per distinct attribute name and the result cached (privCache). The
// explicit set can change at runtime (SetEncryptedAttrs), so it is consulted live.

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

// encryptedAttrs returns the current case-folded explicit encrypted-attribute set (not
// including the always-on private attributes), or nil if none / encryption is disabled.
func (c *Collection) encryptedAttrs() map[string]struct{} {
	h := c.encAttrs.Load()
	if h == nil {
		return nil
	}
	return h.set
}

// shouldEncrypt is the per-attribute encryption predicate handed to the wire encoder.
// It is only consulted when a sealer is present (encodeAd guards that), so it need not
// re-check. An attribute is encrypted if it is private (memoized) or in the explicit set.
func (c *Collection) shouldEncrypt(name string) bool {
	if c.attrIsPrivate(name) {
		return true
	}
	if set := c.encryptedAttrs(); set != nil {
		_, ok := set[strings.ToLower(name)]
		return ok
	}
	return false
}

// attrIsPrivate reports whether name is an HTCondor private attribute, memoizing the
// (immutable) answer so classad.IsPrivateAttribute is called once per distinct name.
func (c *Collection) attrIsPrivate(name string) bool {
	if v, ok := c.privCache.Load(name); ok {
		return v.(bool)
	}
	p := classad.IsPrivateAttribute(name)
	c.privCache.Store(name, p)
	return p
}

// EncryptionEnabled reports whether this collection has a data key (encryption at
// rest is active). Without it, EncryptedAttrs is inert.
func (c *Collection) EncryptionEnabled() bool { return c.sealer != nil }

// checkNotIndexed errors if any of names (case-insensitive) is currently an indexed
// attribute: an encrypted value is stored opaque, so it can never satisfy an index.
func (c *Collection) checkNotIndexed(names []string) error {
	if len(names) == 0 {
		return nil
	}
	cat, val := c.IndexedAttrs()
	indexed := make(map[string]struct{}, len(cat)+len(val))
	for _, a := range cat {
		indexed[strings.ToLower(a)] = struct{}{}
	}
	for _, a := range val {
		indexed[strings.ToLower(a)] = struct{}{}
	}
	for _, n := range names {
		if _, clash := indexed[strings.ToLower(n)]; clash {
			return fmt.Errorf("collections: attribute %q cannot be both encrypted and indexed", n)
		}
	}
	return nil
}

// EncryptedAttrNames returns the explicit encrypted-attribute set (case-folded, sorted)
// -- the human-toggled attributes, not the always-on private ones. Used for persistence
// and diagnostics.
func (c *Collection) EncryptedAttrNames() []string {
	set := c.encryptedAttrs()
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SetEncryptedAttrs replaces the explicit encrypted-attribute set at runtime (the
// toggle meta-command). It is a no-op-with-error if encryption is disabled, and errors
// if any named attribute is currently indexed (an encrypted value is opaque, so it
// cannot be indexed). Private attributes are always encrypted regardless of this set.
// New records use the new set immediately; existing records keep their prior form until
// rewritten (compaction/Rewrite re-encodes them under the current policy).
func (c *Collection) SetEncryptedAttrs(attrs []string) error {
	if c.sealer == nil {
		return errEncryptionDisabled
	}
	if err := c.checkNotIndexed(attrs); err != nil {
		return err
	}
	c.encAttrs.Store(&encSetHolder{set: foldedSet(attrs)})
	return nil
}
