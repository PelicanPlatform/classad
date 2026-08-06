package collections

import (
	"strings"
	"testing"
)

// TestInlineAttrIDCollisionRefused pins the collision behaviour: two names that hash to the
// same id must not share one index, because that index would answer for both. The second
// attribute is left unindexed (its queries scan) rather than aliased.
func TestInlineAttrIDCollisionRefused(t *testing.T) {
	s := newInlineIndexSpec(nil, nil)
	first := "Alpha"
	id, ok := s.inlineID(first)
	if !ok {
		t.Fatal("first attribute refused")
	}
	// Forge a collision by planting a different name at the same id.
	s.names[id] = "SomethingElse"
	if _, ok := s.inlineID("Beta"); ok {
		// Beta hashes elsewhere, so this is not the collision case; assert the planted
		// name is what triggers refusal by asking for a name whose id we know is taken.
		delete(s.nameToID, strings.ToLower(first))
		if _, ok := s.inlineID(first); ok {
			t.Error("an id already held by a different attribute was handed out again")
		}
	}
}
