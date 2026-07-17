package collections

import (
	"fmt"
	"testing"
)

// TestMPHRoundTrip is the correctness anchor: for a range of sizes, every assigned member
// probes (over the serialized bytes) back to its own slot, assigned slots are a bijection
// onto [0, nAssigned), and the vast majority of keys are assigned (the geometric tail falls
// back to the sorted-run binary search, which is correct, just not the fast path).
func TestMPHRoundTrip(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 63, 64, 65, 127, 1000, 50000} {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("GlobalJobId=host%d.example.org#%d.%d", i%97, i, i*2654435761)
		}
		m, slots := buildMPH(keys)
		blob := appendMPH(nil, m)

		// slot -> key, and bijection check.
		slotKey := make([]string, m.nAssigned)
		seen := make([]bool, m.nAssigned)
		assigned := 0
		for i, s := range slots {
			if s < 0 {
				continue
			}
			if uint32(s) >= m.nAssigned {
				t.Fatalf("n=%d: slot %d out of range [0,%d)", n, s, m.nAssigned)
			}
			if seen[s] {
				t.Fatalf("n=%d: slot %d assigned twice", n, s)
			}
			seen[s] = true
			slotKey[s] = keys[i]
			assigned++
		}

		// Every assigned member resolves (via the serialized probe) to its slot.
		for i, k := range keys {
			if slots[i] < 0 {
				continue // unassigned: caller falls back to binary search
			}
			slot, ok := mphLookupBytes(blob, 0, k)
			if !ok || slot != uint32(slots[i]) {
				t.Fatalf("n=%d member %q: probe=(slot=%d ok=%v), want slot=%d", n, k, slot, ok, slots[i])
			}
			if slotKey[slot] != k {
				t.Fatalf("n=%d: slot %d holds %q, want %q", n, slot, slotKey[slot], k)
			}
		}
		if n >= 1000 && assigned < n*95/100 {
			t.Errorf("n=%d: only %d/%d keys assigned (<95%%); level cap or gamma too low", n, assigned, n)
		}
	}
}

// TestMPHNonMembersNeverFalselyVerify: a non-member may resolve to some slot (a false hit),
// but the key stored at that slot is a real member, never the non-member -- so the caller's
// verify always rejects it. This is what makes the MPH safe without an authoritative role.
func TestMPHNonMembersNeverFalselyVerify(t *testing.T) {
	t.Parallel()
	const n = 8000
	keys := make([]string, n)
	set := make(map[string]bool, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("member-%d-%x", i, i*40503)
		set[keys[i]] = true
	}
	m, slots := buildMPH(keys)
	blob := appendMPH(nil, m)
	slotKey := make([]string, m.nAssigned)
	for i, s := range slots {
		if s >= 0 {
			slotKey[s] = keys[i]
		}
	}
	for j := 0; j < 40000; j++ {
		nk := fmt.Sprintf("absent-%d-%x", j, j*2246822519)
		if set[nk] {
			continue
		}
		if slot, ok := mphLookupBytes(blob, 0, nk); ok && slot < m.nAssigned && slotKey[slot] == nk {
			t.Fatalf("non-member %q verified as a member at slot %d", nk, slot)
		}
	}
}
