package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// oldServerConn is a server-side transport that refuses the opcodes an older build would
// not recognize: it answers them with respBad exactly as the dispatcher's default does, and
// forwards everything else. That makes the back-compat path testable for real rather than
// by inspection -- the client must see the refusal, never an unfiltered answer.
type oldServerConn struct {
	MsgConn
	knows func(op) bool
}

func (c *oldServerConn) ReadMsg() ([]byte, error) {
	for {
		frame, err := c.MsgConn.ReadMsg()
		if err != nil {
			return nil, err
		}
		o, ok := frameOp(frame)
		if !ok || c.knows(o) {
			return frame, nil
		}
		reqID, _ := frameReqID(frame)
		if err := c.MsgConn.WriteMsg(respBad(reqID)); err != nil {
			return nil, err
		}
	}
}

// testPairOps is testPair against a server that only knows the opcodes knows() accepts.
func testPairOps(t *testing.T, knows func(op) bool) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(&oldServerConn{MsgConn: sconn, knows: knows}) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

// seedStatusMix inserts jobs with a status/owner mix, the shape a per-status dashboard
// pivot runs over.
//
//	alice: 2 completed (4), 1 running (2), 1 held (5)
//	bob:   1 completed,     2 running
func seedStatusMix(t *testing.T, c *Client) {
	t.Helper()
	rows := []struct {
		owner  string
		status int
		cpus   int
	}{
		{"alice", 4, 1}, {"alice", 4, 2}, {"alice", 2, 4}, {"alice", 5, 8},
		{"bob", 4, 1}, {"bob", 2, 2}, {"bob", 2, 16},
	}
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), fmt.Sprintf(
			"Owner = \"%s\"\nJobStatus = %d\nCpus = %d\n", r.owner, r.status, r.cpus)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestAggregateFilterOverRPC is the end-to-end pivot: total plus per-status counts for every
// owner, from one aggregation over the wire.
func TestAggregateFilterOverRPC(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedStatusMix(t, c)

	rows, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: AggCount, Arg: "*"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 4"},
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"},
		{Func: AggSum, Arg: "Cpus", Filter: "JobStatus == 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	want := map[string][]string{
		"alice": {"4", "2", "1", "4"},
		"bob":   {"3", "1", "2", "18"},
	}
	for owner, w := range want {
		g := got[owner]
		if len(g) != len(w) {
			t.Errorf("%s = %v, want %v", owner, g, w)
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("%s = %v, want %v", owner, g, w)
				break
			}
		}
	}
}

// TestAggregateFilterCountFastPath is the regression for the bug the db tests caught: an
// ungrouped COUNT(*) over a match-all constraint has a fast path that answers from the
// tracked row count. A FILTERED count must not take it, or it would silently report the
// total.
func TestAggregateFilterCountFastPath(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedStatusMix(t, c)
	ctx := context.Background()

	all, err := c.Aggregate(ctx, "true", nil, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Values[0] != "7" {
		t.Fatalf("unfiltered count = %s, want 7", all[0].Values[0])
	}
	filtered, err := c.Aggregate(ctx, "true", nil, []AggSpec{
		{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered[0].Values[0] != "3" {
		t.Errorf("filtered count = %s, want 3 (the fast path must not answer this)",
			filtered[0].Values[0])
	}
}

// TestAggregateFilterBucketed covers the bucketed opcode's filtered form.
func TestAggregateFilterBucketed(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedStatusMix(t, c)

	rows, err := c.AggregateBucketed(context.Background(), "true",
		[]GroupCol{{Attr: "Owner"}}, []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"},
		})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	if g := got["alice"]; len(g) != 2 || g[0] != "4" || g[1] != "1" {
		t.Errorf("alice = %v, want [4 1]", g)
	}
	if g := got["bob"]; len(g) != 2 || g[0] != "3" || g[1] != "2" {
		t.Errorf("bob = %v, want [3 2]", g)
	}
}

// TestAggregateFilterUnsupportedServer is the back-compat guarantee. A server that does not
// know the filtered opcodes must produce an ERROR, never an unfiltered answer -- the whole
// reason these are separate opcodes rather than an extra field.
func TestAggregateFilterUnsupportedServer(t *testing.T) {
	c, cleanup := testPairOps(t, func(o op) bool {
		return o != opAggregateFiltered && o != opArchiveAggregateFiltered
	})
	defer cleanup()
	seedStatusMix(t, c)
	ctx := context.Background()

	// An unfiltered aggregate is unaffected: it never reaches for the new opcode.
	if _, err := c.Aggregate(ctx, "true", []string{"Owner"},
		[]AggSpec{{Func: AggCount, Arg: "*"}}); err != nil {
		t.Fatalf("unfiltered aggregate should still work against an old server: %v", err)
	}
	// A filtered one is refused, distinctly, so a caller cannot mistake it for an answer.
	_, err := c.Aggregate(ctx, "true", []string{"Owner"},
		[]AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"}})
	if !errors.Is(err, ErrFilteredAggregateUnsupported) {
		t.Errorf("filtered aggregate against an old server: err = %v, want ErrFilteredAggregateUnsupported", err)
	}
	_, err = c.AggregateBucketed(ctx, "true", []GroupCol{{Attr: "Owner"}},
		[]AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"}})
	if !errors.Is(err, ErrFilteredAggregateUnsupported) {
		t.Errorf("filtered bucketed aggregate: err = %v, want ErrFilteredAggregateUnsupported", err)
	}
}

// TestAggregateFilterPrivateAttr checks that a filter cannot be used to read a private
// attribute an unprivileged connection may not aggregate.
func TestAggregateFilterPrivateAttr(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedStatusMix(t, c)

	_, err := c.Aggregate(context.Background(), "true", nil, []AggSpec{
		{Func: AggCount, Arg: "*", Filter: `_condor_privSecret == "x"`},
	})
	if err == nil {
		t.Error("a filter over a private attribute should be refused")
	}
}
