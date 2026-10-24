package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/parser"
)

// TestMatchCurrentTimeScopes pins libclassad's CurrentTime in matchmaking on every match
// entry point, over slots that omit CurrentTime, define it, define it as undefined, or
// define it as an expression. An unscoped or MY.CurrentTime the job lacks is the time,
// whatever the slot holds; TARGET.CurrentTime is the slot's, or the time when the slot
// lacks it; a CurrentTime the job defines as undefined is undefined. Each case lists the
// slots libclassad matches, and every entry point must agree with MatchClassAd.
func TestMatchCurrentTimeScopes(t *testing.T) {
	t.Parallel()
	slots := []string{
		`[ Id = 1; N = 1; Requirements = true ]`,
		`[ Id = 2; N = 1; CurrentTime = 5; Requirements = true ]`,
		`[ Id = 3; N = 1; CurrentTime = undefined; Requirements = true ]`,
		`[ Id = 4; N = 1; Base = 2; CurrentTime = Base + 3; Requirements = true ]`,
		`[ Id = 5; N = 5; Requirements = true ]`,
		// The slot's own Requirements reads its own CurrentTime, never the job's.
		`[ Id = 6; N = 1; Requirements = CurrentTime > 1000000000 ]`,
	}
	const rank = `; Rank = ifThenElse(TARGET.CurrentTime > 1000000000, 1, 2) ]`
	cases := []struct {
		job  string
		want []int
	}{
		{`[ Requirements = TARGET.N > 2 || CurrentTime > 1000000000`, []int{1, 2, 3, 4, 5, 6}},
		{`[ Requirements = TARGET.N > 2 || CurrentTime < 1000`, []int{5}},
		{`[ Requirements = TARGET.N > 2 || MY.CurrentTime > 1000000000`, []int{1, 2, 3, 4, 5, 6}},
		{`[ Requirements = TARGET.N > 2 || TARGET.CurrentTime > 1000000000`, []int{1, 5, 6}},
		{`[ Requirements = TARGET.N > 2 || TARGET.CurrentTime =?= undefined`, []int{3, 5}},
		{`[ Requirements = TARGET.N > 2 || TARGET.CurrentTime == 5`, []int{2, 4, 5}},
		{`[ CurrentTime = undefined; Requirements = TARGET.N > 2 || CurrentTime =?= undefined`, []int{1, 2, 3, 4, 5, 6}},
		{`[ CurrentTime = 7; Requirements = TARGET.N > 2 || CurrentTime == 7`, []int{1, 2, 3, 4, 5, 6}},
		{`[ Deadline = CurrentTime + 100; Requirements = TARGET.N > 2 || Deadline > 1000000000`, []int{1, 2, 3, 4, 5, 6}},
		{`[ Requirements = TARGET.N > 2 && TARGET.CurrentTime =?= undefined`, []int{}},
	}
	build := func(opts Options, pad int) *Collection {
		c := New(opts)
		for i, s := range slots {
			text := s
			if pad > 0 {
				var b strings.Builder
				for k := 0; k < pad; k++ {
					fmt.Fprintf(&b, "Pad%d = %d; ", k, k)
				}
				text = "[ " + b.String() + strings.TrimPrefix(s, "[")
			}
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, text)); err != nil {
				t.Fatal(err)
			}
		}
		c.Reindex()
		return c
	}
	colls := []struct {
		name string
		c    *Collection
	}{
		{"plain", build(Options{}, 0)},
		{"indexed", build(Options{Shards: 2, ValueAttrs: []string{"N", "CurrentTime"}}, 0)},
		{"wide", build(Options{Shards: 2}, closureDecodeMinAttrs)},
	}
	for _, tc := range cases {
		jobText := tc.job + rank
		want := bruteMatch(t, jobText, slots)
		ids := make([]int, 0, len(want))
		for id := range want {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		if fmt.Sprint(ids) != fmt.Sprint(tc.want) {
			t.Errorf("MatchClassAd(%s) matches %v, libclassad matches %v", jobText, ids, tc.want)
		}
		for _, cc := range colls {
			checkEntryPoints(t, cc.name, cc.c, jobText, want)
		}
	}
}

