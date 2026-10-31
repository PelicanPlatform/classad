package collections

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The persisted schema records its fields in LAYOUT order. Re-binding the names to a different
// process's intern ids must not re-order them: the order IS the record layout.
func TestPersistedSchemaKeepsItsStoredOrder(t *testing.T) {
	// Writer's ids: W=10, X=11, Y=12, Z=13. All int/4, so layout order is [W,X,Y,Z].
	wname := map[uint32]string{10: "W", 11: "X", 12: "Y", 13: "Z"}
	s := layoutSchema([]adField{
		{id: 10, kind: akInt, width: 4},
		{id: 11, kind: akInt, width: 4},
		{id: 12, kind: akInt, width: 4},
		{id: 13, kind: akInt, width: 4},
	})
	var order []string
	for _, f := range s.fields {
		order = append(order, wname[f.id])
	}
	t.Logf("writer order: %v offs %d %d %d %d", order, s.fields[0].off, s.fields[1].off, s.fields[2].off, s.fields[3].off)

	blob := marshalAdSchema(nil, s, func(id uint32) (string, bool) { n, ok := wname[id]; return n, ok })

	// Reader's ids: the SAME names, numbered in a different first-seen order (Z was seen first).
	rids := map[string]uint32{"Z": 1, "W": 2, "X": 3, "Y": 4}
	rname := map[uint32]string{1: "Z", 2: "W", 3: "X", 4: "Y"}
	got := readAdSchema(&cursor{b: blob}, func(n string) uint32 { return rids[n] })
	if got == nil {
		t.Fatal("readAdSchema failed")
	}
	var rorder []string
	for _, f := range got.fields {
		rorder = append(rorder, rname[f.id])
	}
	t.Logf("reader order: %v", rorder)
	for i := range order {
		if order[i] != rorder[i] {
			t.Fatalf("field %d: stored %s, read back as %s -- the record layout moved under the reader",
				i, order[i], rorder[i])
		}
	}
}

// The committed base schema is the layout new segments are written under and the one every
// segment's payload is compared against, so it has to come back from baseschema.json in the order
// it was written in -- not re-sorted by whatever ids this process handed out.
func TestCommittedBaseSchemaKeepsItsStoredOrder(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Intern in an order that does NOT match the file's: Z first, so a re-sort would put it first.
	for _, n := range []string{"Z", "W", "X", "Y"} {
		c.intern.Intern(n)
	}
	want := []string{"W", "X", "Y", "Z"}
	rec := persistedBaseSchema{Version: baseSchemaVersion}
	for _, n := range want {
		rec.Fields = append(rec.Fields, persistedSchemaField{Name: n, Kind: "int", Width: 4})
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, baseSchemaFile), data); err != nil {
		t.Fatal(err)
	}
	s, _, ok := c.loadCommittedBaseSchema()
	if !ok {
		t.Fatal("loadCommittedBaseSchema refused the record")
	}
	for i, f := range s.fields {
		n, _ := c.schemaFieldName(f.id)
		if n != want[i] {
			t.Fatalf("field %d: committed as %s, loaded as %s -- the committed layout moved", i, want[i], n)
		}
	}
}
