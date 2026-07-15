package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// buildDNFCorpus makes n ads with a spread Disk (value) and a sparse "catalogs"
// attribute present on ~1%. indexed configures Disk (value) + catalogs (categorical),
// which the presence probe uses; unindexed forces the full scan the old planner did
// for a top-level OR.
func buildDNFCorpus(tb testing.TB, n int, indexed bool) *Collection {
	opts := Options{Shards: 8}
	if indexed {
		opts.ValueAttrs = []string{"Disk"}
		opts.CategoricalAttrs = []string{"catalogs"}
	}
	c := New(opts)
	for i := 0; i < n; i++ {
		disk := 1024 * (1 + (i*2654435761)%200) // 200 distinct values (cheap range probe)
		var text string
		if i%100 == 0 { // ~1% advertise catalogs
			text = fmt.Sprintf(`[ ID=%d; Disk=%d; catalogs="c%d" ]`, i, disk, i)
		} else {
			text = fmt.Sprintf(`[ ID=%d; Disk=%d ]`, i, disk)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			tb.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	if indexed {
		c.Reindex() // build the segment indexes
	}
	return c
}

// BenchmarkDNFUnion compares the DNF union pushdown (a selective range OR a sparse
// presence probe -- ~2% of ads) against the full scan the old planner fell back to
// for a top-level OR.
func BenchmarkDNFUnion(b *testing.B) {
	const n = 200_000
	q := mustQuery(b, `Disk >= 202752 || catalogs isnt undefined`) // Disk top ~1.5% (>=1024*198)
	for _, tc := range []struct {
		name    string
		indexed bool
	}{
		{"indexed-union", true},
		{"full-scan", false},
	} {
		c := buildDNFCorpus(b, n, tc.indexed)
		// sanity: same result count both ways
		cnt := 0
		for range c.Query(q) {
			cnt++
		}
		b.Run(fmt.Sprintf("%s/match=%d", tc.name, cnt), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				n := 0
				for range c.Query(q) {
					n++
				}
			}
		})
	}
}
