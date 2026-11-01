package dbrpc

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

type joinFixture struct {
	s   *Server
	c   *Client
	cat *db.Catalog
}

func (f *joinFixture) close() { f.c.Close(); f.s.Close(); f.cat.Close() }

// newJoinFixture builds two tables with a deliberate shape: one job pairs with nothing, one
// history row pairs with nothing, and one job has two history rows.
func newJoinFixture(t *testing.T) *joinFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		t.Fatal(err)
	}
	put := func(d *db.DB, key, text string) {
		ad, perr := classad.Parse(text)
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := d.Put(key, ad); werr != nil {
			t.Fatal(werr)
		}
	}
	put(jobs, "1", `[ ClusterId = 1; Owner = "alice"; Cpus = 2 ]`)
	put(jobs, "2", `[ ClusterId = 2; Owner = "bob";   Cpus = 4 ]`)
	put(jobs, "3", `[ ClusterId = 3; Owner = "alice"; Cpus = 8 ]`)
	put(hist, "a", `[ ClusterId = 1; ExitCode = 0 ]`)
	put(hist, "b", `[ ClusterId = 2; ExitCode = 1 ]`)
	put(hist, "c", `[ ClusterId = 2; ExitCode = 7 ]`)
	put(hist, "d", `[ ClusterId = 9; ExitCode = 0 ]`)

	f := &joinFixture{s: NewServerCatalog(cat), cat: cat}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	f.c = NewClient(cconn)
	return f
}

func equiReq(kind db.JoinKind) JoinRequest {
	return JoinRequest{Spec: db.JoinSpec{
		Kind:  kind,
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
}

// pairs renders each joined row as "owner/exit", sorted, so the comparison does not depend on
// scan order.
func pairs(t *testing.T, rows []string) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	for _, text := range rows {
		ad, err := classad.ParseOld(text)
		if err != nil {
			// The rows go out in new-ClassAd form, being composite ads.
			ad, err = classad.Parse(text)
			if err != nil {
				t.Fatalf("parsing a joined row %q: %v", text, err)
			}
		}
		out = append(out, field(ad, "j.Owner")+"/"+field(ad, "h.ExitCode"))
	}
	sort.Strings(out)
	return out
}

func field(ad *classad.ClassAd, path string) string {
	ex, err := classad.ParseExpr(path)
	if err != nil {
		return "!" + path
	}
	v := ex.Eval(ad)
	switch {
	case v.IsString():
		str, _ := v.StringValue()
		return str
	case v.IsInteger():
		n, _ := v.IntValue()
		return strconv.FormatInt(n, 10)
	}
	return "undefined"
}

func TestJoinOverTheWireReturnsTheJoinedRows(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	rows, err := f.c.JoinRows(context.Background(), equiReq(db.JoinInner))
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	got := pairs(t, rows)
	want := []string{"alice/0", "bob/1", "bob/7"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// The joined rows are what db.Join produces -- the opcode moves the work, it does not change
// the answer. Compared against the in-process join over the same catalog, which is the only
// oracle that cannot drift from the implementation being tested by accident.
func TestJoinOverTheWireAgreesWithTheInProcessJoin(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	cases := map[string]JoinRequest{
		"inner":      equiReq(db.JoinInner),
		"left":       equiReq(db.JoinLeft),
		"cross":      {Spec: db.JoinSpec{Kind: db.JoinCross, Left: db.JoinSide{Table: "jobs", Alias: "j"}, Right: db.JoinSide{Table: "hist", Alias: "h"}}},
		"theta":      {Spec: db.JoinSpec{Kind: db.JoinInner, Left: db.JoinSide{Table: "jobs", Alias: "j"}, Right: db.JoinSide{Table: "hist", Alias: "h"}, Cond: []string{"j.ClusterId < h.ClusterId"}}},
		"post":       {Spec: db.JoinSpec{Kind: db.JoinInner, Left: db.JoinSide{Table: "jobs", Alias: "j"}, Right: db.JoinSide{Table: "hist", Alias: "h"}, Cond: []string{"j.ClusterId == h.ClusterId"}, Equi: []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}}, Post: []string{"h.ExitCode == 0"}}},
		"sideconstr": {Spec: db.JoinSpec{Kind: db.JoinInner, Left: db.JoinSide{Table: "jobs", Alias: "j", Constraint: `Owner == "bob"`}, Right: db.JoinSide{Table: "hist", Alias: "h"}, Cond: []string{"j.ClusterId == h.ClusterId"}, Equi: []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}}}},
		"projected":  {Spec: db.JoinSpec{Kind: db.JoinInner, Left: db.JoinSide{Table: "jobs", Alias: "j", Project: []string{"Owner"}}, Right: db.JoinSide{Table: "hist", Alias: "h", Project: []string{"ExitCode"}}, Cond: []string{"j.ClusterId == h.ClusterId"}}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			wire, err := f.c.JoinRows(context.Background(), req)
			if err != nil {
				t.Fatalf("over the wire: %v", err)
			}
			var st db.JoinStats
			seq, err := f.cat.Join(req.Spec, req.Limits, &st)
			if err != nil {
				t.Fatalf("in process: %v", err)
			}
			var local []string
			for ad := range seq {
				local = append(local, field(ad, "j.Owner")+"/"+field(ad, "h.ExitCode"))
			}
			sort.Strings(local)
			got := pairs(t, wire)
			if strings.Join(got, " ") != strings.Join(local, " ") {
				t.Errorf("the wire join and the in-process join disagree:\n wire: %v\n local: %v", got, local)
			}
		})
	}
}

// A limit met part way through has already had rows sent, so it cannot be reported as a
// refusal. It must still reach the caller: a truncated join that reads as a complete one is
// the failure this whole path exists to avoid.
func TestJoinReportsATruncatedResultAfterTheRows(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()

	req := JoinRequest{
		Spec:   db.JoinSpec{Kind: db.JoinCross, Left: db.JoinSide{Table: "jobs", Alias: "j"}, Right: db.JoinSide{Table: "hist", Alias: "h"}},
		Limits: db.JoinLimits{MaxPairs: 5}, // 12 pairs are available
	}
	rows, err := f.c.JoinRows(context.Background(), req)
	if err == nil {
		t.Fatalf("a 5-pair budget over 12 pairs returned %d rows and no error", len(rows))
	}
	if !strings.Contains(err.Error(), "candidate pairs") {
		t.Errorf("error = %v, want one naming the pair budget", err)
	}
}

// A server that cannot join says so as a bad request, which the client maps to a sentinel so
// a caller falls back to joining the two sides itself rather than reading an empty result as
// "nothing matched".
func TestJoinFallsBackWhenTheServerCannotJoin(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	defer s.Close()
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer c.Close()

	_, jerr := c.JoinRows(context.Background(), equiReq(db.JoinInner))
	if !errors.Is(jerr, ErrJoinUnsupported) {
		t.Errorf("error = %v, want ErrJoinUnsupported", jerr)
	}
}

// An unknown table is a refusal before any row, not an empty stream.
func TestJoinRefusesAnUnknownTable(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	req := equiReq(db.JoinInner)
	req.Spec.Right.Table = "nope"
	if _, err := f.c.JoinRows(context.Background(), req); err == nil {
		t.Errorf("joining against a table that does not exist returned no error")
	}
}
