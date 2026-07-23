package collections

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestScanRawRedacted verifies the redacting raw scan strips exactly the private
// attributes -- flagged once at intern time -- while ScanRaw still returns them,
// and that near-miss names (Cpus, TotalCpus, ClaimIdea, ...) survive redaction.
func TestScanRawRedacted(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	defer c.Close()

	ad, err := classad.Parse(`[
		MyType = "Machine"; TargetType = "Job";
		Name = "slot1@host";
		Cpus = 8; TotalCpus = 16;
		ClaimId = "<secret-claim>";
		CLAIMID = "casing is irrelevant, same interned id";
		Capability = "<secret-cap>";
		ChildClaimIds = { "a", "b" };
		TransferKey = "<secret-key>";
		ClaimIdea = "not private, just similar";
		Transferkeys = "not private either";
		_condor_privToken = "<secret-v2>";
		State = "Claimed"
	]`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put([]byte("slot1@host"), ad); err != nil {
		t.Fatal(err)
	}

	names := func(ras []map[string]struct{}) map[string]struct{} {
		if len(ras) != 1 {
			t.Fatalf("got %d ads, want 1", len(ras))
		}
		return ras[0]
	}
	collect := func(redacted bool) map[string]struct{} {
		var out []map[string]struct{}
		it := c.ScanRaw()
		if redacted {
			it = c.ScanRawRedacted()
		}
		for ra := range it {
			m := map[string]struct{}{}
			for _, e := range ra.Exprs {
				name, _, ok := strings.Cut(string(e), " = ")
				if !ok {
					t.Fatalf("malformed expr %q", e)
				}
				m[name] = struct{}{}
			}
			out = append(out, m)
		}
		return names(out)
	}

	private := []string{"ClaimId", "CLAIMID", "Capability", "ChildClaimIds", "TransferKey", "_condor_privToken"}
	public := []string{"Name", "Cpus", "TotalCpus", "ClaimIdea", "Transferkeys", "State"}

	full := collect(false)
	for _, n := range append(append([]string{}, private...), public...) {
		// CLAIMID interns to ClaimId's id; the unredacted scan surfaces the stored
		// attribute under one canonical casing, so only require the fold to exist.
		if _, ok := full[n]; !ok {
			found := false
			for got := range full {
				if strings.EqualFold(got, n) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("unredacted scan missing %s", n)
			}
		}
	}

	red := collect(true)
	for _, n := range private {
		for got := range red {
			if strings.EqualFold(got, n) {
				t.Errorf("redacted scan leaked private attribute %s (as %s)", n, got)
			}
		}
	}
	for _, n := range public {
		found := false
		for got := range red {
			if strings.EqualFold(got, n) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("redacted scan dropped public attribute %s", n)
		}
	}
}
