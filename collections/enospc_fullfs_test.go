//go:build unix

package collections

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestENOSPCFullFilesystem drives a persistent store against a size-limited filesystem until the
// disk fills, exercising EVERY write path (segment allocation, msync writeback, sidecars, dict,
// snapshot) under real ENOSPC at once -- the broad coverage a per-site injected fault cannot give.
//
// It is opt-in (CLASSAD_ENOSPC=1) and expects a small filesystem mounted at CLASSAD_ENOSPC_DIR (the
// nightly workflow provides a tiny tmpfs on Linux). With no dir configured it runs on an ordinary
// temp dir capped by a byte budget, which does not reach real ENOSPC but validates the harness
// (ingest, error handling, reopen, integrity assertions) end to end.
//
// Invariant asserted regardless of whether the disk actually fills: the store never returns a WRONG
// value and never loses a record it acknowledged (Put returned nil) -- a full disk may reject new
// writes, but must not corrupt or silently drop committed data, and reopen must be consistent. On a
// truly small filesystem this also demonstrates that a full disk is a clean fail-stop rather than a
// SIGBUS/silent-loss (see the fallocate hardening) -- if the process crashes here, that regression
// is exactly what this test exists to surface.
func TestENOSPCFullFilesystem(t *testing.T) {
	if os.Getenv("CLASSAD_ENOSPC") == "" {
		t.Skip("opt-in; the nightly ENOSPC job sets CLASSAD_ENOSPC=1 with a small filesystem at CLASSAD_ENOSPC_DIR")
	}
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	base := os.Getenv("CLASSAD_ENOSPC_DIR")
	onRealFS := base != ""
	if !onRealFS {
		base = t.TempDir() // harness validation on a normal FS: budget-capped, no real ENOSPC
	}
	dir, err := os.MkdirTemp(base, "enospc")
	if err != nil {
		t.Fatal(err)
	}
	budgetMB := envInt("CLASSAD_ENOSPC_BUDGET_MB", 256)
	budget := int64(budgetMB) << 20

	c, err := Open(Options{Shards: 4, Dir: dir, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}

	// Ingest padded records until Put reports the disk is full, or the safety budget is reached
	// (so this never fills a real developer disk when CLASSAD_ENOSPC_DIR is unset).
	pad := strings.Repeat("x", 4000)
	var (
		committed int   // count of records Put acknowledged (returned nil)
		written   int64 // bytes offered
		putErr    error
	)
	for {
		ad := mustAdNoT2(fmt.Sprintf(`[Id=%d;pad=%q]`, committed, pad))
		if err := c.Put([]byte(fmt.Sprintf("k%d", committed)), ad); err != nil {
			putErr = err
			break
		}
		committed++
		written += int64(len(pad))
		if written >= budget {
			break
		}
	}

	// Every acknowledged record must read back with its own value -- no silent corruption, no loss.
	verify := func(store *Collection, upto int, label string) {
		for i := 0; i < upto; i++ {
			ad, ok := store.Get([]byte(fmt.Sprintf("k%d", i)))
			if !ok {
				t.Errorf("%s: acknowledged key k%d is missing (committed data lost)", label, i)
				continue
			}
			if id, ok := ad.EvaluateAttrInt("Id"); !ok || id != int64(i) {
				t.Errorf("%s: k%d wrong value Id=%d,ok=%v (silent corruption)", label, i, id, ok)
			}
		}
	}
	verify(c, committed, "pre-close")

	if putErr != nil {
		t.Logf("disk filled after %d acknowledged records: %v", committed, putErr)
		// Further writes must keep failing cleanly (sticky fail-stop), never panic.
		if err := c.Put([]byte("after-full"), mustAdNoT2(`[Id=-1]`)); err == nil {
			t.Log("note: a small Put succeeded after the disk-full (fit an already-allocated segment)")
		}
	} else {
		t.Logf("wrote %d records (%d MiB) without hitting ENOSPC; harness validated (set CLASSAD_ENOSPC_DIR to a small FS for the real test)", committed, written>>20)
	}

	// Close may itself fail to write its snapshot on a full disk; that is acceptable (reopen falls
	// back to a scan). Reopen must recover the acknowledged data consistently.
	if err := c.Close(); err != nil {
		t.Logf("Close on a full disk returned: %v (acceptable; reopen rebuilds from segments)", err)
	}
	c2, err := Open(Options{Shards: 4, Dir: dir, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("reopen after ENOSPC failed: %v", err)
	}
	defer c2.Close()
	verify(c2, committed, "reopen")
}

// mustAdNoT2 parses a known-valid ad literal without a *testing.T (safe here; the literals are
// fixed). Named to avoid colliding with mustAdNoT in the soak test if both land in one module.
func mustAdNoT2(src string) *classad.ClassAd {
	ad, err := classad.Parse(src)
	if err != nil {
		panic(err)
	}
	return ad
}
