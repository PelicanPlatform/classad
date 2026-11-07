package dbrpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// THE JOIN EXPLAIN OVER A REAL CONNECTION.
//
// What these pin is not the planning -- db tests that -- but that the plan survives the
// wire: that both spellings reach the handler that matches them, that the report comes back
// whole, and that the explainer is behind the same private-attribute gate the join is.

// The report crosses intact, for both spellings, and names the planner that chose.
func TestExplainJoinOverTheWire(t *testing.T) {
	c, cat := chainRPCFixture(t)
	ctx := context.Background()

	chain := chainRPCRequest(db.JoinInner, db.JoinInner)
	pair := JoinRequest{Spec: db.JoinSpec{
		Left:  db.JoinSide{Table: "history", Alias: "h"},
		Right: db.JoinSide{Table: "epoch_history", Alias: "e"},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}

	for _, tc := range []struct {
		name    string
		jr      JoinRequest
		planner string
		sides   int
	}{
		{"chain", chain, "chain", 3},
		{"two tables", pair, "build-side", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.ExplainJoin(ctx, tc.jr)
			if err != nil {
				t.Fatal(err)
			}
			if got.Planner != tc.planner {
				t.Errorf("planner %q, want %q", got.Planner, tc.planner)
			}
			if len(got.Sides) != tc.sides {
				t.Fatalf("report has %d sides, want %d", len(got.Sides), tc.sides)
			}
			if got.OrderWhy == "" {
				t.Error("the report gives no reason for the order it chose")
			}
			if !got.Sides[0].Streamed {
				t.Error("the first side is not marked streamed")
			}
			for _, s := range got.Sides {
				if s.Read == nil || s.Read.Plan == "" {
					t.Errorf("%s crossed the wire with no access path (%s)", s.Alias, s.ReadWhy)
				}
			}
			// The same question asked locally must give the same answer: a report that
			// changed shape on the way across is the failure this catches.
			want, err := cat.ExplainJoin(tc.jr.Spec, tc.jr.Limits)
			if err != nil {
				t.Fatal(err)
			}
			if got.Planner != want.Planner || got.OrderWhy != want.OrderWhy ||
				len(got.Sides) != len(want.Sides) || len(got.Steps) != len(want.Steps) {
				t.Errorf("the wire report differs from the local one:\n got  %+v\n want %+v",
					got, want)
			}
			for i := range want.Sides {
				if got.Sides[i].Alias != want.Sides[i].Alias {
					t.Errorf("side %d: wire says %s, local says %s",
						i, got.Sides[i].Alias, want.Sides[i].Alias)
				}
			}
		})
	}
}

// The limits the plan would run under cross too, lowered to what the caller asked for.
func TestExplainJoinReportsTheLimitsItWouldRunUnder(t *testing.T) {
	c, _ := chainRPCFixture(t)
	jr := chainRPCRequest(db.JoinInner, db.JoinInner)
	jr.Limits = db.JoinLimits{MaxHashRows: 7}
	got, err := c.ExplainJoin(context.Background(), jr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits.MaxHashRows != 7 {
		t.Errorf("reports %d max hash rows, want the 7 the caller asked for",
			got.Limits.MaxHashRows)
	}
	if got.Limits.MaxRows != db.DefaultJoinLimits().MaxRows {
		t.Errorf("reports %d max rows, want the store's own %d where the caller named none",
			got.Limits.MaxRows, db.DefaultJoinLimits().MaxRows)
	}
}

// The explainer names the plan's expressions back to the caller, so it must be behind the
// same private-attribute gate the join is. A caller barred from referencing one in a
// condition must not be able to get it echoed out of the report instead.
func TestExplainJoinRefusesAPrivateReference(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, name := range []string{"history", "epoch_history", "job_metrics"} {
		if _, cerr := cat.CreateTable(name); cerr != nil {
			t.Fatal(cerr)
		}
	}
	srv := NewServerCatalog(cat)
	defer srv.Close()
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: false}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(srv.SessionAttrs()))
	defer c.Close()

	const private = "ClaimId" // a private attribute by HTCondor's own rules
	if !classad.IsPrivateAttribute(private) {
		t.Fatalf("%s is not treated as private, so this test gates nothing", private)
	}
	// Both spellings, because they are caught by two different gates: a side's own
	// constraint is a BARE reference, and a condition over the joined row is an
	// alias-qualified one. An explainer wired to only one of them leaks through the other.
	for _, tc := range []struct {
		name string
		set  func(jr *JoinRequest)
	}{
		{"a side's own constraint, read bare", func(jr *JoinRequest) {
			jr.Spec.Steps[1].Side.Constraint = private + ` == "x"`
		}},
		{"an ON term, qualified by an alias", func(jr *JoinRequest) {
			jr.Spec.Steps[1].Cond = append(jr.Spec.Steps[1].Cond, `m.`+private+` == "x"`)
		}},
		{"an equality half", func(jr *JoinRequest) {
			jr.Spec.Steps[1].Equi = []db.JoinEqui{{Left: "e.ClusterId", Right: private}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jr := chainRPCRequest(db.JoinInner, db.JoinInner)
			tc.set(&jr)
			_, err := c.ExplainJoin(context.Background(), jr)
			if err == nil {
				t.Fatal("explained a join referencing a private attribute")
			}
			if !strings.Contains(err.Error(), private) {
				t.Errorf("refused with %q, want it to name %s", err, private)
			}
		})
	}
}

// A server that does not advertise the feature is not asked, and the caller gets the same
// sentinel Join uses rather than an opaque bad request.
func TestExplainJoinUnsupportedWithoutTheFeature(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if _, cerr := cat.CreateTable("history"); cerr != nil {
		t.Fatal(cerr)
	}
	srv := NewServerCatalog(cat)
	defer srv.Close()
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()

	// No peer session attrs: this client has been told nothing, and the feature postdates
	// advertisement, so silence is a no. The request must not be sent at all.
	quiet := NewClient(cconn)
	defer quiet.Close()
	jr := JoinRequest{Spec: db.JoinSpec{
		Left:  db.JoinSide{Table: "history", Alias: "h"},
		Right: db.JoinSide{Table: "history", Alias: "h2"},
		Cond:  []string{"h.ClusterId == h2.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
	if _, err := quiet.ExplainJoin(context.Background(), jr); !errors.Is(err, ErrJoinUnsupported) {
		t.Errorf("a peer advertising nothing gave %v, want ErrJoinUnsupported", err)
	}

	// And a client that WAS told does get an answer, or the check above passes for the
	// wrong reason -- on a server that could not have answered either way.
	told := NewClient(mustSecondConn(t, srv), WithPeerSessionAttrs(srv.SessionAttrs()))
	defer told.Close()
	if !told.SupportsNewer(FeatJoinExplain) {
		t.Fatal("a catalog server does not advertise FeatJoinExplain")
	}
	if _, err := told.ExplainJoin(context.Background(), jr); err != nil {
		t.Errorf("the advertised feature did not answer: %v", err)
	}
}

// mustSecondConn connects another client to an already-serving server, with its features.
func mustSecondConn(t *testing.T, srv *Server) MsgConn {
	t.Helper()
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	t.Cleanup(func() { _ = cconn.Close() })
	return cconn
}
