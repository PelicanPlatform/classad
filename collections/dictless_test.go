package collections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQuarantineDictlessMakesTheStoreOpenable is the whole point: Open refuses a store
// whose segments name a dictionary it lacks, and this is the supported way out. The pass
// moves those segments aside, drops the snapshots describing them, and the store opens
// again -- without the records that needed the dictionary, which is the cost of the
// dictionary being gone, not of this pass.
func TestQuarantineDictlessMakesTheStoreOpenable(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	if len(named) == 0 {
		t.Fatal("fixture wrote no segment under dictionary 1")
	}
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}); err == nil {
		t.Fatal("control: Open succeeded over a missing dictionary")
	}

	rep, err := QuarantineDictless(dir, DictlessOptions{})
	if err != nil {
		t.Fatalf("QuarantineDictless: %v", err)
	}
	t.Log("\n" + rep.String())
	if len(rep.Moved) != len(named) {
		t.Errorf("moved %d segments; want %d", len(rep.Moved), len(named))
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != 1 {
		t.Errorf("Missing = %v; want [1]", rep.Missing)
	}
	if len(rep.DroppedSnapshots) == 0 {
		t.Error("the shard's directory snapshot was not dropped")
	}

	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after quarantine: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records survived the quarantine")
	}
}

// TestQuarantineDictlessKeepsTheBytes: the segments are set aside, not destroyed. If the
// dictionary is found later -- a backup, a replica, the filesystem -- the records are
// still there to be recovered.
func TestQuarantineDictlessKeepsTheBytes(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	sizes := map[string]int64{}
	for _, name := range named {
		st, err := os.Stat(filepath.Join(shardDir, name))
		if err != nil {
			t.Fatal(err)
		}
		sizes[name] = st.Size()
	}
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	rep, err := QuarantineDictless(dir, DictlessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range named {
		q := filepath.Join(dir, quarantineDir, "0", name)
		st, statErr := os.Stat(q)
		if statErr != nil {
			t.Fatalf("quarantined segment missing: %v", statErr)
		}
		if st.Size() != sizes[name] {
			t.Errorf("%s: quarantined size %d != original %d", name, st.Size(), sizes[name])
		}
	}
	// Quarantine is not a shard directory, so nothing walks it.
	if _, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}); err != nil {
		t.Fatalf("quarantine/ was not invisible to Open: %v", err)
	}
	if len(rep.Moved) == 0 {
		t.Error("nothing reported moved")
	}
}

// TestQuarantineDictlessRefusesWhenTheAtticHasIt is the rule that matters most: pruning
// retires dictionaries to dicts/attic/ exactly so this is recoverable, and quarantining
// instead would set aside records a one-file restore would have kept.
func TestQuarantineDictlessRefusesWhenTheAtticHasIt(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	attic := filepath.Join(dir, "dicts", "attic")
	if err := os.MkdirAll(attic, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "dicts", "1.zst"),
		filepath.Join(attic, "1.zst")); err != nil {
		t.Fatal(err)
	}

	rep, err := QuarantineDictless(dir, DictlessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	if len(rep.Moved) != 0 {
		t.Errorf("quarantined %d segments although the dictionary is in the attic", len(rep.Moved))
	}
	if len(rep.Recoverable) != 1 || rep.Recoverable[0] != 1 {
		t.Errorf("Recoverable = %v; want [1]", rep.Recoverable)
	}
	if out := rep.String(); !strings.Contains(out, "REFUSED") || !strings.Contains(out, "attic") {
		t.Errorf("report does not point at the attic:\n%s", out)
	}
	for _, name := range named {
		if _, statErr := os.Stat(filepath.Join(shardDir, name)); statErr != nil {
			t.Errorf("segment %s was moved despite the refusal", name)
		}
	}

	// Restoring the file is the recovery the refusal is protecting, and it works.
	if rerr := os.Rename(filepath.Join(attic, "1.zst"),
		filepath.Join(dir, "dicts", "1.zst")); rerr != nil {
		t.Fatal(rerr)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after restoring from the attic: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back after restoring from the attic")
	}
}

