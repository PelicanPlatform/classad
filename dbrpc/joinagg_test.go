package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// THE ORACLE FOR THE REDUCED JOIN.
//
// opJoinAgg answers the same question db.Catalog.JoinAggregate answers in process, with a
// frame in between. Everything that frame can get wrong is quiet: a group cell read at the
// wrong offset is a group under the wrong label, an aggregate cell read at the wrong offset is
// a number that is still a number, and a batch whose row count disagrees with its payload
// drops groups off the end without saying so. So the test is the two against each other over
// one fixture, compared as rendered groups rather than as bytes.

// aggKey renders one group for comparison: the group's label, its typed value, every
// aggregate value, and what the group lost. Every field that crosses the wire is in here, so
// a cell that arrives wrong changes the string.
func aggKey(row db.AggRow) string {
	var b strings.Builder
	b.WriteString(strings.Join(row.Group, "|"))
	b.WriteString(" @")
	for _, v := range row.GroupValues {
		b.WriteString(db.GroupValueText(v))
		b.WriteByte(',')
	}
	b.WriteString(" = ")
	b.WriteString(strings.Join(row.Values, "|"))
	if len(row.Elided) > 0 {
		fmt.Fprintf(&b, " elided%v", row.Elided)
	}
	return b.String()
}

func aggKeys(rows []db.AggRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, aggKey(r))
	}
	sort.Strings(out)
	return out
}

// newRestrictedClient opens a SECOND connection to the fixture's server and tells the client
// only the features named, so a test can drive a peer that lacks one.
//
// Its own connection, not the fixture's: a client starts a read loop, so two clients sharing
// one connection race for every reply and a request sent by the second may be answered into
// the first. A client that only ever REFUSES can share one; one that sends cannot.
func newRestrictedClient(t *testing.T, f *joinFixture, feats ...Feature) *Client {
	t.Helper()
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(sessionAttrsFor(feats)))
	t.Cleanup(func() { c.Close() })
	return c
}

// exitAggSpec is the reduction every differential case runs: one group per job, and every
// aggregate kind the engine has, including the list-valued one.
func exitAggSpec(kind db.JoinKind) db.JoinAggSpec {
	return db.JoinAggSpec{
		Join: db.JoinSpec{
			Kind:  kind,
			Left:  db.JoinSide{Table: "jobs", Alias: "j"},
			Right: db.JoinSide{Table: "hist", Alias: "h"},
			Cond:  []string{"j.ClusterId == h.ClusterId"},
			Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		},
		GroupCols: []db.GroupCol{{Attr: "j.ClusterId"}},
		Aggs: []db.AggSpec{
			{Func: db.AggCount, Arg: "*"},
			{Func: db.AggCount, Arg: "h.ExitCode"},
			{Func: db.AggSum, Arg: "h.ExitCode"},
			{Func: db.AggMin, Arg: "h.ExitCode"},
			{Func: db.AggMax, Arg: "h.ExitCode"},
			{Func: db.AggList, Arg: "h.ExitCode"},
			{Func: db.AggCount, Arg: "*", Filter: "h.ExitCode == 0"},
		},
	}
}

