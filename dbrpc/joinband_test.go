package dbrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// A band is part of what a join MATCHES ON, and the request body has nowhere to put one. So
// every client path refuses the plan rather than sending the rest of it: a join missing one
// of its conditions is answered, with too many rows and nothing to say the question changed.
func TestEveryJoinPathRefusesABandItCannotSend(t *testing.T) {
	band := []db.JoinBand{{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi"}}
	pair := db.JoinSpec{
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "ev", Alias: "e"},
		Band:  band,
	}
	chain := db.JoinSpec{
		Left: db.JoinSide{Table: "jobs", Alias: "j"},
		Steps: []db.JoinStep{{Kind: db.JoinInner,
			Side: db.JoinSide{Table: "ev", Alias: "e"}, Band: band}},
	}
	for _, c := range []struct {
		name string
		spec db.JoinSpec
	}{{"pair", pair}, {"chain", chain}} {
		t.Run(c.name, func(t *testing.T) {
			if err := refuseJoinBand(c.spec); !errors.Is(err, ErrJoinBandUnsupported) {
				t.Fatalf("refuseJoinBand returned %v", err)
			}
		})
	}
	// And a plan with no band is not refused, or the guard would take out every join.
	nob := pair
	nob.Band = nil
	if err := refuseJoinBand(nob); err != nil {
		t.Fatalf("a plan with no band was refused: %v", err)
	}
}

// Every exported path that takes a plan, through a real client against a real server. The
// helper above says the refusal is right; this says each path reaches it -- a call site left
// out is exactly the mistake that ships a band-shaped hole.
func TestTheClientRefusesABandOnEveryPath(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	jr := JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "hist", Alias: "h"},
		Band:  []db.JoinBand{{Attr: "ExitCode", Lo: "j.Cpus"}},
	}}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"Join", func() error { return f.c.Join(ctx, jr, func(string) bool { return true }) }},
		{"JoinRows", func() error { _, err := f.c.JoinRows(ctx, jr); return err }},
		{"JoinStats", func() error {
			var rep JoinStatsReport
			return f.c.JoinStats(ctx, jr, func(string) bool { return true }, &rep)
		}},
		{"JoinWire", func() error {
			return f.c.JoinWire(ctx, jr, false, func([]byte) bool { return true })
		}},
		{"JoinWireStats", func() error {
			var rep JoinStatsReport
			return f.c.JoinWireStats(ctx, jr, false, func([]byte) bool { return true }, &rep)
		}},
		{"ExplainJoin", func() error { _, err := f.c.ExplainJoin(ctx, jr); return err }},
		{"JoinAggregate", func() error {
			_, err := f.c.JoinAggregate(ctx, JoinAggRequest{Spec: db.JoinAggSpec{
				Join: jr.Spec, GroupCols: []db.GroupCol{{Attr: "j.Owner"}},
				Aggs: []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
			}})
			return err
		}},
		{"JoinAggregateStats", func() error {
			var rep JoinStatsReport
			_, err := f.c.JoinAggregateStats(ctx, JoinAggRequest{Spec: db.JoinAggSpec{
				Join: jr.Spec, GroupCols: []db.GroupCol{{Attr: "j.Owner"}},
				Aggs: []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
			}}, &rep)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); !errors.Is(err, ErrJoinBandUnsupported) {
				t.Fatalf("%s sent a plan carrying a band, or failed otherwise: %v", c.name, err)
			}
		})
	}
}
