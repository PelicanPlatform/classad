package collections

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/crypt"
	"github.com/PelicanPlatform/classad/collections/vm"
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
// A sealed value refers to no other attribute. That is a contract on what gets encrypted, not
// something checked: private attributes are literal secrets, and an attribute in the explicit
// set is expected to be one too. Every read relies on it -- the closure checks (adClosed), the
// wire-row and text projections that chase references, and the partial decode a query runs on all
// stop at a sealed value -- so a sealed expression over another attribute is evaluated without
// that attribute unless the read loaded it anyway. Honouring the reference instead would mean
// opening every sealed value just to learn what else to load, on every path, which is what the
// contract avoids.
//
// An attribute is encrypted when encryption is enabled AND it is either private (an
// HTCondor secret -- claim ids, capabilities, transfer keys, _condor_priv*; always
// encrypted, non-negotiable) or in the human-toggled explicit set. Privateness is a
// compiled-in, name-only property and classad.IsPrivateAttribute is written for exactly this
// call frequency -- a length gate and a first-byte switch reject almost every name before any
// map is touched, with no allocation -- so it is called directly, per attribute. Memoizing it
// in a per-collection sync.Map measured 2.5x SLOWER than the predicate it cached (12.0ns vs
// 4.9ns per name over a realistic job ad's attributes): hashing the whole name costs more than
// the two comparisons it replaces, and the cache grew without bound over an unbounded key
// space. The explicit set can change at runtime (SetEncryptedAttrs), so it is consulted live.

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
// It is only consulted when a sealer is present (encodeAd guards that, and the encoders
// short-circuit on a nil Sealer), so it need not re-check. An attribute is encrypted if it
// is private or in the explicit set.
func (c *Collection) shouldEncrypt(name string) bool {
	if classad.IsPrivateAttribute(name) {
		return true
	}
	if set := c.encryptedAttrs(); set != nil {
		return foldedHas(set, name)
	}
	return false
}

// foldedHas reports whether set (case-folded names) holds name, case-insensitively. Folding into a
// stack buffer keeps it allocation-free for ordinary attribute names: the keyless reads of an archive
// consult the sealing policy per attribute looked up (see keylessHides), where strings.ToLower's
// allocation for every mixed-case name would be a cost per record.
func foldedHas[T string | []byte](set map[string]struct{}, name T) bool {
	var buf [64]byte
	if len(name) > len(buf) {
		_, ok := set[strings.ToLower(string(name))]
		return ok
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b >= 0x80 {
			_, ok := set[strings.ToLower(string(name))]
			return ok
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		buf[i] = b
	}
	_, ok := set[string(buf[:len(name)])]
	return ok
}

// hidesClearSealed reports whether a read not entitled to sealed values must also withhold, by name,
// the values of sealed attributes that are stored in the clear.
//
// Sealing is applied when a record is encoded, so a record written before an attribute was sealed --
// before the store had a key at all, or before the attribute joined EncryptedAttrs -- still holds its
// value in the clear. A mutable table rewrites such records when it opens (MigrateSealedAttrs), so by
// the time it serves a read every value the policy names is sealed and the keyless decode alone keeps
// it from an unentitled reader. An append-only store does not: rewriting a history to seal it is a
// rewrite of the whole history, so its legacy records are resealed only when maintenance rewrites
// them anyway (a merge, a retrain, interning). Until then the keyless decode would serve them, so its
// keyless reads also treat every attribute the policy names as sealed, whatever form it is stored in.
//
// An in-memory store never seals (see SealsValues), so it withholds nothing here either.
func (c *Collection) hidesClearSealed() bool { return c.appendOnly() && c.SealsValues() }

// keylessHides reports whether a read not entitled to sealed values must read name as undefined
// whatever its stored form (see hidesClearSealed).
func (c *Collection) keylessHides(name string) bool {
	return c.hidesClearSealed() && c.shouldEncrypt(name)
}

// keylessHidesBytes is keylessHides for a name held as bytes, for a renderer walking a record's
// stored names.
func (c *Collection) keylessHidesBytes(name []byte) bool {
	if !c.hidesClearSealed() {
		return false
	}
	if isPrivateNameBytes(name) {
		return true
	}
	set := c.encryptedAttrs()
	return set != nil && foldedHas(set, name)
}

// keylessQuery is q as a read not entitled to sealed values must plan and evaluate it: with every
// reference to an attribute the policy seals replaced by undefined when the store may hold such values
// in the clear (see hidesClearSealed and vm.Query.Hiding), and q itself otherwise.
func (c *Collection) keylessQuery(q *vm.Query) *vm.Query {
	if q == nil || !c.hidesClearSealed() {
		return q
	}
	return q.Hiding(c.shouldEncrypt)
}

// keylessHidesAny reports whether any of names is one keylessHides withholds.
func (c *Collection) keylessHidesAny(names ...string) bool {
	if !c.hidesClearSealed() {
		return false
	}
	for _, n := range names {
		if c.shouldEncrypt(n) {
			return true
		}
	}
	return false
}

// anySealedAttr reports whether any of the interned attribute ids this collection just streamed names an
// attribute it would seal. Used to defer a streaming ingest to the sealing path for that ad only (see
// encodeOld); the id set is one ad's worth.
func (c *Collection) anySealedAttr(seen map[uint32]struct{}) bool {
	for id := range seen {
		name, ok := c.intern.Name(id)
		if !ok {
			// An id whose name will not resolve cannot be checked, so it is treated as sealable: a
			// wrong guess here stores a secret in the clear.
			return true
		}
		if c.shouldEncrypt(name) {
			return true
		}
	}
	return false
}

// EncryptionEnabled reports whether this collection has a data key (encryption at
// rest is active). Without it, EncryptedAttrs is inert.
func (c *Collection) EncryptionEnabled() bool { return c.sealer != nil }

// SealsValues reports whether any stored value can be sealed: a key, and a persistent collection. An
// in-memory collection may hold a key but never seals -- its records are interned against the shared
// table and encoded without one -- so a reader not entitled to sealed values is withheld nothing there.
func (c *Collection) SealsValues() bool { return c.sealer != nil && c.inline }

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
// The named attributes' values must not refer to other attributes; see
// Options.EncryptedAttrs.
// New records use the new set immediately; existing records keep their prior form until
// MigrateSealedAttrs rewrites them. Compaction does not: it copies stored values as they are.
// The migration's marker records the set it ran under, so the next open after a change runs it.
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