// TestJoinAggregateOverTheWireMatchesTheInProcessReduction is the oracle.
//
// The fixture carries the three group shapes on purpose: cluster 1 pairs with exactly one
// history row, cluster 2 with two, and cluster 3 with none -- which exists as a group only
// under a LEFT join, where its aggregates are a COUNT of 1 over a row whose right side is
// undefined. A reduction that is right on the many-match group and wrong on the empty one is
// the failure this shape is here to catch.
func TestJoinAggregateOverTheWireMatchesTheInProcessReduction(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	for _, kind := range []db.JoinKind{db.JoinInner, db.JoinLeft} {
		name := map[db.JoinKind]string{db.JoinInner: "inner", db.JoinLeft: "left"}[kind]
		t.Run(name, func(t *testing.T) {
			spec := exitAggSpec(kind)
			want, err := f.cat.JoinAggregate(spec, db.JoinLimits{}, nil, nil)
			if err != nil {
				t.Fatalf("in-process JoinAggregate: %v", err)
			}
			got, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
			if err != nil {
				t.Fatalf("JoinAggregate over the wire: %v", err)
			}
			if len(want) == 0 {
				t.Fatal("the in-process reduction produced no groups, so this compares nothing")
			}
			wantKeys, gotKeys := aggKeys(want), aggKeys(got)
			if len(gotKeys) != len(wantKeys) {
				t.Fatalf("wire gave %d groups %v, in-process gave %d %v",
					len(gotKeys), gotKeys, len(wantKeys), wantKeys)
			}
			for i := range wantKeys {
				if gotKeys[i] != wantKeys[i] {
					t.Fatalf("group %d differs:\n in-process: %s\n      wire: %s",
						i, wantKeys[i], gotKeys[i])
				}
			}
			// The shapes the fixture exists for. Without these the comparison above
			// would pass just as happily over three one-row groups.
			shapes := map[string]bool{}
			for _, r := range want {
				shapes[r.Values[0]] = true
			}
			if !shapes["2"] {
				t.Fatalf("no group reduced TWO joined rows; the shapes were %v", aggKeys(want))
			}
			if kind == db.JoinLeft && len(want) != 3 {
				t.Fatalf("a left join should group all 3 jobs including the unmatched one; got %v",
					aggKeys(want))
			}
		})
	}
}

// TestJoinAggregateReducesWhatTheJoinWouldHaveStreamed is the claim the opcode is for, said as
// a count: the same query answered as rows and as groups, with the groups strictly fewer.
func TestJoinAggregateReducesWhatTheJoinWouldHaveStreamed(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	rows, err := f.c.JoinRows(context.Background(), equiReq(db.JoinInner))
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	spec := exitAggSpec(db.JoinInner)
	groups, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
	if err != nil {
		t.Fatalf("JoinAggregate: %v", err)
	}
	if len(rows) == 0 || len(groups) == 0 {
		t.Fatalf("the join produced %d rows and %d groups; one of them answers nothing",
			len(rows), len(groups))
	}
	if len(groups) >= len(rows) {
		t.Errorf("the reduction returned %d groups for %d joined rows, so it reduced nothing",
			len(groups), len(rows))
	}
}

// TestJoinAggregateListCarriesTheValuesNotAReductionOfThem is what distinguishes the
// list-valued aggregate from the four that came before it. The caller's estimate needs the
// PAIRS of a job's attempts, which no COUNT/SUM/MIN/MAX of either half can reconstruct.
func TestJoinAggregateListCarriesTheValuesNotAReductionOfThem(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	spec := exitAggSpec(db.JoinInner)
	// One cell per group holding the (cluster, exit) pairs. The argument is an expression
	// over the composite row, so the pair is written as the list it is.
	spec.Aggs = []db.AggSpec{
		{Func: db.AggCount, Arg: "*"},
		{Func: db.AggList, Arg: "{h.ClusterId, h.ExitCode}"},
	}
	rows, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
	if err != nil {
		t.Fatalf("JoinAggregate: %v", err)
	}
	var many db.AggRow
	for _, r := range rows {
		if r.Values[0] == "2" {
			many = r
		}
	}
	if many.Values == nil {
		t.Fatalf("no group reduced two rows, so the list has nothing to hold: %v", aggKeys(rows))
	}
	// The cell is a ClassAd literal, which is the contract: a caller reads it back as the
	// value it is rather than parsing a rendering.
	v := db.ParseGroupValue(many.Values[1])
	elems, err := v.ListValue()
	if err != nil {
		t.Fatalf("the list cell %q did not read back as a list: %v", many.Values[1], err)
	}
	if len(elems) != 2 {
		t.Fatalf("the list cell %q holds %d pairs, want 2", many.Values[1], len(elems))
	}
	for _, e := range elems {
		pair, perr := e.ListValue()
		if perr != nil || len(pair) != 2 {
			t.Fatalf("element %s of %q is not a (cluster, exit) pair", db.GroupValueText(e), many.Values[1])
		}
	}
	// Both attempts of cluster 2 are there, exit 1 and exit 7 -- a SUM would have said 8
	// and a MAX 7, and neither is the answer the caller asked for.
	got := db.GroupValueText(v)
	if !strings.Contains(got, "1") || !strings.Contains(got, "7") {
		t.Errorf("the list cell is %s; want both attempts' exit codes", got)
	}
}

