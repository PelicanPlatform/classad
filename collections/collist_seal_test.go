package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// listSealAd is a slot-shaped record whose ChildRemoteUser list is the attribute the policy will seal.
func listSealAd(i int) string {
	return fmt.Sprintf(`[ ClusterId = %d; ProcId = %d; JobStatus = %d; Cpus = %d;
		ChildRemoteUser = {"user%d@ap", "user%d@ap"}; ChildCpus = {1, %d}; Secret = {"s%d"}; Early = {"e%d", "x"} ]`,
		i, i%10, 1+i%5, 1+i%8, i%7, (i+1)%7, i%4, i%3, i%5)
}

// listFieldIn reports how many base blocks of c's columnarized segments keep a list column of name.
func listFieldIn(c *Collection, name string) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil {
				for _, b := range cs.blocks {
					for _, nm := range b.listNames {
						if nm == name {
							n++
						}
					}
				}
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// TestListColumnsSealedPolicy: a list the policy names is never copied into a column -- not when it is
// sealed at write, and not from a clear record once the policy names it -- and a list copied while the
// attribute was in the clear is not read for a reader not entitled to sealed values after the policy is
// widened to it. A migration then rewrites the segments, and no column of it remains.
func TestListColumnsSealedPolicy(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = []string{"Secret"}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 400
	tx := c.Begin()
	for i := range n {
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, listSealAd(i)))
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("put conflicted")
	}
	internAll(c)
	// Early is written in the clear and named by the policy before the segments are columnarized: its
	// records still hold it in the clear, and no column may copy it.
	if err := c.SetEncryptedAttrs([]string{"Secret", "Early"}); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	if listFieldIn(c, "ChildRemoteUser") == 0 || listFieldIn(c, "ChildCpus") == 0 {
		t.Fatal("fixture: the clear lists have no column")
	}
	if k := listFieldIn(c, "Secret"); k != 0 {
		t.Fatalf("a list sealed at write has a column in %d blocks", k)
	}
	if k := listFieldIn(c, "Early"); k != 0 {
		t.Fatalf("a clear list the policy names has a column in %d blocks", k)
	}
	q := mustParseQ(t, `member("user3@ap", ChildRemoteUser)`)
	want := 0
	for i := range n {
		if i%7 == 3 || (i+1)%7 == 3 {
			want++
		}
	}
	count := func(redact bool) (int, bool) { return c.countQueryAs(q, redact) }
	if got, ok := count(true); !ok || got != want {
		t.Fatalf("before the widen the redacted count is %d (ok=%v), want %d", got, ok, want)
	}
	served := vm.ListFusedCalls.Load()
	if err := c.SetEncryptedAttrs([]string{"Secret", "Early", "ChildRemoteUser"}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{`member("user3@ap", ChildRemoteUser)`, "size(ChildRemoteUser) == 2", "isList(ChildRemoteUser)",
		"ChildRemoteUser isnt undefined"} {
		q := mustParseQ(t, e)
		if got, ok := c.countQueryAs(q, true); ok && got != 0 {
			t.Errorf("after the widen the redacted count of %s is %d, want 0", e, got)
		}
		if got, ok := c.vectorEvalCountAs(q, true); ok && got != 0 {
			t.Errorf("after the widen the redacted vector count of %s is %d, want 0", e, got)
		}
		if got := len(readAllRedacted(t, c, e)); got != 0 {
			t.Errorf("after the widen the redacted query %s returned %d records, want 0", e, got)
		}
	}
	if got, ok := count(false); !ok || got != want {
		t.Errorf("after the widen the entitled count is %d (ok=%v), want %d", got, ok, want)
	}
	if vm.ListFusedCalls.Load() == served {
		t.Error("the entitled count did not read the list column")
	}
	if c.MigrateSealedAttrs(0) == 0 {
		t.Fatal("the migration rewrote nothing")
	}
	c.ColumnarizeSealed()
	if k := listFieldIn(c, "ChildRemoteUser"); k != 0 {
		t.Errorf("after the migration %d blocks keep a column of the sealed ChildRemoteUser", k)
	}
	if listFieldIn(c, "ChildCpus") == 0 {
		t.Error("after the migration the clear ChildCpus lost its column")
	}
	if got, ok := count(false); !ok || got != want {
		t.Errorf("after the migration the entitled count is %d (ok=%v), want %d", got, ok, want)
	}
}

// readAllRedacted is readAll for a reader not entitled to sealed values.
func readAllRedacted(t *testing.T, c *Collection, expr string) []string {
	t.Helper()
	q := mustParseQ(t, expr)
	var out []string
	for ad := range c.QueryRedacted(q) {
		out = append(out, ad.String())
	}
	return out
}
