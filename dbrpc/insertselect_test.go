package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// rollupPair builds a two-table catalog -- a job source and a summary target -- served over
// a pipe, which is the shape the roll-up is for.
func rollupPair(t *testing.T) (c *Client, src, summary *db.DB, cleanup func()) {
	t.Helper()
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	src, err = cat.CreateTable("history")
	if err != nil {
		t.Fatal(err)
	}
	summary, err = cat.CreateTable("daily_summaries")
	if err != nil {
		t.Fatal(err)
	}
	tx := src.Begin()
	n := 0
	for _, owner := range []string{"alice", "bob"} {
		for i := 0; i < 3; i++ {
			ad, perr := classad.ParseOld(fmt.Sprintf(
				"Owner = %q\nRequestCpus = %d\nJobStatus = 4", owner, i+1))
			if perr != nil {
				t.Fatal(perr)
			}
			tx.NewClassAd(fmt.Sprintf("job%d", n), ad)
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c = NewClient(cconn)
	return c, src, summary, func() { c.Close(); s.Close() }
}

func TestPrimaryKeyRoundTrip(t *testing.T) {
	c, _, _, cleanup := rollupPair(t)
	defer cleanup()
	ctx := context.Background()

	if cols, err := c.PrimaryKey(ctx, "daily_summaries"); err != nil {
		t.Fatal(err)
	} else if len(cols) != 0 {
		t.Fatalf("a fresh table reported primary key %v, want none", cols)
	}

	if err := c.SetPrimaryKey(ctx, "daily_summaries", []string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	cols, err := c.PrimaryKey(ctx, "daily_summaries")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cols, ",") != "Owner" {
		t.Fatalf("primary key = %v, want [Owner]", cols)
	}
}

func TestSetPrimaryKeyRefusesPrivateAttribute(t *testing.T) {
	c, _, _, cleanup := rollupPair(t)
	defer cleanup()
	err := c.SetPrimaryKey(context.Background(), "daily_summaries", []string{"ClaimId"})
	if err == nil {
		t.Fatal("declaring a private attribute as the key succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Fatalf("error %v should say the attribute is private", err)
	}
}

// The whole point: the reduction and the write both happen on the server and only a count
// comes back.
func TestInsertSelectServerSide(t *testing.T) {
	c, _, summary, cleanup := rollupPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "daily_summaries", []string{"Owner"}); err != nil {
		t.Fatal(err)
	}

	n, err := c.InsertSelect(ctx, "daily_summaries", "history", "true",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}, {Func: db.AggSum, Arg: "RequestCpus"}},
		[]string{"Owner", "Jobs", "Cpus"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("InsertSelect wrote %d rows, want 2", n)
	}

	ad, ok := summary.LookupClassAd(`"alice"`)
	if !ok {
		t.Fatalf("no summary row for alice; keys are %v", summary.Keys())
	}
	jobs := ad.EvaluateAttr("Jobs")
	if !jobs.IsInteger() {
		t.Fatalf("Jobs arrived as a %v, want an integer", jobs)
	}
	if got, _ := jobs.IntValue(); got != 3 {
		t.Fatalf("Jobs = %d, want 3", got)
	}
	if got, _ := ad.EvaluateAttr("Cpus").IntValue(); got != 6 {
		t.Fatalf("Cpus = %d, want 6", got)
	}
}

func TestInsertSelectIsIdempotentOverTheWire(t *testing.T) {
	c, _, summary, cleanup := rollupPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "daily_summaries", []string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	run := func() int {
		n, err := c.InsertSelect(ctx, "daily_summaries", "history", "true",
			[]db.GroupCol{{Attr: "Owner"}},
			[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
			[]string{"Owner", "Jobs"})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	run()
	run()
	if got := summary.Len(); got != 2 {
		t.Fatalf("after two runs the summary holds %d rows, want 2", got)
	}
}

func TestInsertSelectRefusedWithoutDeclaredKey(t *testing.T) {
	c, _, _, cleanup := rollupPair(t)
	defer cleanup()
	_, err := c.InsertSelect(context.Background(), "daily_summaries", "history", "true",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "Jobs"})
	if err == nil {
		t.Fatal("roll-up into a table with no declared key succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "primary key") {
		t.Fatalf("error %v should say the table declares no primary key", err)
	}
}

// An archive is append-only and a view is derived; neither can be a roll-up's target, and
// the refusal should say which it is rather than claim the table does not exist.
func TestInsertSelectRefusesNonTableTarget(t *testing.T) {
	c, _, _, cleanup := rollupPair(t)
	defer cleanup()
	_, err := c.InsertSelect(context.Background(), "nosuchtable", "history", "true",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "Jobs"})
	if err == nil {
		t.Fatal("roll-up into a missing table succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a writable table") {
		t.Fatalf("error %v should say the target is not a writable table", err)
	}
}

// A roll-up is a write, so a read-only connection must not be able to run one.
func TestInsertSelectRefusedOnReadOnlyConnection(t *testing.T) {
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	if _, createErr := cat.CreateTable("history"); createErr != nil {
		t.Fatal(createErr)
	}
	summary, err := cat.CreateTable("daily_summaries")
	if err != nil {
		t.Fatal(err)
	}
	if setErr := summary.SetPrimaryKey([]string{"Owner"}); setErr != nil {
		t.Fatal(setErr)
	}
	s := NewServerCatalog(cat)
	defer s.Close()
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{ReadOnly: true}) }()
	c := NewClient(cconn)
	defer c.Close()

	_, err = c.InsertSelect(context.Background(), "daily_summaries", "history", "true",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "Jobs"})
	if err == nil {
		t.Fatal("a read-only connection ran a roll-up, want a refusal")
	}
}

