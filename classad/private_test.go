package classad

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsPrivateAttribute(t *testing.T) {
	private := []string{
		"ClaimId", "claimid", "CLAIMID", // V1, case-insensitive
		"Capability", "ClaimIds", "ClaimIdList", "ChildClaimIds", "TransferKey",
		"_condor_priv_foo", "_CONDOR_PRIV_bar", // V2 prefix, case-insensitive
	}
	for _, n := range private {
		if !IsPrivateAttribute(n) {
			t.Errorf("IsPrivateAttribute(%q) = false, want true", n)
		}
	}
	public := []string{
		"Owner", "JobStatus", "ClaimIdentifier", "Capabilities", // not exact matches
		"_condor_public", "MyClaimId", "", "PublicClaimId",
	}
	for _, n := range public {
		if IsPrivateAttribute(n) {
			t.Errorf("IsPrivateAttribute(%q) = true, want false", n)
		}
	}
}

// TestSerializersRedactByDefault is the core regression guard: none of the
// default serializers may emit a private attribute's name or its secret value.
// If a future change reintroduces a leak here, this fails.
func TestSerializersRedactByDefault(t *testing.T) {
	ad, err := Parse(`[ Owner = "alice"; JobStatus = 2; ClaimId = "SECRET-CAP-123"; Capability = "SECRET-CAP-456"; _condor_priv_x = "SECRET-789" ]`)
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{
		"String":     ad.String(),
		"MarshalOld": ad.MarshalOld(),
	}
	jb, err := json.Marshal(ad) // exercises MarshalJSON via the json.Marshaler path
	if err != nil {
		t.Fatal(err)
	}
	outputs["MarshalJSON"] = string(jb)

	secrets := []string{"ClaimId", "Capability", "_condor_priv_x", "SECRET-CAP-123", "SECRET-CAP-456", "SECRET-789"}
	for name, out := range outputs {
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s leaked %q: %s", name, s, out)
			}
		}
		// Public attributes must survive.
		if !strings.Contains(out, "Owner") || !strings.Contains(out, "alice") {
			t.Errorf("%s dropped public attribute: %s", name, out)
		}
	}
}

// TestWithPrivateOptIn verifies the explicit opt-in serializers DO include
// secrets (for internal/authorized use).
func TestWithPrivateOptIn(t *testing.T) {
	ad, err := Parse(`[ Owner = "alice"; ClaimId = "SECRET-123" ]`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ad.StringWithPrivate(), "SECRET-123") {
		t.Errorf("StringWithPrivate dropped the secret: %s", ad.StringWithPrivate())
	}
	if !strings.Contains(ad.MarshalOldWithPrivate(), "SECRET-123") {
		t.Errorf("MarshalOldWithPrivate dropped the secret: %s", ad.MarshalOldWithPrivate())
	}
	jb, err := ad.MarshalJSONWithPrivate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jb), "SECRET-123") {
		t.Errorf("MarshalJSONWithPrivate dropped the secret: %s", jb)
	}
}

// TestRedacted verifies Redacted strips secrets while leaving the original intact.
func TestRedacted(t *testing.T) {
	ad, err := Parse(`[ Owner = "alice"; ClaimId = "SECRET-123" ]`)
	if err != nil {
		t.Fatal(err)
	}
	red := ad.Redacted()
	if _, ok := red.Lookup("ClaimId"); ok {
		t.Error("Redacted retained ClaimId")
	}
	if _, ok := red.Lookup("Owner"); !ok {
		t.Error("Redacted dropped a public attribute")
	}
	// Original is untouched (secret still present for internal/authorized use).
	if _, ok := ad.Lookup("ClaimId"); !ok {
		t.Error("Redacted mutated the original ad")
	}
}
