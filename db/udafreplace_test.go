package db

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// doubleInts is sumInts' replacement throughout this file: the same shape, a visibly different
// answer, so "which implementation ran" is readable off a single number.
type doubleInts struct{}

func (doubleInts) NewAccumulator() Accumulator { return &doubleIntsAcc{} }

type doubleIntsAcc struct{ n int64 }

func (a *doubleIntsAcc) Add(v classad.Value) {
	if i, err := v.IntValue(); err == nil {
		a.n += 2 * i
	}
}
func (a *doubleIntsAcc) Merge(other Accumulator) { a.n += other.(*doubleIntsAcc).n }
func (a *doubleIntsAcc) Result() classad.Value   { return classad.NewIntValue(a.n) }

// mustReplace installs agg under name, which is the idempotent way to set a test's starting state
// in a process-wide registry: it does not care what an earlier test, or an earlier run of this one
// under -count, left behind.
func mustReplace(t *testing.T, name string, agg Aggregate) string {
	t.Helper()
	if err := ReplaceAggregate(name, agg); err != nil {
		t.Fatalf("ReplaceAggregate(%q): %v", name, err)
	}
	return name
}

// sumOf reduces one ungrouped user-defined aggregate over rows, through the same engine a query
// uses, and returns the rendered answer.
func sumOf(t *testing.T, name string, rows [][]classad.Value) string {
	t.Helper()
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	out, err := AggregateValues(rowSeq(rows), attrs, nil, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues(%q): %v", name, err)
	}
	if len(out) != 1 {
		t.Fatalf("want one ungrouped row, got %+v", out)
	}
	return out[0].Values[0]
}

func TestReplaceAggregateInstallsOverWhateverIsThere(t *testing.T) {
	name := mustReplace(t, "rep_sum", sumInts{})
	if got, ok := LookupAggregate(name); !ok {
		t.Fatal("ReplaceAggregate did not register an absent name")
	} else if _, isSum := got.(sumInts); !isSum {
		t.Fatalf("lookup returned %T, want sumInts", got)
	}
	if err := ReplaceAggregate(name, doubleInts{}); err != nil {
		t.Fatalf("ReplaceAggregate over an existing registration: %v", err)
	}
	got, ok := LookupAggregate(name)
	if !ok {
		t.Fatal("the name is gone after a replacement")
	}
	if _, isDouble := got.(doubleInts); !isDouble {
		t.Fatalf("lookup returned %T after the replacement, want doubleInts", got)
	}
	rows := [][]classad.Value{{classad.NewIntValue(3)}, {classad.NewIntValue(4)}}
	if v := sumOf(t, name, rows); v != "14" {
		t.Fatalf("a query after the replacement reduced with the old implementation: got %q, want 14", v)
	}
}

// TestReplaceAggregateUnderAnotherCasingKeepsOneEntry pins both halves of the casing rule: lookups
// fold, so there is never a second entry, and the entry carries the spelling of the registration
// that is installed rather than of the one it replaced.
func TestReplaceAggregateUnderAnotherCasingKeepsOneEntry(t *testing.T) {
	name := mustReplace(t, "cas_sum", sumInts{})
	if err := ReplaceAggregate("CAS_SUM", doubleInts{}); err != nil {
		t.Fatalf("ReplaceAggregate under another casing: %v", err)
	}
	if got, ok := LookupAggregate(name); !ok {
		t.Fatal("the lowercase name stopped looking up after an uppercase replacement")
	} else if _, isDouble := got.(doubleInts); !isDouble {
		t.Fatalf("lookup returned %T, want the replacement", got)
	}
	var spellings []string
	for _, n := range RegisteredAggregates() {
		if strings.EqualFold(n, name) {
			spellings = append(spellings, n)
		}
	}
	if len(spellings) != 1 {
		t.Fatalf("want exactly one entry for %q, got %v", name, spellings)
	}
	if spellings[0] != "CAS_SUM" {
		t.Fatalf("RegisteredAggregates() reports %q; the most recent registration spelled it CAS_SUM", spellings[0])
	}
}

