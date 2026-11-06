package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// typedPair serves one database to two clients: one that was told what the server can do, and
// so asks for typed group cells, and one that was told nothing, which is every client built
// before FeatTypedGroups and so takes the text path. The point of every test here is that the
// two must not be able to tell each other apart.
func typedPair(t *testing.T) (typed, text *Client, cleanup func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	c1conn, s1conn := netPipe()
	c2conn, s2conn := netPipe()
	go func() { _ = s.ServeConn(s1conn) }()
	go func() { _ = s.ServeConn(s2conn) }()
	typed = NewClient(c1conn, WithPeerSessionAttrs(ServerSessionAttrs()))
	text = NewClient(c2conn)
	if !typed.SupportsNewer(FeatTypedGroups) {
		t.Fatal("the advertising client did not see FeatTypedGroups; the test proves nothing")
	}
	if text.SupportsNewer(FeatTypedGroups) {
		t.Fatal("the silent client claims FeatTypedGroups; the test proves nothing")
	}
	return typed, text, func() { typed.Close(); text.Close(); s.Close(); d.Close() }
}

func seedGroupKinds(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// One ad per group shape a group key can take: a string, an integer, a real that is
	// integral (which groups WITH the integer), a non-integral real, a bool, and a list,
	// whose group is its text rather than its value.
	ads := []string{
		`Owner = "alice"` + "\n" + `Shape = 1` + "\n",
		`Owner = "alice"` + "\n" + `Shape = 1.0` + "\n",
		`Owner = "bob"` + "\n" + `Shape = 2.5` + "\n",
		`Owner = "carol"` + "\n" + `Shape = true` + "\n",
		`Owner = "dave"` + "\n" + `Shape = {1,2}` + "\n",
		`Owner = "erin"` + "\n" + `Shape = "1"` + "\n",
	}
	for i, ad := range ads {
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestTypedGroupsAgreeWithTheTextPath is the one that matters: the typed reply and the text
// reply must name every group identically. They are two encodings of one answer, and a client
// that upgrades must not see its labels change underneath it.
//
// It is also the regression for getting this wrong once. A list's group text is a RENDERING
// ("[1 2]"), not a literal ("{ 1,2 }"), so sending the list as a value and rebuilding the
// label from it gave the typed path a different label for the same group.
func TestTypedGroupsAgreeWithTheTextPath(t *testing.T) {
	typed, text, cleanup := typedPair(t)
	defer cleanup()
	seedGroupKinds(t, typed)
	ctx := context.Background()

	for _, groupBy := range [][]string{{"Shape"}, {"Owner"}, {"Owner", "Shape"}} {
		a, err := typed.Aggregate(ctx, "true", groupBy, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatalf("typed aggregate on %v: %v", groupBy, err)
		}
		b, err := text.Aggregate(ctx, "true", groupBy, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatalf("text aggregate on %v: %v", groupBy, err)
		}
		gotTyped := rowsByGroup(t, a)
		gotText := rowsByGroup(t, b)
		if len(gotTyped) != len(gotText) {
			t.Fatalf("GROUP BY %v: typed returned %d groups, text returned %d: %v vs %v",
				groupBy, len(gotTyped), len(gotText), gotTyped, gotText)
		}
		for k, v := range gotText {
			if gotTyped[k] != v {
				t.Errorf("GROUP BY %v: group %q is %q over text and %q over values",
					groupBy, k, v, gotTyped[k])
			}
		}
	}
}

// TestTypedGroupsCarryValuesBothWays pins that GroupValues is filled whichever path answered:
// the whole reason the fallback reads the text back is so a caller needs one code path.
func TestTypedGroupsCarryValuesBothWays(t *testing.T) {
	typed, text, cleanup := typedPair(t)
	defer cleanup()
	seedGroupKinds(t, typed)
	ctx := context.Background()

	for name, c := range map[string]*Client{"typed": typed, "text": text} {
		rows, err := c.Aggregate(ctx, "true", []string{"Shape"}, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s: no rows", name)
		}
		for _, r := range rows {
			if len(r.GroupValues) != len(r.Group) {
				t.Fatalf("%s: group %v has %d values for %d columns",
					name, r.Group, len(r.GroupValues), len(r.Group))
			}
			// The value beside a label must name the label's own group. A list is the
			// documented exception: its group is text, and it arrives as that text.
			if got, want := db.GroupValueText(r.GroupValues[0]), r.Group[0]; got != want {
				if s, serr := r.GroupValues[0].StringValue(); serr == nil && s == want {
					continue // the list/nested-ad case, carrying its group text
				}
				t.Errorf("%s: group %q carries a value that renders as %q", name, want, got)
			}
		}
	}
}

func rowsByGroup(t *testing.T, rows []AggRow) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range rows {
		key := ""
		for i, g := range r.Group {
			if i > 0 {
				key += "\x00"
			}
			key += g
		}
		out[key] = r.Values[0]
	}
	return out
}
