package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// A user-defined aggregate reduces in the db package and is NAMED in the frames the *Named
// opcodes carry, so these tests are about the name surviving the trip -- and about what happens
// when it cannot, which is an error rather than a different aggregate's answer.
//
// WHY THESE TESTS ARE SHAPED THE WAY THEY ARE. db's registry is process-global, and in a dbrpc
// test both ends live in one process, so an aggregate registered "on the server" is visible to
// the client too. A test that registers one aggregate and queries it therefore proves nothing:
// it passes just as well if the name were dropped on the floor and the server picked the only
// registration it had. So each round-trip below asks for TWO DIFFERENT aggregates over the SAME
// argument in one request and checks that each column got its own answer -- which no amount of
// guessing produces -- and a companion test asks for a name registered on neither side.
//
// RegisterAggregate refuses a name that is taken, so every name here is unique to its test.

// minInt and maxInt are two aggregates that disagree on every input but the degenerate one, so a
// request naming both over one attribute distinguishes "the name crossed" from "the server
// guessed". They are deliberately ordinary -- the point under test is the wire, not the
// reduction.
type extremeInt struct{ max bool }

func (e extremeInt) NewAccumulator() db.Accumulator { return &extremeIntAcc{max: e.max} }

type extremeIntAcc struct {
	max bool
	n   int64
	set bool
}

func (a *extremeIntAcc) Add(v classad.Value) {
	i, err := v.IntValue()
	if err != nil {
		return
	}
	if !a.set || (a.max && i > a.n) || (!a.max && i < a.n) {
		a.n, a.set = i, true
	}
}

func (a *extremeIntAcc) Merge(other db.Accumulator) {
	if o := other.(*extremeIntAcc); o.set {
		a.Add(classad.NewIntValue(o.n))
	}
}

func (a *extremeIntAcc) Result() classad.Value {
	if !a.set {
		return classad.NewUndefinedValue()
	}
	return classad.NewIntValue(a.n)
}

// registerExtremes registers a fresh min/max pair under names unique to the caller and returns
// them. Unique because RegisterAggregate fails rather than overwriting, so two tests sharing a
// name would make the second depend on the first having run.
func registerExtremes(t *testing.T, suffix string) (minName, maxName string) {
	t.Helper()
	minName, maxName = "udafwire_lo_"+suffix, "udafwire_hi_"+suffix
	if err := db.RegisterAggregate(minName, extremeInt{}); err != nil {
		t.Fatalf("RegisterAggregate(%q): %v", minName, err)
	}
	if err := db.RegisterAggregate(maxName, extremeInt{max: true}); err != nil {
		t.Fatalf("RegisterAggregate(%q): %v", maxName, err)
	}
	return minName, maxName
}

// TestNamedAggregateOverRPC is the round trip: two different aggregates over one attribute in one
// request, each answering its own way. alice's Cpus are 1,2,4,8 and bob's are 1,2,16 (seedStatusMix),
// so lo != hi for both groups and neither column can be produced by answering with the other.
func TestNamedAggregateOverRPC(t *testing.T) {
	lo, hi := registerExtremes(t, "table")
	c, cleanup := testPair(t)
	defer cleanup()
	seedStatusMix(t, c)

	rows, err := c.Aggregate(context.Background(), "true", []string{"Owner"}, []AggSpec{
		{Func: db.AggUser, UserFunc: lo, Arg: "Cpus"},
		{Func: db.AggUser, UserFunc: hi, Arg: "Cpus"},
		{Func: AggCount, Arg: "*"}, // a built-in rides along on the same frame
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	for owner, want := range map[string][]string{
		`"alice"`: {"1", "8", "4"},
		`"bob"`:   {"1", "16", "3"},
	} {
		g := got[owner]
		if len(g) != 3 || g[0] != want[0] || g[1] != want[1] || g[2] != want[2] {
			t.Errorf("%s = %v, want %v", owner, g, want)
		}
	}
}

// TestNamedAggregateBucketedOverRPC covers the bucketed entry point, which reaches the same
// opcode by another route, and proves a named aggregate composes with a FILTER -- the two
// extensions the named frame is a superset of.
func TestNamedAggregateBucketedOverRPC(t *testing.T) {
	lo, hi := registerExtremes(t, "bucketed")
	c, cleanup := testPair(t)
	defer cleanup()
	seedStatusMix(t, c)

	rows, err := c.AggregateBucketed(context.Background(), "true", []GroupCol{{Attr: "Owner"}},
		[]AggSpec{
			{Func: db.AggUser, UserFunc: hi, Arg: "Cpus"},
			{Func: db.AggUser, UserFunc: hi, Arg: "Cpus", Filter: "JobStatus == 2"},
			{Func: db.AggUser, UserFunc: lo, Arg: "Cpus", Filter: "JobStatus == 2"},
		})
	if err != nil {
		t.Fatalf("AggregateBucketed: %v", err)
	}
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Group[0]] = r.Values
	}
	// alice: cpus 1,2,4,8 with statuses 4,4,2,5 -> running set {4}. bob: 1,2,16 / 4,2,2 -> {2,16}.
	for owner, want := range map[string][]string{
		`"alice"`: {"8", "4", "4"},
		`"bob"`:   {"16", "16", "2"},
	} {
		g := got[owner]
		if len(g) != 3 || g[0] != want[0] || g[1] != want[1] || g[2] != want[2] {
			t.Errorf("%s = %v, want %v", owner, g, want)
		}
	}
}

