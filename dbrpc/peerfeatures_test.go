package dbrpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// untoldClient is the harness mistake this is about: a second client on the SAME server,
// built straight on the connection with no WithPeerSessionAttrs. The server implements every
// join opcode and says so; this client was simply never told.
func untoldClient(t *testing.T, f *joinFixture) *Client {
	t.Helper()
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn)
	t.Cleanup(func() { c.Close() })
	return c
}

// TestUntoldClientSaysSoRatherThanBlamingTheServer: a client never given the peer's session
// attributes reports its own ignorance, not a verdict on the server.
//
// Before the two were told apart the caller was handed "the server does not implement a
// server-side join" about a server that does, with nothing to distinguish it from a genuinely
// old peer. Every join entry point is covered, because the one that is missed is the one a
// harness will hit.
func TestUntoldClientSaysSoRatherThanBlamingTheServer(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	// The server really does advertise the join: the fixture's own client can run one.
	var ran int
	if err := f.c.Join(context.Background(), equiReq(db.JoinInner), func(string) bool { ran++; return true }); err != nil {
		t.Fatalf("the told client could not join, so there is nothing to be wrong about: %v", err)
	}
	if ran == 0 {
		t.Fatal("the told client's join produced no rows")
	}

	c := untoldClient(t, f)
	for _, tc := range []struct {
		name     string
		call     func() error
		sentinel error
	}{
		{"Join", func() error {
			return c.Join(context.Background(), equiReq(db.JoinInner), func(string) bool { return true })
		}, ErrJoinUnsupported},
		{"JoinWire", func() error {
			return c.JoinWire(context.Background(), equiReq(db.JoinInner), false, func([]byte) bool { return true })
		}, ErrJoinWireUnsupported},
		{"ExplainJoin", func() error {
			_, err := c.ExplainJoin(context.Background(), equiReq(db.JoinInner))
			return err
		}, ErrJoinUnsupported},
		{"JoinAggregate", func() error {
			_, err := c.JoinAggregate(context.Background(), JoinAggRequest{Spec: exitAggSpec(db.JoinInner)})
			return err
		}, ErrJoinAggUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("an untold client sent the request anyway")
			}
			// The compatibility guarantee: a caller that already classifies the request's
			// own sentinel, and falls back to doing the work itself, keeps working.
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, %v) = false: an existing caller's fallback would "+
					"stop firing; err = %v", tc.sentinel, err)
			}
			if !errors.Is(err, ErrPeerFeaturesUnknown) {
				t.Errorf("the error does not say the client was never told: %v", err)
			}
			if !strings.Contains(err.Error(), "WithPeerSessionAttrs") {
				t.Errorf("the error does not name the fix: %v", err)
			}
		})
	}
}

// TestToldClientBlamesTheServerPlainly: when this client WAS told what the peer advertised,
// a missing feature is the peer's own silence and the error stays exactly what it was --
// ErrPeerFeaturesUnknown must not start appearing on a genuinely old server.
func TestToldClientBlamesTheServerPlainly(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{}) }()
	// Told, and what it was told advertises nothing: a peer that really lacks the opcode.
	c := NewClient(cconn, WithPeerSessionAttrs(map[string]string{
		SessionAttrProtocol: "1",
		SessionAttrFeatures: "",
	}))
	defer c.Close()

	err := c.Join(context.Background(), equiReq(db.JoinInner), func(string) bool { return true })
	if !errors.Is(err, ErrJoinUnsupported) {
		t.Fatalf("a told client against a featureless peer: want ErrJoinUnsupported, got %v", err)
	}
	if errors.Is(err, ErrPeerFeaturesUnknown) {
		t.Errorf("the peer's own silence was reported as this client's ignorance: %v", err)
	}
	if err.Error() != ErrJoinUnsupported.Error() {
		t.Errorf("the plain refusal gained prose: %q", err.Error())
	}
}

// TestToldByAPeerThatListedNoFeatures is the case that actually turns on peerAttrsGiven.
//
// The peer sent session attributes but no feature LIST -- an older protocol that advertises
// its version and nothing else. peerFeatures stays nil, so Supports reports SupportUnknown
// exactly as it does for a client that was never told anything; the two are told apart only
// by whether this client was handed the peer's attrs at all. It was, so the refusal is about
// the peer and must not claim this client is ignorant.
func TestToldByAPeerThatListedNoFeatures(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(map[string]string{SessionAttrProtocol: "1"}))
	defer c.Close()

	if c.Supports(FeatJoin) != SupportUnknown {
		t.Fatalf("Supports(FeatJoin) = %v, want SupportUnknown: this test only means something "+
			"when the feature map is nil, which is what it shares with an untold client", c.Supports(FeatJoin))
	}
	err := c.Join(context.Background(), equiReq(db.JoinInner), func(string) bool { return true })
	if !errors.Is(err, ErrJoinUnsupported) {
		t.Fatalf("want ErrJoinUnsupported, got %v", err)
	}
	if errors.Is(err, ErrPeerFeaturesUnknown) {
		t.Errorf("a peer that advertised no feature list was reported as this client's own "+
			"ignorance: %v", err)
	}
}
