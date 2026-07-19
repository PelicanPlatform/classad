package collections

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSystemKeyHiddenFromScansButRetrievable verifies that a system-keyed record is
// stored and updated like any record, retrievable by explicit key, and preserved by
// compaction, yet excluded from every client-facing scan/query/iteration path and
// visible only through the dedicated ForEachSystemAd sweep.
func TestSystemKeyHiddenFromScansButRetrievable(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4})

	const n = 12
	normalKeys := map[string]bool{}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("job%d", i)
		normalKeys[k] = true
		if err := c.Put([]byte(k), mustAd(t, fmt.Sprintf(`[Owner="alice"; Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	sysKey := SystemKey("idem/marker-1")
	if !IsSystemKey(sysKey) {
		t.Fatalf("SystemKey did not produce a system key: %q", sysKey)
	}
	if IsSystemKey("job0") || IsSystemKey("") {
		t.Fatal("IsSystemKey misclassified a normal/empty key")
	}
	if err := c.Put([]byte(sysKey), mustAd(t, `[Owner="system"; Marker=1; Ttl=100]`)); err != nil {
		t.Fatal(err)
	}

	// Explicit lookup still finds the system record (writes/reads are unchanged).
	if _, ok := c.Get([]byte(sysKey)); !ok {
		t.Fatal("Get(systemKey) did not find the system record")
	}
	// Len counts every stored record, including the system one.
	if got := c.Len(); got != n+1 {
		t.Fatalf("Len = %d, want %d", got, n+1)
	}

	hasMarker := func(ad *classad.ClassAd) bool {
		_, ok := ad.Lookup("Marker")
		return ok
	}
	rawHasMarker := func(exprs [][]byte) bool {
		for _, e := range exprs {
			if bytes.Contains(e, []byte("Marker")) {
				return true
			}
		}
		return false
	}

	// Scan: exactly the normal ads, never the system record.
	{
		count := 0
		for ad := range c.Scan() {
			count++
			if hasMarker(ad) {
				t.Error("Scan yielded the system record")
			}
		}
		if count != n {
			t.Errorf("Scan returned %d ads, want %d", count, n)
		}
	}

	// Query (match-all)
	{
		q := mustParseQuery(t, "true")
		count := 0
		for ad := range c.Query(q) {
			count++
			if hasMarker(ad) {
				t.Error("Query(true) yielded the system record")
			}
		}
		if count != n {
			t.Errorf("Query(true) returned %d ads, want %d", count, n)
		}
	}

	// ScanRaw
	{
		count := 0
		for ra := range c.ScanRaw() {
			count++
			if rawHasMarker(ra.Exprs) {
				t.Error("ScanRaw yielded the system record")
			}
		}
		if count != n {
			t.Errorf("ScanRaw returned %d ads, want %d", count, n)
		}
	}

	// QueryRaw (match-all)
	{
		q := mustParseQuery(t, "true")
		count := 0
		for ra := range c.QueryRaw(q) {
			count++
			if rawHasMarker(ra.Exprs) {
				t.Error("QueryRaw(true) yielded the system record")
			}
		}
		if count != n {
			t.Errorf("QueryRaw(true) returned %d ads, want %d", count, n)
		}
	}

	// ForEachAd: yields normal keys only.
	{
		seen := map[string]bool{}
		c.ForEachAd(func(key string, _ *classad.ClassAd) bool {
			if IsSystemKey(key) {
				t.Errorf("ForEachAd exposed system key %q", key)
			}
			seen[key] = true
			return true
		})
		for k := range normalKeys {
			if !seen[k] {
				t.Errorf("ForEachAd omitted normal key %q", k)
			}
		}
		if len(seen) != n {
			t.Errorf("ForEachAd yielded %d keys, want %d", len(seen), n)
		}
	}

	// ForEachSystemAd: yields ONLY the system record.
	{
		seen := map[string]bool{}
		c.ForEachSystemAd(func(key string, ad *classad.ClassAd) bool {
			if !IsSystemKey(key) {
				t.Errorf("ForEachSystemAd exposed non-system key %q", key)
			}
			if !hasMarker(ad) {
				t.Errorf("ForEachSystemAd yielded an ad without the Marker attr: %s", ad.String())
			}
			seen[key] = true
			return true
		})
		if len(seen) != 1 || !seen[sysKey] {
			t.Errorf("ForEachSystemAd yielded %v, want exactly {%q}", keysOf(seen), sysKey)
		}
	}

	// Compaction preserves the system record: churn to build garbage, force compaction,
	// then confirm the marker is still retrievable and still hidden from scans.
	for round := 0; round < 30; round++ {
		for i := 0; i < n; i++ {
			if err := c.Put([]byte(fmt.Sprintf("job%d", i)), mustAd(t, fmt.Sprintf(`[Owner="alice"; Id=%d; R=%d]`, i, round))); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Put([]byte(sysKey), mustAd(t, fmt.Sprintf(`[Owner="system"; Marker=1; Ttl=%d]`, round))); err != nil {
			t.Fatal(err)
		}
	}
	c.Rewrite() // re-encode + force-compact every shard
	if _, ok := c.Get([]byte(sysKey)); !ok {
		t.Fatal("system record lost after compaction/Rewrite")
	}
	sysCount := 0
	c.ForEachSystemAd(func(key string, _ *classad.ClassAd) bool { sysCount++; return true })
	if sysCount != 1 {
		t.Errorf("after compaction ForEachSystemAd yielded %d records, want 1", sysCount)
	}
	scanCount := 0
	for ad := range c.Scan() {
		scanCount++
		if hasMarker(ad) {
			t.Error("Scan yielded the system record after compaction")
		}
	}
	if scanCount != n {
		t.Errorf("after compaction Scan returned %d ads, want %d", scanCount, n)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