// TestNamedAggregateOverRPCArchive is the same round trip against a history table, which reduces
// through its own ladder behind its own opcode.
func TestNamedAggregateOverRPCArchive(t *testing.T) {
	lo, hi := registerExtremes(t, "archive")
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i := 1; i <= 20; i++ {
		if err := c.ArchiveAppend(ctx, "history",
			fmt.Sprintf("ClusterId = %d\nCpus = %d\nOwner = \"alice\"", i, i)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	rows, err := c.ArchiveAggregate(ctx, "history", "true", []string{"Owner"}, []AggSpec{
		{Func: db.AggUser, UserFunc: lo, Arg: "Cpus"},
		{Func: db.AggUser, UserFunc: hi, Arg: "Cpus"},
	})
	if err != nil {
		t.Fatalf("ArchiveAggregate: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Values) != 2 {
		t.Fatalf("want one row with two values, got %+v", rows)
	}
	if rows[0].Values[0] != "1" || rows[0].Values[1] != "20" {
		t.Errorf("archive [lo hi] = %v, want [1 20]", rows[0].Values)
	}

	// The stats entry point reaches the same opcode, asking for the trailer in the frame
	// rather than on an opcode of its own, so the rows AND the breakdown must both arrive.
	var stats collections.ScanStats
	rows, err = c.ArchiveAggregateStats(ctx, "history", "true", nil,
		[]AggSpec{{Func: db.AggUser, UserFunc: hi, Arg: "Cpus"}}, &stats)
	if err != nil {
		t.Fatalf("ArchiveAggregateStats: %v", err)
	}
	if len(rows) != 1 || rows[0].Values[0] != "20" {
		t.Fatalf("ArchiveAggregateStats rows = %+v, want one row holding 20", rows)
	}
	if stats.RecordsVisited == 0 {
		t.Error("stats.RecordsVisited = 0: the named aggregate's scan was not reported over the wire")
	}
}

// TestNamedAggregateUnknownNameFails is the other half of proving the name travels: a name
// registered NOWHERE must fail the query, with the cause the db package gives it, rather than
// answering that column undefined. A column of undefined where p95 was asked for is an answer,
// and a wrong one.
func TestNamedAggregateUnknownNameFails(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{})
	defer cleanup()
	ctx := context.Background()
	if err := c.CreateTable(ctx, DefaultTable); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	seedStatusMix(t, c)
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	if err := c.ArchiveAppend(ctx, "history", "ClusterId = 1\nCpus = 3"); err != nil {
		t.Fatalf("ArchiveAppend: %v", err)
	}

	for _, call := range []struct {
		name string
		run  func() ([]AggRow, error)
	}{
		{"Aggregate", func() ([]AggRow, error) {
			return c.Aggregate(ctx, "true", nil,
				[]AggSpec{{Func: db.AggUser, UserFunc: "udafwire_never_registered", Arg: "Cpus"}})
		}},
		{"AggregateBucketed", func() ([]AggRow, error) {
			return c.AggregateBucketed(ctx, "true", []GroupCol{{Attr: "Owner"}},
				[]AggSpec{{Func: db.AggUser, UserFunc: "udafwire_never_registered", Arg: "Cpus"}})
		}},
		{"ArchiveAggregate", func() ([]AggRow, error) {
			return c.ArchiveAggregate(ctx, "history", "true", nil,
				[]AggSpec{{Func: db.AggUser, UserFunc: "udafwire_never_registered", Arg: "Cpus"}})
		}},
		{"ArchiveAggregateStats", func() ([]AggRow, error) {
			return c.ArchiveAggregateStats(ctx, "history", "true", nil,
				[]AggSpec{{Func: db.AggUser, UserFunc: "udafwire_never_registered", Arg: "Cpus"}}, nil)
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			rows, err := call.run()
			if err == nil {
				t.Fatalf("an unregistered aggregate was ANSWERED: %+v", rows)
			}
			if !errors.Is(err, db.ErrNoSuchAggregate) {
				t.Fatalf("err = %v, want db.ErrNoSuchAggregate", err)
			}
			// Not mistakable for the protocol refusal, which has a different fix.
			if errors.Is(err, ErrNamedAggregateUnsupported) {
				t.Errorf("an unknown NAME was reported as an unsupported PROTOCOL: %v", err)
			}
		})
	}
}

// TestNamedAggregateUnsupportedServer is the back-compat guarantee. A server that does not know
// the named opcodes must produce an ERROR -- never a built-in's answer, never an undefined
// column -- because the only frame it could have read drops the name.
func TestNamedAggregateUnsupportedServer(t *testing.T) {
	lo, hi := registerExtremes(t, "oldserver")
	c, cleanup := testPairOps(t, func(o op) bool {
		return o != opAggregateNamed && o != opArchiveAggregateNamed
	})
	defer cleanup()
	seedStatusMix(t, c)
	ctx := context.Background()

	// An aggregate that names nothing is unaffected: it never reaches for the new opcode.
	if _, err := c.Aggregate(ctx, "true", []string{"Owner"},
		[]AggSpec{{Func: AggCount, Arg: "*"}}); err != nil {
		t.Fatalf("an ordinary aggregate should still work against an old server: %v", err)
	}
	for _, call := range []struct {
		name string
		run  func() ([]AggRow, error)
	}{
		{"Aggregate", func() ([]AggRow, error) {
			return c.Aggregate(ctx, "true", []string{"Owner"},
				[]AggSpec{{Func: db.AggUser, UserFunc: lo, Arg: "Cpus"}})
		}},
		{"AggregateBucketed", func() ([]AggRow, error) {
			return c.AggregateBucketed(ctx, "true", []GroupCol{{Attr: "Owner"}},
				[]AggSpec{{Func: db.AggUser, UserFunc: hi, Arg: "Cpus"}})
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			rows, err := call.run()
			if err == nil {
				t.Fatalf("an old server ANSWERED a named aggregate: %+v", rows)
			}
			if !errors.Is(err, ErrNamedAggregateUnsupported) {
				t.Fatalf("err = %v, want ErrNamedAggregateUnsupported", err)
			}
			// The former name is the same value, so a caller written against it keeps matching.
			if !errors.Is(err, ErrUserAggregateUnsupported) {
				t.Errorf("the deprecated alias should still match")
			}
		})
	}
}

// TestArchiveAggregateRefusalNamesTheRightLimit pins which "unsupported" an old server's refusal
// maps to, which is not cosmetic. ErrArchiveAggregateUnsupported tells the caller it may fetch the
// rows and reduce them itself; the other two tell it that it may NOT, because dropping a filter or
// an aggregate's name changes the question rather than only where it is answered. A bucket width
// alone widens the FRAME without changing the question, so it stays in the first class -- which is
// easy to lose when one value starts describing both the frame and the request.
func TestArchiveAggregateRefusalNamesTheRightLimit(t *testing.T) {
	lo, _ := registerExtremes(t, "archiveold")
	c, cleanup := testPairOps(t, func(o op) bool {
		switch o {
		case opArchiveAggregate, opArchiveAggregateFiltered, opArchiveAggregateStats,
			opArchiveAggregateNamed:
			return false
		}
		return true
	})
	defer cleanup()
	ctx := context.Background()

	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	for _, tc := range []struct {
		name   string
		groups []GroupCol
		aggs   []AggSpec
		want   error
	}{
		{"plain", nil, count, ErrArchiveAggregateUnsupported},
		{"width only", []GroupCol{{Attr: "QDate", BucketWidth: 86400}}, count, ErrArchiveAggregateUnsupported},
		{"filtered", nil, []AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"}}, ErrExtendedAggregateUnsupported},
		{"named", nil, []AggSpec{{Func: db.AggUser, UserFunc: lo, Arg: "Cpus"}}, ErrNamedAggregateUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ArchiveAggregateBucketed(ctx, "history", "true", tc.groups, tc.aggs)
			if !errors.Is(err, tc.want) {
				t.Errorf("ArchiveAggregateBucketed: err = %v, want %v", err, tc.want)
			}
		})
	}
	// The stats entry point always sends the extended frame, which is a property of the
	// opcode and not of the request, so an unfiltered one still falls back as a plain
	// archive aggregate.
	if _, err := c.ArchiveAggregateStats(ctx, "history", "true", nil, count, nil); !errors.Is(err, ErrArchiveAggregateUnsupported) {
		t.Errorf("ArchiveAggregateStats: err = %v, want ErrArchiveAggregateUnsupported", err)
	}
}

// TestNamedAggregateRefusedByAdvertisement covers the other way a client learns it cannot ask: a
// peer that advertised its features and did not name this one. That is a definite no, so the
// request must not be issued at all -- there is nothing to learn from a round trip to a refusal
// already in hand.
func TestNamedAggregateRefusedByAdvertisement(t *testing.T) {
	lo, _ := registerExtremes(t, "advertised")
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	// A peer that advertises a feature list without this one. The server behind the pipe can in
	// fact serve it, which is the point: the refusal must come from the advertisement.
	c := NewClient(cconn, WithPeerSessionAttrs(map[string]string{
		SessionAttrProtocol: "3",
		SessionAttrFeatures: string(FeatExtendedAggregate),
	}))
	defer func() { c.Close(); s.Close(); d.Close() }()

	if got := c.Supports(FeatNamedAggregate); got != SupportNo {
		t.Fatalf("Supports(FeatNamedAggregate) = %v, want SupportNo", got)
	}
	_, err = c.Aggregate(context.Background(), "true", nil,
		[]AggSpec{{Func: db.AggUser, UserFunc: lo, Arg: "Cpus"}})
	if !errors.Is(err, ErrNamedAggregateUnsupported) {
		t.Fatalf("err = %v, want ErrNamedAggregateUnsupported", err)
	}
}

// TestClientRefusesAnUnnamedUserAggregate: AggUser with no name has nothing to resolve. Caught
// locally, because the server can only answer it as a malformed frame, which is what a server
// too old to know the opcode answers -- and those need different fixes.
func TestClientRefusesAnUnnamedUserAggregate(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	_, err := c.Aggregate(context.Background(), "true", nil,
		[]AggSpec{{Func: db.AggUser, Arg: "Cpus"}})
	if err == nil {
		t.Fatal("a user aggregate naming nothing was accepted")
	}
	if errors.Is(err, ErrNamedAggregateUnsupported) {
		t.Errorf("a local mistake was reported as the server being too old: %v", err)
	}
}

// TestServerRefusesAUserAggregateFrame goes under the client, because the client builds the frame
// that can carry a name. A server trusting the selector on a frame that CANNOT carry one would
// reach the engine asking for the aggregate registered under the EMPTY NAME.
func TestServerRefusesAUserAggregateFrame(t *testing.T) {
	for _, form := range []aggSpecForm{aggSpecBase, aggSpecFiltered, aggSpecNamed} {
		var b []byte
		b = putI32(b, 1)
		b = putStr(putU8(b, byte(db.AggUser)), "Cpus")
		if form >= aggSpecFiltered {
			b = putStr(b, "")
		}
		if form >= aggSpecNamed {
			b = putStr(b, "") // the named frame, with the name left blank
		}
		var wrote [][]byte
		aggs, ok := readAggSpecs(&reader{b: b}, 7, func(f []byte) { wrote = append(wrote, f) }, form)
		if ok {
			t.Fatalf("form=%v: the server accepted a frame naming no aggregate: %+v", form, aggs)
		}
		if len(wrote) != 1 {
			t.Fatalf("form=%v: want one refusal frame, got %d", form, len(wrote))
		}
	}
}

// TestServerReadsANamedAggregateFrame is the decoder's own test: the name must come back off the
// frame, and must not be read from the filter's bytes or vice versa.
func TestServerReadsANamedAggregateFrame(t *testing.T) {
	var b []byte
	b = putI32(b, 2)
	b = putStr(putStr(putStr(putU8(b, byte(db.AggUser)), "Cpus"), "JobStatus == 2"), "p95")
	b = putStr(putStr(putStr(putU8(b, byte(db.AggCount)), "*"), ""), "")
	aggs, ok := readAggSpecs(&reader{b: b}, 7, func([]byte) { t.Fatal("refused a well formed frame") }, aggSpecNamed)
	if !ok || len(aggs) != 2 {
		t.Fatalf("ok=%v aggs=%+v", ok, aggs)
	}
	if aggs[0].Func != db.AggUser || aggs[0].Arg != "Cpus" ||
		aggs[0].Filter != "JobStatus == 2" || aggs[0].UserFunc != "p95" {
		t.Errorf("decoded the named spec wrong: %+v", aggs[0])
	}
	if aggs[1].Func != db.AggCount || aggs[1].UserFunc != "" {
		t.Errorf("decoded the built-in spec wrong: %+v", aggs[1])
	}
}

// TestServerStillReadsTheAggregatesItCanServe guards the refusal above from being a refusal of
// everything, which would pass the test it was written for and break every aggregate there is.
func TestServerStillReadsTheAggregatesItCanServe(t *testing.T) {
	var b []byte
	b = putI32(b, 2)
	b = putStr(putU8(b, byte(db.AggCount)), "*")
	b = putStr(putU8(b, byte(db.AggCountDistinct)), "Owner")
	aggs, ok := readAggSpecs(&reader{b: b}, 7, func([]byte) { t.Fatal("refused an ordinary request") }, aggSpecBase)
	if !ok || len(aggs) != 2 {
		t.Fatalf("ok=%v aggs=%+v", ok, aggs)
	}
	if aggs[0].Func != db.AggCount || aggs[1].Func != db.AggCountDistinct {
		t.Fatalf("decoded the wrong functions: %+v", aggs)
	}
}

// TestNamedAggregateShapeNamesTheAggregate guards the slow-query report from a blindness the
// selector alone would cause: every user-defined aggregate shares one selector, so p95 and p99
// over the same attribute would be one shape and the report could not say which was slow.
func TestNamedAggregateShapeNamesTheAggregate(t *testing.T) {
	got := aggShape([]AggSpec{
		{Func: db.AggUser, UserFunc: "P95", Arg: "Cpus"},
		{Func: db.AggUser, UserFunc: "p99", Arg: "Cpus"},
		{Func: AggCount, Arg: "*"},
	})
	if want := "p95(cpus), p99(cpus), count(*)"; got != want {
		t.Errorf("aggShape = %q, want %q", got, want)
	}
}

// TestUnnamedAggregatesKeepTheirFrames is the compatibility guarantee stated as a test: a request
// that names no user-defined aggregate must still go out on the opcode it went out on before, so
// an older server keeps serving every query it served.
func TestUnnamedAggregatesKeepTheirFrames(t *testing.T) {
	for _, tc := range []struct {
		name string
		aggs []AggSpec
		want aggSpecForm
	}{
		{"plain", []AggSpec{{Func: AggCount, Arg: "*"}}, aggSpecBase},
		{"filtered", []AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 2"}}, aggSpecFiltered},
		{"countdistinct", []AggSpec{{Func: AggCountDistinct, Arg: "Owner"}}, aggSpecFiltered},
		{"named", []AggSpec{{Func: db.AggUser, UserFunc: "p95", Arg: "Cpus"}}, aggSpecNamed},
		{"mixed", []AggSpec{
			{Func: AggCount, Arg: "*"},
			{Func: db.AggUser, UserFunc: "p95", Arg: "Cpus"},
		}, aggSpecNamed},
	} {
		if got := aggSpecFormFor(tc.aggs); got != tc.want {
			t.Errorf("%s: aggSpecFormFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}
