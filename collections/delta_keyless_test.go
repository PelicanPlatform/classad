package collections

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// TestSealedUseDifferential: what a record read hands back for a reader that never opens a sealed value
// (sealedUnopened) against what it hands back for one that may (sealedOpened), every visible record of
// every sealed layout.
//
//   - A record that is not a delta comes back as the same bytes for both: the reader's use of sealed
//     values decides only how a delta chain is completed.
//   - A delta chain completed for the entitled reader decodes, with the key, to the source ad.
//   - Completed for the keyless one, it decodes with no key to exactly what the entitled bytes do --
//     every unsealed value the same, every sealed one undefined -- and was completed without the key.
func TestSealedUseDifferential(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			ctr := &countingSealer{inner: c.sealer}
			c.sealer = ctr
			deltas, records := 0, 0
			for _, sh := range c.shards {
				s0, wins := sh.snapshot()
				forEachVisibleRef(s0, wins, func(r recRef) bool {
					if isSystemKeyBytes(r.key()) {
						return true
					}
					records++
					key := string(r.key())
					i, err := strconv.Atoi(strings.TrimSuffix(key, ".0"))
					if err != nil {
						t.Fatalf("unexpected key %q", key)
					}
					before := ctr.opens.Load() + ctr.seals.Load()
					unopened, err := c.wireAt(r, s0, nil, sealedUnopened)
					if err != nil {
						t.Fatalf("%s: unopened read: %v", key, err)
					}
					unopened = slices.Clone(unopened)
					if n := ctr.opens.Load() + ctr.seals.Load() - before; n != 0 {
						t.Fatalf("%s: the unopened read used the key %d times", key, n)
					}
					opened, err := c.wireAt(r, s0, nil, sealedOpened)
					if err != nil {
						t.Fatalf("%s: opened read: %v", key, err)
					}
					if !recIsDelta(r.w.data, r.off) {
						if !bytes.Equal(opened, unopened) {
							t.Fatalf("%s: a whole record's bytes depend on the reader's use of sealed values", key)
						}
						return true
					}
					deltas++
					for _, pt := range sealedPlaintexts {
						if bytes.Contains(unopened, []byte(pt)) {
							t.Fatalf("%s: the unopened merge carries plaintext %q", key, pt)
						}
					}
					// Merged chains are inline-names, whatever segment their versions came from.
					full, err := c.decodeWireDict(nil, opened)
					if err != nil {
						t.Fatalf("%s: keyed decode of the opened merge: %v", key, err)
					}
					want := mustAd(t, sealedParityAd(i, lay.patched))
					if got, w := astRow(full), astRow(want.AST()); got != w {
						t.Fatalf("%s: opened merge\n got %s\nwant %s", key, got, w)
					}
					hidOpened, err := c.decodeWireDictAs(nil, opened, true)
					if err != nil {
						t.Fatal(err)
					}
					hidUnopened, err := c.decodeWireDictAs(nil, unopened, true)
					if err != nil {
						t.Fatalf("%s: keyless decode of the unopened merge: %v", key, err)
					}
					if got, w := astRow(hidUnopened), astRow(hidOpened); got != w {
						t.Fatalf("%s: keyless view of the two merges differs\nunopened %s\n  opened %s", key, got, w)
					}
					for _, a := range hidUnopened.Attributes {
						if !c.shouldEncrypt(a.Name) {
							continue
						}
						if _, undef := a.Value.(*ast.UndefinedLiteral); !undef {
							t.Fatalf("%s: %s reads as %s with no key", key, a.Name, a.Value)
						}
					}
					return true
				})
				releaseWindows(wins)
			}
			if records != sealedParityN {
				t.Fatalf("visited %d records, want %d", records, sealedParityN)
			}
			if lay.patched && deltas == 0 {
				t.Fatal("no delta record was read; the layout does not exercise the merge")
			}
		})
	}
}

// astRow renders an ad's attributes sorted by name, so two ads compare by content, not order.
func astRow(ad *ast.ClassAd) string {
	rows := make([]string, 0, len(ad.Attributes))
	for _, a := range ad.Attributes {
		rows = append(rows, fmt.Sprintf("%s=%s", strings.ToLower(a.Name), a.Value))
	}
	slices.Sort(rows)
	return strings.Join(rows, "; ")
}