// The feature must be advertised, or every client goes back to probing for it.
func TestInsertSelectFeaturesAdvertised(t *testing.T) {
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	defer s.Close()
	attrs := s.SessionAttrs()
	feats := attrs[SessionAttrFeatures]
	for _, want := range []Feature{FeatPrimaryKey, FeatInsertSelect} {
		if !strings.Contains(feats, string(want)) {
			t.Errorf("server does not advertise %q; advertised: %s", want, feats)
		}
	}
}

// A roll-up whose SOURCE is an archive. This is the shape the feature is for -- history is
// append-only -- and it reaches a different reduction path from a mutable table's.
func TestInsertSelectFromArchiveSource(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{
		SegmentSize:      1 << 12,
		CategoricalAttrs: []string{"Owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"alice": 5, "bob": 3}
	for owner, n := range want {
		for i := 0; i < n; i++ {
			ad, perr := classad.Parse(fmt.Sprintf(`[ Owner = "%s"; JobStatus = 4 ]`, owner))
			if perr != nil {
				t.Fatal(perr)
			}
			if appendErr := arch.Append(ad); appendErr != nil {
				t.Fatal(appendErr)
			}
		}
	}
	summary, err := cat.CreateTable("daily_summaries")
	if err != nil {
		t.Fatal(err)
	}
	if setErr := summary.SetPrimaryKey([]string{"Owner"}); setErr != nil {
		t.Fatal(setErr)
	}

	s := NewServerCatalog(cat)
	defer s.Close()
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer c.Close()

	n, err := c.InsertSelect(context.Background(), "daily_summaries", "history", "true",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "Jobs"})
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) {
		t.Fatalf("rolled up %d rows from the archive, want %d", n, len(want))
	}
	for owner, jobs := range want {
		ad, ok := summary.LookupClassAd(`"` + owner + `"`)
		if !ok {
			t.Fatalf("no summary row for %s; keys are %v", owner, summary.Keys())
		}
		got, _ := ad.EvaluateAttr("Jobs").IntValue()
		if int(got) != jobs {
			t.Errorf("%s: Jobs = %d, want %d", owner, got, jobs)
		}
	}
}

// A source that does not exist is named as such, rather than failing as an empty roll-up
// that writes nothing and reports success.
func TestInsertSelectRefusesMissingSource(t *testing.T) {
	c, _, _, cleanup := rollupPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "daily_summaries", []string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.InsertSelect(ctx, "daily_summaries", "nosuchsource", "true",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "Jobs"})
	if err == nil {
		t.Fatal("roll-up from a missing source succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("error %v should name the missing source", err)
	}
}

// A bad constraint is reported as the error it is, not as a malformed request.
func TestInsertSelectReportsBadConstraint(t *testing.T) {
	c, _, _, cleanup := rollupPair(t)
	defer cleanup()
	ctx := context.Background()
	if err := c.SetPrimaryKey(ctx, "daily_summaries", []string{"Owner"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.InsertSelect(ctx, "daily_summaries", "history", "this is not )( an expression",
		[]db.GroupCol{{Attr: "Owner"}},
		[]db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		[]string{"Owner", "Jobs"})
	if err == nil {
		t.Fatal("a malformed constraint was accepted, want a refusal")
	}
}
