package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// BenchmarkWholeRecordCompose times one patch write that needs a whole record (a removal, delta records
// off) on an inline encrypted store, as an entitled transaction composes it (decode with the key, merge,
// re-encode and re-seal) and as a redacted one does (splice the stored bytes, nothing opened). Encode
// only; nothing is committed.
func BenchmarkWholeRecordCompose(b *testing.B) {
	if !mmapSupported {
		b.Skip("persistence is unix-only")
	}
	for _, arm := range []struct {
		name   string
		redact bool
	}{{"entitled", false}, {"redacted", true}} {
		b.Run(arm.name, func(b *testing.B) {
			c, err := Open(sealedParityOpts(b.TempDir()))
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			sealedPut(b, c, 0, sealedParityN)
			c.deltaMax = 0
			patch := classad.New()
			patch.InsertAttr("JobStatus", 3)
			removed := []string{"Filler0"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("%d.0", i%sealedParityN)
				tx := c.Begin()
				if arm.redact {
					tx = c.BeginRedacted()
				}
				tx.PatchAttrs([]byte(key), patch, removed)
				if _, _, ok, why := tx.encodePatchOnly(nil, tx.writes[key], c.h.Hash([]byte(key))); !ok {
					b.Fatalf("%s refused: %s", key, why)
				}
			}
		})
	}
}
