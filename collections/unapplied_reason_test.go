package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A refused write reported without a reason is a bare fact with no action attached: an operator
// seeing "these keys were dropped" in a log had to go to the daemon ad's counters and correlate
// by hand to learn whether it was a missing base, a broken chain or bad bytes -- which are
// different faults with different responses.
func TestUnappliedCarriesItsReason(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	const key = "42.0"
	full := classad.New()
	full.InsertAttr("ClusterId", int64(42))
	for i := range 20 {
		full.InsertAttr(fmt.Sprintf("Pad%02d", i), int64(i))
	}
	tx := c.Begin()
	tx.Put([]byte(key), full)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}

	readStoredMissHook = func(k []byte) bool { return string(k) == key }
	defer func() { readStoredMissHook = nil }()

	res := patchTx(t, c, key, "CompletionDate", 1789838852, "Pad00")
	if !res.HasUnapplied() {
		t.Fatal("the write was not refused")
	}
	if len(res.UnappliedReasons) != len(res.Unapplied) {
		t.Fatalf("%d reasons for %d unapplied keys: they must stay parallel or a log line names the wrong cause",
			len(res.UnappliedReasons), len(res.Unapplied))
	}
	if res.UnappliedReasons[0] == "" {
		t.Error("the reason is empty: the refusal is still silent about why")
	}
	t.Logf("key %q refused: %s", res.Unapplied[0], res.UnappliedReasons[0])
}