// TestQuarantineDictlessForceOverridesTheAttic: the refusal is a default, not a wall.
func TestQuarantineDictlessForceOverridesTheAttic(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	attic := filepath.Join(dir, "dicts", "attic")
	if err := os.MkdirAll(attic, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "dicts", "1.zst"),
		filepath.Join(attic, "1.zst")); err != nil {
		t.Fatal(err)
	}

	rep, err := QuarantineDictless(dir, DictlessOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Moved) != len(named) {
		t.Errorf("Force moved %d segments; want %d", len(rep.Moved), len(named))
	}
}

// TestQuarantineDictlessDryRunWritesNothing: an operator facing a store that will not open
// needs to see the cost before paying it.
func TestQuarantineDictlessDryRunWritesNothing(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	rep, err := QuarantineDictless(dir, DictlessOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	if len(rep.Moved) != len(named) {
		t.Errorf("dry run reported %d segments; want %d", len(rep.Moved), len(named))
	}
	for _, s := range rep.Moved {
		if s.Quarantined != "" {
			t.Errorf("dry run reported a destination: %s", s.Quarantined)
		}
	}
	for _, name := range named {
		if _, statErr := os.Stat(filepath.Join(shardDir, name)); statErr != nil {
			t.Errorf("dry run moved %s", name)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, quarantineDir)); statErr == nil {
		t.Error("dry run created the quarantine directory")
	}
	if _, statErr := os.Stat(filepath.Join(shardDir, dirSnapName)); statErr != nil {
		t.Error("dry run dropped the directory snapshot")
	}
	// And the store is still refused, because nothing changed.
	if _, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14}); !errors.Is(err, ErrMissingDictionaries) {
		t.Errorf("after a dry run Open gave %v; want ErrMissingDictionaries", err)
	}
}

// TestQuarantineDictlessHealthyStoreIsANoop is the control: a store with every dictionary
// present must not have anything moved out from under it.
func TestQuarantineDictlessHealthyStoreIsANoop(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	before, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatal(err)
	}

	rep, qerr := QuarantineDictless(dir, DictlessOptions{})
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rep.Moved) != 0 || len(rep.Missing) != 0 || len(rep.DroppedSnapshots) != 0 {
		t.Errorf("healthy store: moved=%d missing=%v dropped=%v",
			len(rep.Moved), rep.Missing, rep.DroppedSnapshots)
	}
	after, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("shard directory changed: %d -> %d entries", len(before), len(after))
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("healthy store no longer opens: %v", err)
	}
	c.Close()
}

// TestQuarantineDictlessMovesSidecars: a sidecar describes its segment's record offsets
// and is worthless without it, but it moves rather than being deleted -- it is part of
// what a later recovery would want.
func TestQuarantineDictlessMovesSidecars(t *testing.T) {
	dir, shardDir := buildTwoDictStore(t)
	named := segmentsNaming(t, shardDir, 1)
	withSidecar := ""
	for _, name := range named {
		if _, err := os.Stat(filepath.Join(shardDir, name+".idx")); err == nil {
			withSidecar = name
			break
		}
	}
	if withSidecar == "" {
		t.Skip("no sealed segment under dictionary 1 carried a sidecar")
	}
	if err := os.Remove(filepath.Join(dir, "dicts", "1.zst")); err != nil {
		t.Fatal(err)
	}

	if _, err := QuarantineDictless(dir, DictlessOptions{}); err != nil {
		t.Fatal(err)
	}
	q := filepath.Join(dir, quarantineDir, "0", withSidecar+".idx")
	if _, err := os.Stat(q); err != nil {
		t.Errorf("sidecar was not moved with its segment: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shardDir, withSidecar+".idx")); err == nil {
		t.Error("sidecar left behind in the shard directory")
	}
}

// TestQuarantineDictlessMissingStore reports a usable error rather than succeeding
// vacuously on a path that is not a store.
func TestQuarantineDictlessMissingStore(t *testing.T) {
	_, err := QuarantineDictless(filepath.Join(t.TempDir(), "nope"), DictlessOptions{})
	if err == nil {
		t.Fatal("no error for a directory that does not exist")
	}
	if !strings.Contains(err.Error(), "quarantine") {
		t.Errorf("error does not name the operation: %v", err)
	}
	_ = fmt.Sprint(err)
}
