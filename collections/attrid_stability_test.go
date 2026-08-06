package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestInlineAttrIDsSurviveDropAndReopen is the regression for a silent wrong-answer bug.
//
// Inline attribute ids used to be positional -- 0,1,2 in the order of the persisted name
// list -- while spec.gen, which decides whether a sidecar looks current, is not persisted and
// resets to 0 on every open. Dropping an index therefore shifted every later attribute's id
// on the next start, and sidecars written before the drop (gen 0) matched the reset gen and
// were published under the NEW id mapping. One attribute's postings then answered for
// another. Candidates are re-verified against the real predicate, so the damage showed up not
// as wrong rows but as MISSING ones -- the quietest possible failure.
//
// The ids now follow the attribute name, so an old sidecar either means what it always meant
// or matches nothing current, in which case the segment is scanned.
func TestInlineAttrIDsSurviveDropAndReopen(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()

	// Owner is listed FIRST, so under positional ids it owned id 0 and Group owned id 1.
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 13,
		CategoricalAttrs: []string{"Owner", "Group"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 1500
	const wantGroup = "grp7"
	expect := 0
	for i := 0; i < n; i++ {
		grp := fmt.Sprintf("grp%d", i%10)
		if grp == wantGroup {
			expect++
		}
		ad := mustAdOld(t, fmt.Sprintf("Owner = %q\nGroup = %q\nPad = %q",
			fmt.Sprintf("user%d", i%4), grp, strings.Repeat("x", 60)))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex() // seal sidecars for the segments written so far
	if got := countGroup(t, c, wantGroup); got != expect {
		t.Fatalf("before drop: %d rows, want %d", got, expect)
	}

	// Drop the FIRST-listed index. Under positional ids this is what shifted Group onto
	// Owner's id when the collection was next opened.
	if !c.DropIndex("Owner") {
		t.Fatal("DropIndex(Owner) reported no change")
	}
	c.Close()

	c2, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 13,
		CategoricalAttrs: []string{"Group"}, // the persisted set after the drop
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	if got := countGroup(t, c2, wantGroup); got != expect {
		t.Errorf("after drop+reopen: %d rows, want %d -- an attribute's index answered for "+
			"a different attribute, so matching records were skipped", got, expect)
	}
}

func countGroup(t *testing.T, c *Collection, group string) int {
	t.Helper()
	q, err := vm.Parse(fmt.Sprintf("Group == %q", group))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}