func TestUnregisterAggregate(t *testing.T) {
	name := mustReplace(t, "unr_sum", sumInts{})
	if !UnregisterAggregate("UNR_SUM") {
		t.Fatal("UnregisterAggregate folds case like lookup; it reported nothing to remove")
	}
	if UnregisterAggregate(name) {
		t.Fatal("UnregisterAggregate reported a removal of a name that was already gone")
	}
	if _, ok := LookupAggregate(name); ok {
		t.Fatal("an unregistered name still looks up")
	}
	for _, n := range RegisteredAggregates() {
		if strings.EqualFold(n, name) {
			t.Fatalf("RegisteredAggregates() still lists %q", n)
		}
	}
	// A query naming it gets what a name that was never registered gets.
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}
	attrs, groupCol, aggCol := AggProjection(nil, aggs)
	if _, err := AggregateValues(rowSeq(nil), attrs, nil, aggs, groupCol, aggCol, nil); !errors.Is(err, ErrNoSuchAggregate) {
		t.Fatalf("a query naming an unregistered aggregate should fail with ErrNoSuchAggregate, got %v", err)
	}
	if UnregisterAggregate("never_registered_at_all") {
		t.Fatal("UnregisterAggregate reported a removal of a name nothing ever registered")
	}
	// An unusable name is a name nothing can be under, so it is simply absent.
	if UnregisterAggregate("not a name") || UnregisterAggregate("") {
		t.Fatal("UnregisterAggregate reported a removal of a name no query could refer to")
	}
}

// TestRegisterAggregateStillRefusesADuplicateAfterReplace keeps the two doors distinct: replacing
// is the caller saying it means to overwrite, and it does not turn RegisterAggregate into an
// upsert for the name it touched.
func TestRegisterAggregateStillRefusesADuplicateAfterReplace(t *testing.T) {
	name := mustReplace(t, "repdup_sum", sumInts{})
	if err := ReplaceAggregate(name, doubleInts{}); err != nil {
		t.Fatalf("ReplaceAggregate: %v", err)
	}
	if err := RegisterAggregate(name, sumInts{}); !errors.Is(err, ErrAggregateExists) {
		t.Fatalf("RegisterAggregate should still fail with ErrAggregateExists, got %v", err)
	}
	if err := RegisterAggregate("REPDUP_SUM", sumInts{}); !errors.Is(err, ErrAggregateExists) {
		t.Fatalf("RegisterAggregate should collide across casing, got %v", err)
	}
	if got, _ := LookupAggregate(name); got == nil {
		t.Fatal("the refused registration disturbed the installed one")
	} else if _, isDouble := got.(doubleInts); !isDouble {
		t.Fatalf("the refused registration replaced the installed one: lookup returned %T", got)
	}
}

func TestReplaceAndUnregisterValidateLikeRegister(t *testing.T) {
	if err := ReplaceAggregate("repbad_sum", nil); !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate for a nil aggregate, got %v", err)
	}
	if err := ReplaceAggregate("not a name", sumInts{}); !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate for an unusable name, got %v", err)
	}
	if err := ReplaceAggregate("", sumInts{}); !errors.Is(err, ErrBadAggregate) {
		t.Fatalf("want ErrBadAggregate for an empty name, got %v", err)
	}
	if _, ok := LookupAggregate("repbad_sum"); ok {
		t.Fatal("a refused replacement registered the name anyway")
	}
}

