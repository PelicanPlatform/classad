package dbrpc

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestProposeHookRoutesWrites verifies that with a propose hook set, a committing
// transaction's writes are handed to the hook (not committed to the local store), the
// batch carries the table and ops in order, and read-your-writes still works mid-txn.
func TestProposeHookRoutesWrites(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()

	var gotTable string
	var gotOps []WriteOp
	applied := 0
	s.SetProposeHook(func(table string, ops []WriteOp) error {
		gotTable = table
		gotOps = ops
		// Simulate the FSM: apply the batch to the store (as raft would on every node).
		tx := d.Begin()
		for _, op := range ops {
			switch op.Kind {
			case WriteNewClassAd:
				ad, _ := classad.ParseOld(op.Value)
				tx.NewClassAd(op.Key, ad)
			case WriteSetAttribute:
				_ = tx.SetAttribute(op.Key, op.Name, op.Value)
			case WriteDeleteAttribute:
				tx.DeleteAttribute(op.Key, op.Name)
			case WriteDestroyClassAd:
				tx.DestroyClassAd(op.Key)
			}
		}
		applied++
		return tx.Commit()
	})

	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer c.Close()

	tx, err := c.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd("1.0", "Owner = \"alice\""); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetAttribute("1.0", "JobStatus", "2"); err != nil {
		t.Fatal(err)
	}
	// Read-your-writes mid-transaction (served from the local txn buffer).
	if v, ok, err := tx.LookupAttr("1.0", "JobStatus"); err != nil || !ok || v != "2" {
		t.Fatalf("read-your-writes JobStatus = %q,%v,%v want 2", v, ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if applied != 1 {
		t.Fatalf("propose hook called %d times, want 1", applied)
	}
	if gotTable != DefaultTable {
		t.Errorf("proposed table = %q, want %q", gotTable, DefaultTable)
	}
	if len(gotOps) != 2 || gotOps[0].Kind != WriteNewClassAd || gotOps[1].Kind != WriteSetAttribute {
		t.Fatalf("proposed ops = %+v", gotOps)
	}
	if gotOps[1].Key != "1.0" || gotOps[1].Name != "JobStatus" || gotOps[1].Value != "2" {
		t.Errorf("SetAttribute op = %+v", gotOps[1])
	}
	// The store reflects the write, but ONLY because the hook applied it (not a local commit).
	if _, ok := d.LookupClassAd("1.0"); !ok {
		t.Error("store missing the proposed ad")
	}
}
