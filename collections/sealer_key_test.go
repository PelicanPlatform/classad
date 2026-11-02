package collections

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"testing"
)

// refSealer is a wire.Sealer built straight from the standard library, per call, sharing nothing with
// crypt or the collection's own sealer. Reading a store through it tells which key its values were
// really sealed under, even if the store's cipher handling had gone wrong.
type refSealer struct{ key []byte }

func (s refSealer) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s refSealer) Seal(pt []byte) (nonce, ct []byte, err error) {
	g, err := s.gcm()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, pt, nil), nil
}

func (s refSealer) Open(nonce, ct []byte) ([]byte, error) {
	g, err := s.gcm()
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, nil)
}

// TestSealerBindsItsKey: a store seals under the data key it was opened with, and only that key, when
// another store in the same process holds a different one -- a mutable table and an archive, each
// written before and after the other store's sealer exists. The values are then read back through a
// reference sealer: under the store's own key every one opens, under the other key none does.
func TestSealerBindsItsKey(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	_, k1 := deriveDataKey(t)
	_, k2 := deriveDataKey(t)
	enc := []string{"Secret"}
	secret := func(store string, i int) string { return fmt.Sprintf("%s-secret-%d", store, i) }

	t.Run("mutable", func(t *testing.T) {
		open := func(key []byte) *Collection {
			c, err := Open(Options{Shards: 1, Dir: t.TempDir(), DataKey: key, EncryptedAttrs: enc})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			return c
		}
		put := func(c *Collection, store string, i int) {
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; Secret=%q]`, i, secret(store, i)))); err != nil {
				t.Fatal(err)
			}
		}
		c1 := open(k1)
		put(c1, "one", 0)
		c2 := open(k2)
		put(c2, "two", 0)
		put(c1, "one", 1)
		put(c2, "two", 1)

		read := func(c *Collection, store string, key []byte) int {
			saved := c.sealer
			c.sealer = refSealer{key}
			defer func() { c.sealer = saved }()
			n := 0
			for i := 0; i < 2; i++ {
				if ad, ok := c.Get([]byte(fmt.Sprintf("k%d", i))); ok {
					if v, _ := ad.EvaluateAttrString("Secret"); v == secret(store, i) {
						n++
					}
				}
			}
			return n
		}
		for _, s := range []struct {
			name       string
			c          *Collection
			own, other []byte
		}{{"one", c1, k1, k2}, {"two", c2, k2, k1}} {
			if n := read(s.c, s.name, s.own); n != 2 {
				t.Errorf("store %s: %d of 2 values open under its own key", s.name, n)
			}
			if n := read(s.c, s.name, s.other); n != 0 {
				t.Errorf("store %s: %d of 2 values open under the other store's key", s.name, n)
			}
		}
	})

	t.Run("archive", func(t *testing.T) {
		open := func(key []byte) *Archive {
			a, err := OpenArchive(ArchiveOptions{Dir: t.TempDir(), DataKey: key, EncryptedAttrs: enc})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close() })
			return a
		}
		app := func(a *Archive, store string, i int) {
			if err := a.Append(mustAd(t, fmt.Sprintf(`[Id=%d; Secret=%q]`, i, secret(store, i)))); err != nil {
				t.Fatal(err)
			}
		}
		a1 := open(k1)
		app(a1, "one", 0)
		a2 := open(k2)
		app(a2, "two", 0)
		app(a1, "one", 1)
		app(a2, "two", 1)

		read := func(a *Archive, store string, key []byte) int {
			saved := a.c.sealer
			a.c.sealer = refSealer{key}
			defer func() { a.c.sealer = saved }()
			n := 0
			for ad := range a.Query(mustQuery(t, "true")) {
				id, _ := ad.EvaluateAttrInt("Id")
				if v, _ := ad.EvaluateAttrString("Secret"); v == secret(store, int(id)) {
					n++
				}
			}
			return n
		}
		for _, s := range []struct {
			name       string
			a          *Archive
			own, other []byte
		}{{"one", a1, k1, k2}, {"two", a2, k2, k1}} {
			if n := read(s.a, s.name, s.own); n != 2 {
				t.Errorf("archive %s: %d of 2 values open under its own key", s.name, n)
			}
			if n := read(s.a, s.name, s.other); n != 0 {
				t.Errorf("archive %s: %d of 2 values open under the other archive's key", s.name, n)
			}
		}
	})
}