// TestJoinAggregateCutsALongListAndSaysSo pins the degrade-with-a-name rule. The cell is cut,
// which is useful, and the cut is reported per cell, which is what keeps a cut list from being
// read as the whole of the group.
func TestJoinAggregateCutsALongListAndSaysSo(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	spec := exitAggSpec(db.JoinInner)
	spec.Aggs = []db.AggSpec{
		{Func: db.AggCount, Arg: "*"},
		{Func: db.AggList, Arg: "h.ExitCode"},
	}
	spec.MaxListValues = 1
	rows, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
	if err != nil {
		t.Fatalf("JoinAggregate: %v", err)
	}
	cut, whole := 0, 0
	for _, r := range rows {
		switch r.Values[0] {
		case "2":
			if len(r.Elided) != 2 || r.Elided[1] != 1 {
				t.Errorf("a group of 2 rows cut to 1 value reported Elided %v, want one "+
					"dropped from aggregate 1", r.Elided)
			}
			if len(r.Elided) == 2 && r.Elided[0] != 0 {
				t.Errorf("the COUNT reported %d elided values; it cannot elide any",
					r.Elided[0])
			}
			// The cut is to the LIST, not to the group: the cell holds one value and
			// the COUNT beside it still counts both rows.
			if cell := db.GroupValueText(db.ParseGroupValue(r.Values[1])); cell != "{ 1 }" && cell != "{ 7 }" {
				t.Errorf("the cut cell is %s, want one of the group's two values", cell)
			}
			cut++
		case "1":
			if len(r.Elided) != 0 {
				t.Errorf("a group of 1 row under a cap of 1 reported Elided %v, want none",
					r.Elided)
			}
			whole++
		}
	}
	if cut == 0 {
		t.Fatal("no group was cut, so the elision report proves nothing")
	}
	if whole == 0 {
		t.Fatal("no group fitted, so 'reports no elision' is vacuous")
	}
	// Uncapped, the same request keeps both values and reports nothing elided -- so the
	// report above is the cap firing rather than a field that is always set.
	spec.MaxListValues = 0
	rows, err = f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
	if err != nil {
		t.Fatalf("JoinAggregate uncapped: %v", err)
	}
	for _, r := range rows {
		if len(r.Elided) != 0 {
			t.Errorf("group %v reported Elided %v under no cap", r.Group, r.Elided)
		}
		if r.Values[0] == "2" && !strings.Contains(r.Values[1], "7") {
			t.Errorf("the uncapped group of 2 is %s; both values should be there", r.Values[1])
		}
	}
}

// TestJoinAggregateGroupCapIsTheRequestsOwn pins the other half of the limit story: too many
// groups is REFUSED, not truncated, and the cap is the request's rather than a global.
func TestJoinAggregateGroupCapIsTheRequestsOwn(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	spec := exitAggSpec(db.JoinInner)
	spec.MaxGroups = 1
	rows, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
	if err == nil {
		t.Fatalf("2 groups under a cap of 1 returned %v and no error", aggKeys(rows))
	}
	if !strings.Contains(err.Error(), "groups") {
		t.Errorf("error = %v, want one naming the group cap", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused reduction returned %d groups; a partial answer must not read "+
			"as a complete one", len(rows))
	}
	// The same request with no cap of its own is served, so the refusal is the cap and not
	// the query.
	spec.MaxGroups = 0
	if _, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec}); err != nil {
		t.Errorf("the same request under the default cap: %v", err)
	}
	// And a cap the answer fits under is served, so "refuses" is not "refuses whenever a
	// cap is named".
	spec.MaxGroups = 1000
	if _, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec}); err != nil {
		t.Errorf("the same request under a cap of 1000: %v", err)
	}
}