// TestReplaceDoesNotDisturbAQueryInFlight is the guarantee the replacement API rests on. The
// aggregate is replaced from INSIDE the row sequence, after the first row and before the rest, so
// the replacement lands while the reduction is running: the query resolved the name to an
// Aggregate before it read a row and holds that value, so every row of every group reaches the
// implementation the query started with. A group split across two implementations would be a wrong
// answer indistinguishable from a right one.
//
// It groups deliberately. One group is first seen BEFORE the replacement and one AFTER it, so a
// reduction that consulted the registry when it created a group -- rather than once, up front --
// would answer the second group with the replacement and be caught here.
func TestReplaceDoesNotDisturbAQueryInFlight(t *testing.T) {
	name := mustReplace(t, "inflight_sum", sumInts{})
	rows := [][]classad.Value{
		{classad.NewStringValue("alice"), classad.NewIntValue(1)},
		{classad.NewStringValue("bob"), classad.NewIntValue(2)},
		{classad.NewStringValue("alice"), classad.NewIntValue(3)},
	}
	replaced := false
	seq := func(yield func([]classad.Value) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
			if !replaced {
				// After alice's group exists and before bob's does.
				if err := ReplaceAggregate(name, doubleInts{}); err != nil {
					t.Errorf("ReplaceAggregate mid-scan: %v", err)
				}
				replaced = true
			}
		}
	}
	groups := []GroupCol{{Attr: "Owner"}}
	aggs := []AggSpec{{Func: AggUser, UserFunc: name, Arg: "N"}}
	attrs, groupCol, aggCol := AggProjection(groups, aggs)
	out, err := AggregateValues(seq, attrs, groups, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatalf("AggregateValues: %v", err)
	}
	if !replaced {
		t.Fatal("the sequence never reached the replacement; the test proved nothing")
	}
	want := map[string]string{"alice": "4", "bob": "2"}
	if len(out) != len(want) {
		t.Fatalf("want %d groups, got %+v", len(want), out)
	}
	for _, r := range out {
		owner := strings.Trim(r.Group[0], `"`) // a string group renders as a ClassAd literal
		if got := r.Values[0]; got != want[owner] {
			t.Fatalf("group %q answered %q; a replacement mid-scan reached a query in flight (want %q)",
				owner, got, want[owner])
		}
	}
	// The NEXT query means the new thing, which is the hazard the doc comment states.
	plain := [][]classad.Value{{classad.NewIntValue(1)}, {classad.NewIntValue(3)}}
	if v := sumOf(t, name, plain); v != "8" {
		t.Fatalf("the query after the replacement got %q, want the replacement's 8", v)
	}
}

// TestRegistryWritersAndLookupsRace is the -race test for the CAS discipline. Writers on distinct
// names must not lose each other's updates, and a lookup running beside them must return a
// complete, usable aggregate -- never a half-built map and never a nil Aggregate.
func TestRegistryWritersAndLookupsRace(t *testing.T) {
	const writers = 8
	const rounds = 50
	names := make([]string, writers)
	for i := range names {
		names[i] = "race_sum_" + string(rune('a'+i))
	}
	shared := mustReplace(t, "race_shared_sum", sumInts{})

	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				agg := Aggregate(sumInts{})
				if (i+r)%2 == 1 {
					agg = doubleInts{}
				}
				if err := ReplaceAggregate(n, agg); err != nil {
					t.Errorf("ReplaceAggregate(%q): %v", n, err)
					return
				}
			}
		}(i, n)
	}
	// One name replaced under every reader, so a reader's lookup competes with a write to the
	// entry it is reading rather than only with writes elsewhere in the map.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < rounds*writers; r++ {
			agg := Aggregate(sumInts{})
			if r%2 == 1 {
				agg = doubleInts{}
			}
			if err := ReplaceAggregate(shared, agg); err != nil {
				t.Errorf("ReplaceAggregate(%q): %v", shared, err)
				return
			}
		}
	}()
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds*4; r++ {
				agg, ok := LookupAggregate(shared)
				if !ok {
					t.Errorf("LookupAggregate(%q) found nothing while it was being replaced", shared)
					return
				}
				acc := agg.NewAccumulator()
				if acc == nil {
					t.Errorf("LookupAggregate(%q) returned an aggregate with no accumulator", shared)
					return
				}
				acc.Add(classad.NewIntValue(1))
				got, err := acc.Result().IntValue()
				if err != nil || (got != 1 && got != 2) {
					t.Errorf("an aggregate taken mid-replacement did not compute: %v (%v)", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, n := range names {
		if _, ok := LookupAggregate(n); !ok {
			t.Fatalf("%q was lost: a concurrent writer dropped another writer's update", n)
		}
		if !UnregisterAggregate(n) {
			t.Fatalf("UnregisterAggregate(%q) found nothing to remove", n)
		}
	}
	if !UnregisterAggregate(shared) {
		t.Fatalf("UnregisterAggregate(%q) found nothing to remove", shared)
	}
}
