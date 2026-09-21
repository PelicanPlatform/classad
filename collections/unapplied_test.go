package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A patch whose stored base cannot be read is refused rather than written against an empty ad.
// WHICH bucket it lands in decides whether a caller loops forever: a conflict invites a retry, and
// re-applying the identical write cannot make an unreadable record readable. On a production
// mirror that distinction was the difference between a stale row and a tailer that rewound, failed
// identically, escalated to a 460-second full replay that wrote nothing, and did it 285 times.
func TestUnreadableBaseIsUnappliedNotConflict(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()

	full := classad.New()
	full.InsertAttr("ClusterId", int64(42))
	full.InsertAttr("JobStatus", int64(2))
	for i := 0; i < 20; i++ {
		full.InsertAttr(fmt.Sprintf("Pad%02d", i), int64(i))
	}
	tx := c.Begin()
	tx.Put([]byte("42.0"), full)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}

	readStoredMissHook = func(key []byte) bool { return string(key) == "42.0" }
	defer func() { readStoredMissHook = nil }()

	patch := classad.New()
	patch.InsertAttr("CompletionDate", int64(1789838852))
	w := c.Begin()
	w.PatchAttrs([]byte("42.0"), patch, []string{"Pad00"}) // a removal forces the compose path
	res := w.Commit()

	if res.Conflicted() {
		t.Errorf("reported as a CONFLICT (%d keys): a caller will retry this forever", len(res.Conflicts))
	}
	if !res.HasUnapplied() {
		t.Fatal("not reported as unapplied either: the write vanished silently")
	}
	if got := string(res.Unapplied[0]); got != "42.0" {
		t.Errorf("Unapplied[0] = %q, want 42.0", got)
	}
	if res.Committed != 0 {
		t.Errorf("Committed = %d, want 0", res.Committed)
	}

	// And the stored ad is still whole -- the refusal's original purpose.
	readStoredMissHook = nil
	ad, ok := c.Get([]byte("42.0"))
	if !ok {
		t.Fatal("42.0 vanished")
	}
	if n := len(ad.AST().Attributes); n != 22 {
		t.Errorf("42.0 has %d attributes, want the original 22", n)
	}
}