// TestJoinAggregateBatchesManyGroupsPerFrame pins the framing. A reduction whose grouping key
// is the entity returns one group per entity, which is the count this opcode exists to serve
// -- a frame per group would put back the cost opJoinWire was added to remove.
func TestJoinAggregateBatchesManyGroupsPerFrame(t *testing.T) {
	f := newCountingJoinFixture(t, 400)
	defer f.close()

	spec := db.JoinAggSpec{
		Join: db.JoinSpec{
			Kind:  db.JoinInner,
			Left:  db.JoinSide{Table: "jobs", Alias: "j"},
			Right: db.JoinSide{Table: "hist", Alias: "h"},
			Cond:  []string{"j.ClusterId == h.ClusterId"},
			Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		},
		GroupCols: []db.GroupCol{{Attr: "j.ClusterId"}},
		Aggs:      []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
	}
	f.count.reset()
	rows, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.count.frames()
	if len(rows) != 400 {
		t.Fatalf("got %d groups, want 400 (one per job)", len(rows))
	}
	// Loose on purpose: the point is the order of magnitude, not a count that breaks when
	// a group's width changes. 400 groups of a few bytes each fit one 64KB frame.
	if frames >= 400/4 {
		t.Errorf("400 groups took %d frames; it is not batching", frames)
	}
	t.Logf("400 groups: %d frames", frames)
}

// TestJoinAggregateIsNotSentToAPeerThatCannotServeIt is the negotiation path a mixed-version
// site actually walks: the client must not send the opcode, and must say so before anything
// crosses, so the caller can reduce the rows itself.
func TestJoinAggregateIsNotSentToAPeerThatCannotServeIt(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	// A peer that advertises the row-streaming join and not this one: a server that can
	// join and has no reduce engine wired to it.
	old := newRestrictedClient(t, f, FeatJoin, FeatJoinWire)
	if _, err := old.JoinAggregate(context.Background(), JoinAggRequest{Spec: exitAggSpec(db.JoinInner)}); !errors.Is(err, ErrJoinAggUnsupported) {
		t.Fatalf("got %v, want ErrJoinAggUnsupported", err)
	}
	// A peer that advertised NOTHING is the same answer by a different rule: this feature
	// postdates advertisement, so silence is conclusive.
	silent := &Client{}
	if silent.SupportsNewer(FeatJoinAgg) {
		t.Error("a peer that advertised nothing was treated as having a feature introduced " +
			"after advertisement existed")
	}
	// And the fixture's own client, which captured the real handshake, does send it --
	// without which the two refusals above would pass against a client that never sends
	// anything.
	if _, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: exitAggSpec(db.JoinInner)}); err != nil {
		t.Fatalf("the advertised peer refused the request: %v", err)
	}
}

// TestJoinAggregateIsRefusedByAServerThatCannotReduce covers the half advertisement cannot:
// a client that sends the opcode anyway must get the same sentinel from the server's refusal
// rather than an opaque bad request.
func TestJoinAggregateIsRefusedByAServerThatCannotReduce(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()

	// The BUILD has the opcode; THIS server has one database and no catalog to join
	// across, so it must not advertise it.
	if got := s.SessionAttrs()[SessionAttrFeatures]; strings.Contains(got, string(FeatJoinAgg)) {
		t.Errorf("a single-database server advertised %q in %q", FeatJoinAgg, got)
	}
	if !strings.Contains(ServerSessionAttrs()[SessionAttrFeatures], string(FeatJoinAgg)) {
		t.Errorf("the BUILD should advertise %q; only this server cannot serve it", FeatJoinAgg)
	}

	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	// A client told the server has the feature although it does not -- which is the case
	// advertisement cannot cover, and the one where the refusal has to reach the caller as
	// the sentinel rather than as a malformed request.
	lying := NewClient(cconn, WithPeerSessionAttrs(sessionAttrsFor([]Feature{FeatJoinAgg})))
	defer lying.Close()
	if !lying.SupportsNewer(FeatJoinAgg) {
		t.Fatal("the test's client does not believe the server has the feature, so it sends nothing")
	}
	if _, err := lying.JoinAggregate(context.Background(), JoinAggRequest{Spec: exitAggSpec(db.JoinInner)}); !errors.Is(err, ErrJoinAggUnsupported) {
		t.Fatalf("got %v, want ErrJoinAggUnsupported", err)
	}
}

