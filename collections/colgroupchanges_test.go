package collections

import "testing"

// TestDiffGroupSetsMagnitude is the core of the observability fix: a committed-group change must
// read as its true MAGNITUDE. A single attribute shifting is a Changed entry naming that one attr
// (not an unrelated group dropped and another added), while a disjoint replacement is add+remove.
func TestDiffGroupSetsMagnitude(t *testing.T) {
	// One attribute added to a group -> exactly one Changed, naming the added attr.
	ev, changed := diffGroupSets([][]string{{"A", "B", "C"}}, [][]string{{"A", "B", "C", "D"}})
	if !changed || len(ev.Changed) != 1 || len(ev.Added) != 0 || len(ev.Removed) != 0 {
		t.Fatalf("a one-attribute shift should be a single Changed entry, got %+v", ev)
	}
	if len(ev.Changed[0].Added) != 1 || ev.Changed[0].Added[0] != "D" || len(ev.Changed[0].Removed) != 0 {
		t.Errorf("expected only {D} added, got %+v", ev.Changed[0])
	}

	// One attribute removed -> a Changed entry naming the removed attr.
	ev, _ = diffGroupSets([][]string{{"A", "B", "C"}}, [][]string{{"A", "B"}})
	if len(ev.Changed) != 1 || len(ev.Changed[0].Removed) != 1 || ev.Changed[0].Removed[0] != "C" {
		t.Errorf("a one-attribute drop should be a single Changed naming {C}, got %+v", ev)
	}

	// Disjoint replacement -> a real drop plus a real add, not a Changed.
	ev, _ = diffGroupSets([][]string{{"A", "B"}}, [][]string{{"X", "Y", "Z"}})
	if len(ev.Changed) != 0 || len(ev.Added) != 1 || len(ev.Removed) != 1 {
		t.Fatalf("a disjoint replacement should be add+remove, got %+v", ev)
	}

	// Identical sets -> no change at all.
	if _, ch := diffGroupSets([][]string{{"A", "B"}}, [][]string{{"A", "B"}}); ch {
		t.Error("identical group sets must report no change")
	}
}

// TestGroupSchemaChangeLogRecordsCommittedChurn is the integration regression: when the accelerator
// actually adopts a different committed set, that event is recorded to the change log with a diff and
// a reason, and GroupSchemaDrift's committed-change count agrees. This is the churn a reader can act
// on -- as opposed to the derivation-snapshot count, which is sampler wobble.
func TestGroupSchemaChangeLogRecordsCommittedChurn(t *testing.T) {
	c := autoPromoteFixture(t, 0)
	defer c.Close()

	c.GroupSchemas(4096, 0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	adopted := 0
	for pass := 0; pass < 8; pass++ {
		c.GroupSchemas(4096, 0)
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("refresh returned false while enabled")
		}
		if got := len(c.schemaScan.Load().groups); got > 0 {
			adopted = got
			break
		}
	}
	if adopted == 0 {
		t.Skip("no group schema was promoted in this fixture")
	}

	changes := c.GroupSchemaChanges()
	if len(changes) == 0 {
		t.Fatal("a committed-group set was adopted but no change-log entry was recorded")
	}
	last := changes[len(changes)-1]
	if last.Reason == "" {
		t.Error("change-log entry has an empty reason")
	}
	if len(last.Added) == 0 && len(last.Changed) == 0 {
		t.Errorf("promotion recorded neither Added nor Changed groups: %+v", last)
	}
	if d := c.GroupSchemaDrift(); d.CommittedChanges != len(changes) {
		t.Errorf("drift.CommittedChanges=%d, want %d (the change-log length)", d.CommittedChanges, len(changes))
	}
	t.Logf("recorded %d committed change(s); last reason: %q", len(changes), last.Reason)
}
