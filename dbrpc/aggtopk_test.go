package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// seedTopK inserts two owners with distinct QDates, so the k-th newest and k-th oldest of each
// are known by hand: alice holds 10..19, bob holds 100..109.
func seedTopK(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range []struct {
		owner string
		base  int
	}{{"alice", 10}, {"bob", 100}} {
		for i := 0; i < 10; i++ {
			if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", n), fmt.Sprintf(
				"Owner = %q\nQDate = %d\n", o.owner, o.base+i)); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func topKRowsByOwner(t *testing.T, rows []db.AggRow) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range rows {
		key := strings.Join(r.Group, "\x00")
		if u, err := unquoteGroup(key); err == nil {
			key = u
		}
		out[key] = r.Values
	}
	return out
}

// unquoteGroup strips the quotes a string group label carries, so a test can key on the owner
// the way it wrote it.
func unquoteGroup(s string) (string, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], nil
	}
	return s, errors.New("not quoted")
}

// TestTopKThresholdOverRPC is the round trip: the cutoff is computed on the SERVER and only the
// per-group cutoffs cross the wire, which is the whole point of pushing it down.
func TestTopKThresholdOverRPC(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedTopK(t, c)

	rows, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: db.AggTopKThreshold, Arg: "QDate", K: 3, Desc: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := topKRowsByOwner(t, rows)
	// alice holds 10..19, so the 3rd newest is 17; bob holds 100..109, so 107.
	for owner, want := range map[string]string{"alice": "17", "bob": "107"} {
		if len(got[owner]) != 1 || got[owner][0] != want {
			t.Errorf("3rd newest of %s = %v, want [%s]", owner, got[owner], want)
		}
	}
}

// TestTopKThresholdPerSpecParameters is what catches a K or a DESC written once per FRAME rather
// than once per SPEC: three cutoffs over one column, each with its own k and direction, whose
// answers are all different. Written with one spec, every test above still passes.
func TestTopKThresholdPerSpecParameters(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedTopK(t, c)

	rows, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: db.AggTopKThreshold, Arg: "QDate", K: 1, Desc: true},
		{Func: db.AggTopKThreshold, Arg: "QDate", K: 4, Desc: true},
		{Func: db.AggTopKThreshold, Arg: "QDate", K: 2, Desc: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := topKRowsByOwner(t, rows)
	// alice holds 10..19: newest 19, 4th newest 16, 2nd oldest 11.
	// bob holds 100..109: 109, 106, 101.
	for owner, want := range map[string][]string{
		"alice": {"19", "16", "11"},
		"bob":   {"109", "106", "101"},
	} {
		if len(got[owner]) != len(want) {
			t.Fatalf("%s: got %v, want %v", owner, got[owner], want)
		}
		for i := range want {
			if got[owner][i] != want[i] {
				t.Errorf("%s cutoff %d = %q, want %q", owner, i, got[owner][i], want[i])
			}
		}
	}
}

// TestTopKThresholdComposesWithWhere pins that the cutoff is taken over the rows the constraint
// matched, not over the table. A cutoff computed before the WHERE would be a plausible number
// that admits the wrong rows.
func TestTopKThresholdComposesWithWhere(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	seedTopK(t, c)

	rows, err := c.Aggregate(context.Background(),
		`(Owner == "alice" && QDate < 15) || (Owner == "bob" && QDate < 105)`, []string{"Owner"},
		[]AggSpec{{Func: db.AggTopKThreshold, Arg: "QDate", K: 2, Desc: true}})
	if err != nil {
		t.Fatal(err)
	}
	got := topKRowsByOwner(t, rows)
	// alice keeps 10..14, so the 2nd newest is 13; bob keeps 100..104, so 103.
	for owner, want := range map[string]string{"alice": "13", "bob": "103"} {
		if len(got[owner]) != 1 || got[owner][0] != want {
			t.Errorf("2nd newest of %s under the constraint = %v, want [%s]", owner, got[owner], want)
		}
	}
}

// TestTopKThresholdRefusedByAnOldServer is the back-compat rule stated as a test: a server whose
// frame has no field for K must REFUSE, never answer. There is no narrower frame to retry in --
// a cutoff for a k the server picked is a number, and a number is indistinguishable from the
// right answer.
func TestTopKThresholdRefusedByAnOldServer(t *testing.T) {
	c, cleanup := testPairOps(t, func(o op) bool { return o != opAggregateTopK })
	defer cleanup()
	seedTopK(t, c)

	_, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: db.AggTopKThreshold, Arg: "QDate", K: 3, Desc: true},
	})
	if err == nil {
		t.Fatal("a server without the opcode answered a top-K request")
	}
	if !errors.Is(err, ErrTopKAggregateUnsupported) {
		t.Errorf("error = %v, want ErrTopKAggregateUnsupported", err)
	}
}

// TestTopKThresholdRefusedWhenAdvertisedAbsent covers the other half: a server that DID advertise
// its features and did not name this one is a definite no, so the request is refused without
// spending the round trip.
func TestTopKThresholdRefusedWhenAdvertisedAbsent(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()
	// Advertise a feature list that omits the top-K aggregate, which is what an older build's
	// handshake looks like.
	c.peerFeatures = map[Feature]bool{FeatNamedAggregate: true, FeatTypedGroups: true}

	if got := c.Supports(FeatTopKAggregate); got != SupportNo {
		t.Fatalf("Supports(FeatTopKAggregate) = %v, want SupportNo", got)
	}
	_, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: db.AggTopKThreshold, Arg: "QDate", K: 3, Desc: true},
	})
	if !errors.Is(err, ErrTopKAggregateUnsupported) {
		t.Errorf("error = %v, want ErrTopKAggregateUnsupported", err)
	}
}

// TestTopKThresholdIsAdvertised guards the step that is easy to forget: an opcode whose Feature
// is not in serverFeatures leaves every client probing for something the server has.
func TestTopKThresholdIsAdvertised(t *testing.T) {
	for _, f := range serverFeatures {
		if f == FeatTopKAggregate {
			return
		}
	}
	t.Error("FeatTopKAggregate is not in serverFeatures, so no server advertises the opcode it has")
}