// TestJoinAggregateChainNeedsTheChainFeature pins the one thing the opcode's flags byte does
// not decide on its own. A chain rides this opcode, but folding in a third table is the
// catalog's capability and FeatJoinChain is what states it.
func TestJoinAggregateChainNeedsTheChainFeature(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	chain := db.JoinAggSpec{
		Join: db.JoinSpec{
			Left: db.JoinSide{Table: "jobs", Alias: "j"},
			Steps: []db.JoinStep{{
				Kind: db.JoinInner,
				Side: db.JoinSide{Table: "hist", Alias: "h"},
				Cond: []string{"j.ClusterId == h.ClusterId"},
				Equi: []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
			}},
		},
		GroupCols: []db.GroupCol{{Attr: "j.Owner"}},
		Aggs:      []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
	}
	// The real server has both, so the chain is served and the pair below is a refusal
	// rather than a query that never worked.
	rows, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: chain})
	if err != nil {
		t.Fatalf("a chain over a server with both features: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the chain produced no groups, so the served case proves nothing")
	}
	noChain := newRestrictedClient(t, f, FeatJoin, FeatJoinAgg)
	if _, err := noChain.JoinAggregate(context.Background(), JoinAggRequest{Spec: chain}); !errors.Is(err, ErrJoinAggUnsupported) {
		t.Fatalf("a chain sent to a peer without FeatJoinChain gave %v, want ErrJoinAggUnsupported", err)
	}
	// The same peer serves a two-table reduction perfectly well: the refusal is about the
	// chain and not about the opcode.
	if _, err := noChain.JoinAggregate(context.Background(), JoinAggRequest{Spec: exitAggSpec(db.JoinInner)}); err != nil {
		t.Fatalf("a pair over a peer without FeatJoinChain: %v", err)
	}
}

// TestJoinAggregateRefusesAPrivateGroupColumn is the gate that matters most here, and it is
// not the one the row-streaming join needed. A private attribute in a CONDITION is an oracle:
// the value does not come back and the caller narrows it down by asking. A private attribute
// in a GROUP BY is the value itself, because the group's label is what the column evaluated
// to -- so an ungated reduction would be the simplest way to read one out of this server.
func TestJoinAggregateRefusesAPrivateGroupColumn(t *testing.T) {
	f := newPrivateJoinFixture(t)
	defer f.close()

	base := db.JoinAggSpec{
		Join: db.JoinSpec{
			Kind:  db.JoinInner,
			Left:  db.JoinSide{Table: "jobs", Alias: "j"},
			Right: db.JoinSide{Table: "hist", Alias: "h"},
			Cond:  []string{"j.ClusterId == h.ClusterId"},
			Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		},
		GroupCols: []db.GroupCol{{Attr: "j.ClusterId"}},
		Aggs:      []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
	}
	// Unprivileged, so the gate is live.
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: false}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(f.s.SessionAttrs()))
	defer c.Close()

	// The gate is live only if the ordinary request is served: otherwise every refusal
	// below is just a broken fixture.
	if rows, err := c.JoinAggregate(context.Background(), JoinAggRequest{Spec: base}); err != nil || len(rows) == 0 {
		t.Fatalf("the unprivileged baseline reduction gave %d groups, %v", len(rows), err)
	}
	for _, bad := range []struct {
		what string
		mut  func(*db.JoinAggSpec)
	}{
		{"group column", func(s *db.JoinAggSpec) { s.GroupCols = []db.GroupCol{{Attr: "j.ClaimId"}} }},
		{"aggregate argument", func(s *db.JoinAggSpec) {
			s.Aggs = []db.AggSpec{{Func: db.AggList, Arg: "h.ClaimId"}}
		}},
		{"aggregate filter", func(s *db.JoinAggSpec) {
			s.Aggs = []db.AggSpec{{Func: db.AggCount, Arg: "*", Filter: `h.ClaimId == "x"`}}
		}},
	} {
		t.Run(bad.what, func(t *testing.T) {
			spec := base
			bad.mut(&spec)
			rows, err := c.JoinAggregate(context.Background(), JoinAggRequest{Spec: spec})
			if err == nil {
				t.Fatalf("a private attribute in the %s was served: %v", bad.what, aggKeys(rows))
			}
			if !strings.Contains(err.Error(), "private") {
				t.Errorf("error = %v, want one naming the private attribute", err)
			}
		})
	}
	// Privileged, the same group column is served -- so the refusals are the gate and not
	// an attribute that cannot be grouped by.
	priv := base
	priv.GroupCols = []db.GroupCol{{Attr: "j.ClaimId"}}
	if _, err := f.c.JoinAggregate(context.Background(), JoinAggRequest{Spec: priv}); err != nil {
		t.Errorf("a privileged connection should group by a private attribute: %v", err)
	}
}

