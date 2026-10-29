package dbrpc

import (
	"strconv"
	"strings"
	"testing"
)

// peerFrom builds a client holding what a peer advertised, without a
// connection: the option is pure and the mux is irrelevant to it.
func peerFrom(attrs map[string]string) *Client {
	c := &Client{}
	WithPeerSessionAttrs(attrs)(c)
	return c
}

// TestServerAdvertisementRoundTrips is the contract between the two helpers an
// application wires together: whatever a server publishes, a client that
// captures it must report as supported. It also catches the likeliest mistake
// in adding an opcode -- declaring the Feature and forgetting serverFeatures,
// which would leave clients probing for something the server has.
func TestServerAdvertisementRoundTrips(t *testing.T) {
	c := peerFrom(ServerSessionAttrs())

	if got := c.PeerProtocol(); got != ProtocolVersion {
		t.Errorf("PeerProtocol() = %d, want %d", got, ProtocolVersion)
	}
	for _, f := range serverFeatures {
		if got := c.Supports(f); got != SupportYes {
			t.Errorf("Supports(%s) = %s, want yes", f, got)
		}
	}
	if got := c.Supports(Feature("no-such-feature")); got != SupportNo {
		t.Errorf("Supports(unknown feature) = %s, want no -- the peer listed its features, so anything absent is a definite no", got)
	}
}

// TestSilentPeerIsUnknownNotNo is the compatibility rule in one assertion.
//
// Every feature named here shipped before advertisement did, so a server that
// says nothing may implement all of them. Reading silence as "no" would refuse
// work that an older server does perfectly well -- the new-client/old-server
// case this mechanism exists to keep working.
func TestSilentPeerIsUnknownNotNo(t *testing.T) {
	for _, name := range []string{"no attributes", "empty map"} {
		var attrs map[string]string
		if name == "empty map" {
			attrs = map[string]string{}
		}
		c := peerFrom(attrs)
		if got := c.PeerProtocol(); got != 0 {
			t.Errorf("%s: PeerProtocol() = %d, want 0", name, got)
		}
		for _, f := range serverFeatures {
			if got := c.Supports(f); got != SupportUnknown {
				t.Errorf("%s: Supports(%s) = %s, want unknown", name, f, got)
			}
		}
	}
}

// TestVersionWithoutFeaturesLeavesFeaturesUnknown covers a peer that states a
// version but no feature list -- a future build that drops the list, or an
// attribute lost in transit. The version is usable; the features are not, and
// claiming SupportNo for all of them would be a fabricated answer.
func TestVersionWithoutFeaturesLeavesFeaturesUnknown(t *testing.T) {
	c := peerFrom(map[string]string{SessionAttrProtocol: "7"})
	if got := c.PeerProtocol(); got != 7 {
		t.Errorf("PeerProtocol() = %d, want 7", got)
	}
	if got := c.Supports(FeatTopK); got != SupportUnknown {
		t.Errorf("Supports(topk) = %s, want unknown", got)
	}
}

// TestUnreadableVersionIsNoClaim: a version that is not a non-negative integer
// is a peer we cannot interpret, and the safe reading of an uninterpretable
// claim is no claim. The feature list is still honoured, since it parses
// independently.
func TestUnreadableVersionIsNoClaim(t *testing.T) {
	for _, bad := range []string{"", "   ", "one", "-3", "3.1", "9999999999999999999999"} {
		c := peerFrom(map[string]string{
			SessionAttrProtocol: bad,
			SessionAttrFeatures: string(FeatTopK),
		})
		if got := c.PeerProtocol(); got != 0 {
			t.Errorf("PeerProtocol() for %q = %d, want 0", bad, got)
		}
		if got := c.Supports(FeatTopK); got != SupportYes {
			t.Errorf("features for %q = %s, want yes -- the list parses on its own", bad, got)
		}
	}
}

// TestFeatureListParsing pins the wire form: a comma-separated list, tolerant
// of spacing and of empty entries, since it is assembled and read by peers of
// different vintages.
func TestFeatureListParsing(t *testing.T) {
	c := peerFrom(map[string]string{
		SessionAttrProtocol: "1",
		SessionAttrFeatures: " topk ,, projrefs,",
	})
	for _, f := range []Feature{FeatTopK, FeatProjectRefs} {
		if got := c.Supports(f); got != SupportYes {
			t.Errorf("Supports(%s) = %s, want yes", f, got)
		}
	}
	if got := c.Supports(FeatRawWire); got != SupportNo {
		t.Errorf("Supports(rawwire) = %s, want no", got)
	}
}

// TestServerSessionAttrsShape keeps the advertisement itself honest: the
// version parses as the constant, and the feature list is sorted so two
// captures of one server compare equal.
func TestServerSessionAttrsShape(t *testing.T) {
	attrs := ServerSessionAttrs()
	if got, err := strconv.Atoi(attrs[SessionAttrProtocol]); err != nil || got != ProtocolVersion {
		t.Errorf("%s = %q, want %d", SessionAttrProtocol, attrs[SessionAttrProtocol], ProtocolVersion)
	}
	names := strings.Split(attrs[SessionAttrFeatures], ",")
	if len(names) != len(serverFeatures) {
		t.Fatalf("advertised %d features, this build has %d", len(names), len(serverFeatures))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("feature list is not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// TestClientSessionAttrNamesCoversWhatTheServerSends: the names a client asks
// cedar to capture must be the names the server sets, or the handshake carries
// the advertisement and the client throws it away.
func TestClientSessionAttrNamesCoversWhatTheServerSends(t *testing.T) {
	want := ClientSessionAttrNames()
	for name := range ServerSessionAttrs() {
		found := false
		for _, w := range want {
			if w == name {
				found = true
			}
		}
		if !found {
			t.Errorf("the server advertises %s but no client asks for it", name)
		}
	}
}