// TestQueryCurrentTimeDefined runs queries reading CurrentTime over records that omit it,
// define it as undefined, or define it as a literal, through the row path (Query, with
// and without an index the planner probes) and the columnar count (CountQuery). A record
// that defines CurrentTime as undefined reads undefined, not the time.
func TestQueryCurrentTimeDefined(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	const n = 1000
	build := func(opts Options) *Collection {
		c := New(opts)
		for i := 0; i < n; i++ {
			src := fmt.Sprintf("CompletionDate = %d", now-2*86400)
			if i%2 == 0 {
				src = fmt.Sprintf("CompletionDate = %d", now-3600)
			}
			switch i % 100 {
			case 0:
				src += "\nCurrentTime = undefined"
			case 50:
				src += "\nCurrentTime = 7"
			}
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	cases := []struct {
		expr string
		want int
	}{
		{`CurrentTime =?= undefined`, n / 100},
		{`CurrentTime == 7`, n / 100},
		{`CompletionDate > CurrentTime - 86400`, n/2 - n/100},
		{`CompletionDate > CurrentTime - 86400 && CurrentTime isnt undefined`, n/2 - n/100},
	}
	for _, opts := range []Options{
		{Shards: 1, SegmentSize: 1 << 14},
		{Shards: 2, SegmentSize: 1 << 14, ValueAttrs: []string{"CompletionDate"}},
	} {
		c := build(opts)
		t.Cleanup(func() { c.Close() })
		for _, tc := range cases {
			if got := rowTruth(t, c, tc.expr); got != tc.want {
				t.Errorf("%+v: Query(%s) = %d records, want %d", opts.ValueAttrs, tc.expr, got, tc.want)
			}
		}
		if !c.BuildAndEnableSchemaScan(n, 4) {
			continue
		}
		for _, tc := range cases {
			q, err := vm.Parse(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if got, served := c.CountQuery(q); served && got != tc.want {
				t.Errorf("%+v: CountQuery(%s) = %d, want %d", opts.ValueAttrs, tc.expr, got, tc.want)
			}
			if got := rowTruth(t, c, tc.expr); got != tc.want {
				t.Errorf("%+v: columnar Query(%s) = %d records, want %d", opts.ValueAttrs, tc.expr, got, tc.want)
			}
		}
	}
}

// TestJobScopeCurrentTime: a CurrentTime the job lacks is the time, not a slot read, so
// it adds nothing to the slot closure; a job attribute reading it is still not baked
// (a value computed once would freeze the clock); and in the slot predicate it is an
// opaque job reference, so its conjunct drops out of the plan instead of making the
// whole plan read CurrentTime and decline.
func TestJobScopeCurrentTime(t *testing.T) {
	t.Parallel()
	job := mustAd(t, `[ Deadline = CurrentTime + 100; Mine = MY.CurrentTime; Plain = 5;
		Requirements = TARGET.N > 2 && Deadline > 0 && CurrentTime > 0 ]`)
	js := jobValues(job)
	for _, name := range []string{"deadline", "mine"} {
		if !js.dep[name] {
			t.Errorf("%s reads the clock but is baked", name)
		}
	}
	if js.dep["plain"] {
		t.Error("plain is marked dependent")
	}
	if reads, ok := js.slotReads(parseExprT(t, `CurrentTime + Deadline`)); !ok || len(reads) != 0 {
		t.Errorf("slotReads(CurrentTime + Deadline) = %v, %v; want none", reads, ok)
	}
	c := New(Options{Shards: 1, ValueAttrs: []string{"N"}})
	for i := 0; i < 8; i++ {
		if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, fmt.Sprintf(`[ N = %d; Requirements = true ]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	if _, prunable := c.slotMatchPlan(job, js); !prunable {
		t.Error("a CurrentTime the job lacks made the slot plan unprunable")
	}
}

func parseExprT(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestChainedQueryCurrentTime: a chained parent is read as part of the ad, as libclassad
// reads a chained parent ad, so a proc sees its cluster's CurrentTime; PARENT.CurrentTime
// on a cluster without it is the time.
func TestChainedQueryCurrentTime(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, ParentKeyFor: jobParentKey, IsStructural: jobStructural})
	for k, src := range map[string]string{
		"1.-1": `[CurrentTime = 7]`, "1.0": `[ProcId = 10]`, "1.1": `[ProcId = 11; CurrentTime = 9]`,
		"2.-1": `[CurrentTime = undefined]`, "2.0": `[ProcId = 20]`,
		"3.-1": `[Owner = "u"]`, "3.0": `[ProcId = 30]`,
	} {
		if err := c.Put([]byte(k), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for expr, want := range map[string]string{
		`CurrentTime == 7`:                 "[10]",
		`MY.CurrentTime == 9`:              "[11]",
		`CurrentTime =?= undefined`:        "[20]",
		`CurrentTime > 1000000000`:         "[30]",
		`PARENT.CurrentTime > 1000000000`:  "[30]",
		`PARENT.CurrentTime =?= undefined`: "[20]",
		`PARENT.CurrentTime == 7`:          "[10 11]",
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for ad := range c.Query(q) {
			p, _ := ad.EvaluateAttrInt("ProcId")
			got = append(got, int(p))
		}
		sort.Ints(got)
		if fmt.Sprint(got) != want {
			t.Errorf("Query(%s) = %v, want %s", expr, got, want)
		}
	}
}