// TestListAggregateIsRefusedOnTheSingleTableAggregate pins the other half of AggList's
// containment. The function selector is a u8, so a server that does not know this one decodes
// the frame happily and answers that column "undefined" -- a different aggregate reported as
// success. The client refuses before sending rather than discovering it.
func TestListAggregateIsRefusedOnTheSingleTableAggregate(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	list := []AggSpec{{Func: db.AggList, Arg: "Owner"}}
	_, err := f.c.AggregateTable(context.Background(), "jobs", "true", []string{"Owner"}, list)
	if !errors.Is(err, db.ErrListAggregateNeedsBound) {
		t.Fatalf("AggregateTable with a list aggregate gave %v, want ErrListAggregateNeedsBound", err)
	}
	_, err = f.c.AggregateBucketedTable(context.Background(), "jobs", "true",
		[]GroupCol{{Attr: "Owner"}}, list)
	if !errors.Is(err, db.ErrListAggregateNeedsBound) {
		t.Fatalf("AggregateBucketedTable with a list aggregate gave %v, want ErrListAggregateNeedsBound", err)
	}
	// The same call with a COUNT is served, so the refusal is the aggregate and not the
	// table or the connection.
	if _, err := f.c.AggregateTable(context.Background(), "jobs", "true", []string{"Owner"},
		[]AggSpec{{Func: db.AggCount, Arg: "*"}}); err != nil {
		t.Fatalf("a COUNT over the same table: %v", err)
	}
}

// TestJoinAggregateRowCodecRoundTrips exercises the row encoding directly, including the two
// shapes a live reduction will not produce on demand: an elision on an aggregate that is not
// the last one, and a flags byte from a build that knows an extension this one does not.
func TestJoinAggregateRowCodecRoundTrips(t *testing.T) {
	row := db.AggRow{
		Group:       []string{`"alice"`, "7"},
		GroupValues: []classad.Value{classad.NewStringValue("alice"), classad.NewIntValue(7)},
		Values:      []string{"{ 1,2 }", "3", "{ 4 }"},
		Elided:      []int{5, 0, 2},
	}
	frame := appendJoinAggRow(nil, row)
	got, ok := readJoinAggRow(&reader{b: frame}, 2, 3)
	if !ok {
		t.Fatal("a row this package encoded did not decode")
	}
	if strings.Join(got.Group, "|") != strings.Join(row.Group, "|") {
		t.Errorf("group = %v, want %v", got.Group, row.Group)
	}
	if strings.Join(got.Values, "|") != strings.Join(row.Values, "|") {
		t.Errorf("values = %v, want %v", got.Values, row.Values)
	}
	if fmt.Sprint(got.Elided) != fmt.Sprint(row.Elided) {
		t.Errorf("elided = %v, want %v", got.Elided, row.Elided)
	}
	// A flag this build does not know means the bytes after it are whatever that flag
	// introduced, so the row must be refused rather than read at the wrong offsets.
	unknown := append([]byte(nil), frame...)
	unknown[0] |= 0x80
	if _, ok := readJoinAggRow(&reader{b: unknown}, 2, 3); ok {
		t.Error("a row carrying an unknown flag decoded; its cells came from the wrong offsets")
	}
	// A truncated frame is refused too, rather than yielding a row built from whatever the
	// reader produced after it ran out.
	if _, ok := readJoinAggRow(&reader{b: frame[:len(frame)/2]}, 2, 3); ok {
		t.Error("a truncated row decoded")
	}
}
