package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A key parked in collapseRetry is work waiting to be done, and nothing used to schedule it: the
// pass only ran when a shard had sealedPending set, so a failed collapse waited for the next
// SEAL rather than the next commit. On a table that went quiet it was never retried, and
// collapseRetry is RAM only -- so a restart lost it and the fragment it named stayed live in a
// sealed segment permanently.
func TestAParkedRetryKeySchedulesACollapse(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	ad := classad.New()
	ad.InsertAttr("ClusterId", int64(1))
	tx := c.Begin()
	tx.Put([]byte("1.0"), ad)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}

	// Park a key, with no shard flagged: exactly the state a failed collapse leaves.
	c.rememberCollapse([]byte("1.0"))
	for _, sh := range c.shards {
		sh.sealedPending.Store(false)
	}
	c.collapseMu.Lock()
	parked := len(c.collapseRetry)
	c.collapseMu.Unlock()
	if parked == 0 {
		t.Fatal("nothing parked: the state under test was not created")
	}

	// An ordinary commit that seals nothing must still pick it up.
	ad2 := classad.New()
	ad2.InsertAttr("ClusterId", int64(2))
	w := c.Begin()
	w.Put([]byte("2.0"), ad2)
	w.Commit()

	c.collapseMu.Lock()
	left := len(c.collapseRetry)
	c.collapseMu.Unlock()
	if left == parked {
		t.Error("the parked key was not picked up: a failed collapse still waits for a seal")
	}
}

// Collection.Put does not go through Txn.Commit, which was the only thing that scheduled the
// collapse drain. A Put that rolls a segment therefore flagged a seal and walked away, leaving
// every open chain in that segment waiting on some later transaction -- and stranded outright if
// the process exited first.
func TestPutSchedulesItsOwnCollapseDrain(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	// Enough Puts to roll several segments.
	for i := range 3000 {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		for j := range 20 {
			ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}

	// No shard may be left holding an undrained seal: that is the bookkeeping whose only consumer
	// is a collapse pass, and nothing else will run one.
	for i, sh := range c.shards {
		if sh.sealedPending.Load() {
			t.Errorf("shard %d still flags an undrained seal after Put-driven rollovers", i)
		}
		sh.mu.RLock()
		n := len(sh.pendingSeal)
		sh.mu.RUnlock()
		if n != 0 {
			t.Errorf("shard %d retains %d pending-seal segment(s) after Put-driven rollovers", i, n)
		}
	}
}
