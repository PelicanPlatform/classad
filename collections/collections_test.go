package collections

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

func mustAd(t testing.TB, src string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return ad
}

func TestPutGetDelete(t *testing.T) {
	c := New(Options{Shards: 4})
	if _, ok := c.Get([]byte("missing")); ok {
		t.Fatal("get of missing key reported found")
	}
	if err := c.Put([]byte("k1"), mustAd(t, `[Owner="alice"; Cpus=4]`)); err != nil {
		t.Fatal(err)
	}
	ad, ok := c.Get([]byte("k1"))
	if !ok {
		t.Fatal("k1 not found after put")
	}
	if !ad.Equal(mustAd(t, `[Owner="alice"; Cpus=4]`)) {
		t.Errorf("round-trip mismatch: %s", ad.String())
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}

	// Update replaces the value, keeps the count.
	if err := c.Put([]byte("k1"), mustAd(t, `[Owner="bob"; Cpus=8]`)); err != nil {
		t.Fatal(err)
	}
	ad, _ = c.Get([]byte("k1"))
	if !ad.Equal(mustAd(t, `[Owner="bob"; Cpus=8]`)) {
		t.Errorf("update not reflected: %s", ad.String())
	}
	if c.Len() != 1 {
		t.Errorf("Len after update = %d, want 1", c.Len())
	}

	// Delete.
	if !c.Delete([]byte("k1")) {
		t.Fatal("delete reported not present")
	}
	if _, ok := c.Get([]byte("k1")); ok {
		t.Fatal("k1 present after delete")
	}
	if c.Delete([]byte("k1")) {
		t.Fatal("second delete reported present")
	}
	if c.Len() != 0 {
		t.Errorf("Len after delete = %d, want 0", c.Len())
	}
}

// constHasher forces every key into one shard and one directory bucket, so the
// collision chain (inline-key comparison) is exercised for all operations.
type constHasher struct{}

func (constHasher) Hash([]byte) uint64 { return 0x1234 }

func TestCollisionChain(t *testing.T) {
	c := New(Options{Shards: 8, Hasher: constHasher{}})
	const n = 50
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("key-%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != n {
		t.Fatalf("Len = %d, want %d", c.Len(), n)
	}
	// Every colliding key resolves to its own value.
	for i := 0; i < n; i++ {
		ad, ok := c.Get([]byte(fmt.Sprintf("key-%d", i)))
		if !ok {
			t.Fatalf("key-%d missing from collision chain", i)
		}
		got, _ := ad.EvaluateAttrInt("Id")
		if got != int64(i) {
			t.Fatalf("key-%d -> Id %d, want %d", i, got, i)
		}
	}
	// Update and delete within the chain.
	if err := c.Put([]byte("key-10"), mustAd(t, `[Id=999]`)); err != nil {
		t.Fatal(err)
	}
	ad, _ := c.Get([]byte("key-10"))
	if got, _ := ad.EvaluateAttrInt("Id"); got != 999 {
		t.Fatalf("chain update failed: got %d", got)
	}
	if !c.Delete([]byte("key-25")) {
		t.Fatal("chain delete failed")
	}
	if _, ok := c.Get([]byte("key-25")); ok {
		t.Fatal("key-25 present after chain delete")
	}
	if c.Len() != n-1 {
		t.Fatalf("Len = %d, want %d", c.Len(), n-1)
	}
}

func TestScanAllOnce(t *testing.T) {
	c := New(Options{Shards: 8})
	const n = 1000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	seen := make([]int, n)
	total := 0
	for ad := range c.Scan() {
		id, ok := ad.EvaluateAttrInt("Id")
		if !ok || id < 0 || id >= n {
			t.Fatalf("bad Id %d", id)
		}
		seen[id]++
		total++
	}
	if total != n {
		t.Fatalf("scan yielded %d ads, want %d", total, n)
	}
	for i, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("Id %d seen %d times, want 1", i, cnt)
		}
	}
}

func TestQuery(t *testing.T) {
	c := New(Options{Shards: 4})
	for i := 0; i < 100; i++ {
		src := fmt.Sprintf(`[Id=%d; Cpus=%d; Owner=%q]`, i, i%8, []string{"alice", "bob"}[i%2])
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	q, err := vm.Parse(`Cpus >= 4 && Owner == "alice"`)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	want := 0
	for i := 0; i < 100; i++ {
		if i%8 >= 4 && i%2 == 0 {
			want++
		}
	}
	for ad := range c.Query(q) {
		cpus, _ := ad.EvaluateAttrInt("Cpus")
		owner, _ := ad.EvaluateAttrString("Owner")
		if cpus < 4 || owner != "alice" {
			t.Errorf("query returned non-matching ad: %s", ad.String())
		}
		got++
	}
	if got != want {
		t.Errorf("query matched %d, want %d", got, want)
	}
}

// TestScanExactlyOnceUnderUpdates is the M3 invariant: every key present when a
// shard's scan begins is yielded exactly once, even while concurrent updaters
// churn existing keys.
func TestScanExactlyOnceUnderUpdates(t *testing.T) {
	c := New(Options{Shards: 8})
	const n = 2000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, fmt.Sprintf(`[Id=%d; C=0]`, i))); err != nil {
			t.Fatal(err)
		}
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			counter := 1
			for x := seed; !stop.Load(); x += 7 {
				k := x % n
				_ = c.Put([]byte(fmt.Sprintf("k%d", k)), mustAd(t, fmt.Sprintf(`[Id=%d; C=%d]`, k, counter)))
				counter++
			}
		}(w * 500)
	}

	seen := make([]int, n)
	total := 0
	for ad := range c.Scan() {
		id, ok := ad.EvaluateAttrInt("Id")
		if !ok || id < 0 || id >= n {
			t.Fatalf("bad Id %d", id)
		}
		seen[id]++
		total++
	}
	stop.Store(true)
	wg.Wait()

	for i, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("Id %d seen %d times, want exactly 1 (no dup, no skip)", i, cnt)
		}
	}
	if total != n {
		t.Fatalf("scan yielded %d ads, want %d", total, n)
	}
}
