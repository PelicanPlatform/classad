package collections

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// A wire-row read must hand out a sealed value only to a reader entitled to it.
//
// The attribute under test is encrypted at rest (EncryptedAttrs) but NOT private by name, which is
// the combination that used to leak: the subset assembler's keep test filters private NAMES, so such
// an attribute passes it, and the scans handed the assembler the collection's key whatever the
// caller asked for. Only one cell of the matrix below leaked on any given commit -- the serial path
// leaked while the parallel path passed no key at all and was accidentally safe, then the parallel
// path was given the key unconditionally and leaked too -- so all four combinations are pinned, in
// both the entitled and the redacted direction.
func TestWireRowSealedValueNeedsEntitlement(t *testing.T) {
	const secret = "SUPERSECRET"
	for _, interned := range []bool{false, true} {
		for _, parallel := range []bool{true, false} {
			name := "inline"
			if interned {
				name = "interned"
			}
			if parallel {
				name += "/parallel"
			} else {
				name += "/serial"
			}
			t.Run(name, func(t *testing.T) {
				c, n := sealedProbeStore(t, interned, parallel)
				proj := []string{"ClusterId", "Owner", "Note"}

				// Redacted: no plaintext anywhere, and the row is still served -- a
				// secret the caller may not read must become undefined, not make the
				// whole ad disappear.
				rows, leaked := scanWireCounting(c, proj, true, secret)
				if leaked != 0 {
					t.Errorf("redacted wire read leaked %d plaintext secrets", leaked)
				}
				if rows != n {
					t.Errorf("redacted wire read returned %d rows, want %d", rows, n)
				}

				// Entitled: the real value is there.
				rows, leaked = scanWireCounting(c, proj, false, secret)
				if leaked != n {
					t.Errorf("entitled wire read produced %d plaintext values, want %d", leaked, n)
				}
				if rows != n {
					t.Errorf("entitled wire read returned %d rows, want %d", rows, n)
				}

				// Not leaking the plaintext is not the same as never holding it.
				// Filtering a decrypted secret out of the output afterwards is a
				// deny-list; declining the key is not, and only the second survives
				// someone adding an attribute the name filter does not know about.
				// So assert the key is never USED on a redacted read.
				ctr := &countingSealer{inner: c.sealer}
				c.sealer = ctr
				if _, _ = scanWireCounting(c, proj, true, secret); ctr.opens.Load() != 0 {
					t.Errorf("redacted wire read opened %d sealed values; it should hold no key",
						ctr.opens.Load())
				}
				if n := ctr.seals.Load(); n != 0 {
					t.Errorf("redacted wire read sealed %d values; a read should seal nothing", n)
				}
				ctr.opens.Store(0)
				ctr.seals.Store(0)
				if _, _ = scanWireCounting(c, proj, false, secret); ctr.opens.Load() == 0 {
					t.Error("entitled wire read opened nothing; the probe is not reaching the key")
				}
				// A read re-sealing values is pure waste: nothing it produces is stored.
				if n := ctr.seals.Load(); n != 0 {
					t.Errorf("entitled wire read re-sealed %d values; a read should seal nothing", n)
				}

				// The text scans are the reference for both directions.
				if got := textLeakCount(t, c, proj, true, secret); got != 0 {
					t.Errorf("redacted TEXT read leaked %d plaintext secrets", got)
				}
				if got := textLeakCount(t, c, proj, false, secret); got != n {
					t.Errorf("entitled TEXT read produced %d plaintext values, want %d", got, n)
				}
			})
		}
	}
}

func sealedProbeStore(t *testing.T, interned, parallel bool) (*Collection, int) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	c, err := Open(Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16,
		DataKey: key, EncryptedAttrs: []string{"Note"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	// Pick the path under test rather than hoping for it: a probe that silently
	// measures the serial emitter twice proves nothing about the parallel one.
	if parallel {
		c.parallelMinBytes = 0
	} else {
		c.parallelMinBytes = 1 << 40
	}
	const n = 600
	var batch []OldAdUpdate
	for i := 0; i < n; i++ {
		batch = append(batch, OldAdUpdate{
			Key: []byte(fmt.Sprintf("k%d", i)),
			Text: fmt.Sprintf("ClusterId = %d\nProcId = 0\nOwner = \"u%d\"\n"+
				"Note = \"SUPERSECRET-%d\"\nPad = \"%0256d\"", i, i%7, i, i),
		})
	}
	if err := c.UpdateOld(batch); err != nil {
		t.Fatal(err)
	}
	if interned {
		if _, err := c.RetrainDict(4000); err != nil {
			t.Fatal(err)
		}
	}
	return c, n
}

func scanWireCounting(c *Collection, proj []string, redact bool, needle string) (rows, hits int) {
	for row := range c.ScanRawWire(proj, redact) {
		rows++
		if bytes.Contains(row, []byte(needle)) {
			hits++
		}
	}
	return rows, hits
}

func textLeakCount(t *testing.T, c *Collection, proj []string, redact bool, needle string) int {
	t.Helper()
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	for ad := range c.QueryRawProjected(q, proj, false, redact) {
		for _, e := range ad.Exprs {
			if bytes.Contains(e, []byte(needle)) {
				hits++
			}
		}
	}
	return hits
}

// countingSealer counts key use without changing it.
type countingSealer struct {
	inner        wire.Sealer
	opens, seals atomic.Int64
}

func (s *countingSealer) Seal(pt []byte) (nonce, ct []byte, err error) {
	s.seals.Add(1)
	return s.inner.Seal(pt)
}

func (s *countingSealer) Open(nonce, ct []byte) ([]byte, error) {
	s.opens.Add(1)
	return s.inner.Open(nonce, ct)
}
