package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// semiFixture builds the shape a semi or anti join is asked about: a stream of jobs, and a
// history table where some clusters appear many times, some appear once, some not at all,
// and one job has no key to match on.
//
// fan is how many history rows a matching cluster has, which is the axis that separates a
// native existence test from the LEFT-join emulation of one: the emulation pays a pair per
// history row, this pays one per job.
func semiFixture(t testing.TB, jobs, fan int) *Catalog {
	t.Helper()
	// 512 so the correctness fixtures seal several segments: postings are built when a
	// segment seals, and a fixture that fits one open segment has the index configured and
	// no index built.
	return semiFixtureSized(t, jobs, fan, 512)
}

func semiFixtureSized(t testing.TB, jobs, fan, segment int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: segment,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: segment})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= jobs; i++ {
		ad := classad.New()
		// Every third job has no history at all, which is what an anti join is for.
		if i%3 != 0 {
			ad.InsertAttr("ClusterId", int64(i))
		} else {
			ad.InsertAttr("ClusterId", int64(-i))
		}
		ad.InsertAttrString("Owner", fmt.Sprintf("owner%d", i%5))
		if aerr := job.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// One job with no key at all. An undefined key matches nothing, so it belongs to the
	// anti join and to neither the semi join nor the inner one -- and it is in no bucket,
	// which is the row every path is most likely to lose.
	nokey := classad.New()
	nokey.InsertAttrString("Owner", "nokey")
	if aerr := job.Append(nokey); aerr != nil {
		t.Fatal(aerr)
	}
	for i := 1; i <= jobs; i++ {
		if i%3 == 0 {
			continue
		}
		for f := 0; f < fan; f++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i))
			// ExitCode 0 on the first row of each cluster, non-zero after, so a condition
			// beyond the equality can accept a cluster's later rows and reject its first.
			ad.InsertAttr("ExitCode", int64(f))
			ad.InsertAttrString("Pad", strings.Repeat("x", 64))
			if aerr := hist.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
	}
	// History for clusters no job names, so narrowing this side's read has something to
	// leave out.
	for i := jobs + 1; i <= jobs+20; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttr("ExitCode", int64(0))
		if aerr := hist.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// semiSpec is `jobs <kind> hist ON j.ClusterId == h.ClusterId`, with extra ON conjuncts
// appended so a test can make the condition reject pairs the hash admitted.
func semiSpec(kind JoinKind, extra ...string) JoinSpec {
	return JoinSpec{
		Kind:  kind,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  append([]string{"j.ClusterId == h.ClusterId"}, extra...),
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

// drainOwners collects the jobs side of each row as "owner:cluster", which is every
// attribute a semi or anti join's row can carry.
func drainOwners(t *testing.T, cat *Catalog, spec JoinSpec) ([]string, *JoinStats) {
	t.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var out []string
	for row := range seq {
		out = append(out, evalText(row, "j.Owner")+":"+evalText(row, "j.ClusterId"))
	}
	if st.Err != nil {
		t.Fatalf("join stopped early: %v", st.Err)
	}
	sort.Strings(out)
	return out, &st
}

// wantSemiAnti computes, from the fixture's own rule rather than from the join, which jobs
// have history and which do not. It is the oracle every path is checked against.
func wantSemiAnti(jobs int) (semi, anti []string) {
	for i := 1; i <= jobs; i++ {
		row := fmt.Sprintf("owner%d:%d", i%5, i)
		if i%3 != 0 {
			semi = append(semi, row)
		} else {
			anti = append(anti, fmt.Sprintf("owner%d:%d", i%5, -i))
		}
	}
	// evalText spells an absent attribute "undefined", which is what the job with no
	// ClusterId reads as -- and it is in the anti join precisely because of that.
	anti = append(anti, "nokey:undefined")
	sort.Strings(semi)
	sort.Strings(anti)
	return semi, anti
}

func TestSemiJoinKeepsEachMatchingRowExactlyOnce(t *testing.T) {
	cat := semiFixture(t, 60, 4)
	got, st := drainOwners(t, cat, semiSpec(JoinSemi))
	want, _ := wantSemiAnti(60)
	if !equalStrs(got, want) {
		t.Fatalf("semi join returned\n %v\nwant\n %v", got, want)
	}
	// Four history rows per matching cluster, and one pair examined for each: the fan-out
	// is what a semi join does NOT pay.
	if st.Pairs != len(want) {
		t.Fatalf("semi join examined %d pairs for %d rows; want one each", st.Pairs, len(want))
	}
}

func TestAntiJoinKeepsTheRowsThatMatchNothing(t *testing.T) {
	cat := semiFixture(t, 60, 4)
	got, st := drainOwners(t, cat, semiSpec(JoinAnti))
	_, want := wantSemiAnti(60)
	if !equalStrs(got, want) {
		t.Fatalf("anti join returned\n %v\nwant\n %v", got, want)
	}
	if st.Rows != len(want) {
		t.Fatalf("anti join produced %d rows, want %d", st.Rows, len(want))
	}
}

// The side a semi or anti join tests is not in the row. A caller reading it gets undefined,
// which is what SQL says and what the splice already did for an absent side.
func TestSemiAntiCarryNothingFromTheTestedSide(t *testing.T) {
	cat := semiFixture(t, 30, 3)
	for _, kind := range []JoinKind{JoinSemi, JoinAnti} {
		var st JoinStats
		seq, err := cat.Join(semiSpec(kind), JoinLimits{}, &st)
		if err != nil {
			t.Fatalf("%s: %v", joinKindName(kind), err)
		}
		n := 0
		for row := range seq {
			n++
			if v := evalText(row, "h.ExitCode"); v != "undefined" {
				t.Fatalf("%s row reads h.ExitCode as %q; the tested side is in no row, so "+
					"it must read undefined", joinKindName(kind), v)
			}
			if _, ok := row.Lookup("h"); ok {
				t.Fatalf("%s row has a member for the tested side", joinKindName(kind))
			}
		}
		if n == 0 {
			t.Fatalf("%s produced no rows, so it proved nothing", joinKindName(kind))
		}
	}
}

// The condition decides, not the bucket. A cluster whose every history row fails the ON
// condition has not matched, so it belongs to the anti join -- and a hash that stopped at
// "the bucket is not empty" would put it in the semi join instead.
func TestSemiAntiObeyTheWholeConditionNotJustTheHash(t *testing.T) {
	cat := semiFixture(t, 30, 1)
	// fan 1 means every cluster's only history row has ExitCode 0, so this conjunct is
	// false for every pair the hash offers and the semi join must come back empty.
	semi, _ := drainOwners(t, cat, semiSpec(JoinSemi, "h.ExitCode > 0"))
	if len(semi) != 0 {
		t.Fatalf("semi join returned %d rows; no pair satisfies the condition", len(semi))
	}
	anti, _ := drainOwners(t, cat, semiSpec(JoinAnti, "h.ExitCode > 0"))
	if len(anti) != 31 {
		t.Fatalf("anti join returned %d rows, want all 31", len(anti))
	}
}

// Every path has to answer the same. The hash, the index probe and the batched outer-driven
// path reach the same pair loop by different routes, and the one thing a new join kind can
// break is a row a narrowed read never offered it.
func TestSemiAntiAnswerTheSameOnEveryPath(t *testing.T) {
	// Big enough that the hashed side's read is over minProbeRows. Below it the probe
	// declines on size whatever the kind, and this test would compare the hash with
	// itself three times and pass.
	const jobs = 1200
	cat := semiFixture(t, jobs, 3)
	wantSemi, wantAnti := wantSemiAnti(jobs)

	paths := []struct {
		name string
		with func(func())
		// want reports whether the stats say this path actually ran.
		ran func(*JoinStats) bool
	}{
		{"hash", func(fn func()) {
			withoutOuterDriven(func() { withoutProbe(fn) })
		}, func(st *JoinStats) bool { return st.OuterDrivenBatches == 0 && st.ProbeKeys == 0 }},
		{"probe", func(fn func()) {
			withoutOuterDriven(func() { forcingProbe(fn) })
		}, func(st *JoinStats) bool { return st.OuterDrivenBatches == 0 && st.ProbeKeys > 0 }},
		{"batched", func(fn func()) {
			withOuterDriven(fn)
		}, func(st *JoinStats) bool { return st.OuterDrivenBatches > 0 }},
	}
	for _, p := range paths {
		for _, c := range []struct {
			kind JoinKind
			want []string
		}{{JoinSemi, wantSemi}, {JoinAnti, wantAnti}} {
			p.with(func() {
				got, st := drainOwners(t, cat, semiSpec(c.kind))
				if !p.ran(st) {
					t.Fatalf("%s/%s: wanted the %s path, got batches=%d probeKeys=%d",
						p.name, joinKindName(c.kind), p.name,
						st.OuterDrivenBatches, st.ProbeKeys)
				}
				if !equalStrs(got, c.want) {
					t.Fatalf("%s/%s returned %d rows, want %d\n got %v\nwant %v",
						p.name, joinKindName(c.kind), len(got), len(c.want), got, c.want)
				}
			})
		}
	}
}

// An anti join answers what the LEFT-join emulation of one answers, and examines far fewer
// pairs doing it. The emulation is what callers write today, so this is the comparison that
// says the new kind may replace it.
func TestAntiJoinAgreesWithTheLeftJoinEmulation(t *testing.T) {
	const jobs, fan = 120, 8
	cat := semiFixture(t, jobs, fan)

	emul := semiSpec(JoinLeft)
	emul.Post = []string{"h.ClusterId =?= undefined"}
	emulRows, emulStats := drainOwners(t, cat, emul)

	native, nativeStats := drainOwners(t, cat, semiSpec(JoinAnti))
	if !equalStrs(native, emulRows) {
		t.Fatalf("anti join and its LEFT-join emulation disagree:\n native %v\n emul  %v",
			native, emulRows)
	}
	// The emulation forms one pair per history row of every matching cluster; the anti
	// join forms one per job and stops.
	if nativeStats.Pairs >= emulStats.Pairs {
		t.Fatalf("anti join examined %d pairs, emulation %d; the native kind must examine "+
			"fewer", nativeStats.Pairs, emulStats.Pairs)
	}
	matching := 0
	for i := 1; i <= jobs; i++ {
		if i%3 != 0 {
			matching++
		}
	}
	if emulStats.Pairs != matching*fan {
		t.Fatalf("emulation examined %d pairs, want %d (one per history row)",
			emulStats.Pairs, matching*fan)
	}
	if nativeStats.Pairs != matching {
		t.Fatalf("anti join examined %d pairs, want %d (one per matching job)",
			nativeStats.Pairs, matching)
	}
}

// A semi or anti side is read for its condition alone. An anti join against a wide table
// should not carry its ads.
func TestSemiAntiReadTheTestedSideForItsConditionOnly(t *testing.T) {
	spec := semiSpec(JoinAnti)
	read := planJoinRead(spec)
	if len(read) != 2 {
		t.Fatalf("planJoinRead returned %d sides, want 2", len(read))
	}
	if read[1] == nil {
		t.Fatal("the tested side is read whole; it contributes nothing and needs only its key")
	}
	if !equalStrs(read[1], []string{"ClusterId"}) {
		t.Fatalf("the tested side is read as %v, want just the key", read[1])
	}
	if read[0] != nil {
		t.Fatalf("the FROM side is read as %v, want whole -- its Project is nil", read[0])
	}
}

// Nothing downstream may read a side that is in no row. Each of these would otherwise read
// UNDEFINED and quietly answer a smaller query.
func TestSemiAntiRefuseAReferenceToTheTestedSide(t *testing.T) {
	cat := semiFixture(t, 10, 1)
	cases := []struct {
		name string
		spec func() JoinSpec
		want string
	}{
		{"WHERE", func() JoinSpec {
			s := semiSpec(JoinAnti)
			s.Post = []string{"h.ExitCode == 0"}
			return s
		}, "are in no row"},
		{"projection", func() JoinSpec {
			s := semiSpec(JoinSemi)
			s.Right.Project = []string{"ExitCode"}
			return s
		}, "may not be projected"},
		{"later ON", func() JoinSpec {
			return JoinSpec{
				Left: JoinSide{Table: "jobs", Alias: "j"},
				Steps: []JoinStep{
					{Kind: JoinSemi, Side: JoinSide{Table: "hist", Alias: "h"},
						Cond: []string{"j.ClusterId == h.ClusterId"},
						Equi: []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}},
					{Kind: JoinInner, Side: JoinSide{Table: "jobs", Alias: "k"},
						Cond: []string{"h.ClusterId == k.ClusterId"},
						Equi: []JoinEqui{{Left: "h.ClusterId", Right: "ClusterId"}}},
				},
			}
		}, "are in no row"},
	}
	for _, c := range cases {
		var st JoinStats
		_, err := cat.Join(c.spec(), JoinLimits{}, &st)
		if err == nil {
			t.Fatalf("%s: the spec was accepted; it reads a side that is in no row", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want it to say %q", c.name, err, c.want)
		}
	}
}

// A semi step in the middle of a chain tests its side and the chain carries on without it.
func TestSemiStepInAChainFoldsInNothing(t *testing.T) {
	cat := semiFixture(t, 30, 2)
	spec := JoinSpec{
		Left: JoinSide{Table: "jobs", Alias: "j"},
		Steps: []JoinStep{
			{Kind: JoinSemi, Side: JoinSide{Table: "hist", Alias: "h"},
				Cond: []string{"j.ClusterId == h.ClusterId"},
				Equi: []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}},
			{Kind: JoinInner, Side: JoinSide{Table: "jobs", Alias: "k"},
				Cond: []string{"j.ClusterId == k.ClusterId"},
				Equi: []JoinEqui{{Left: "j.ClusterId", Right: "ClusterId"}}},
		},
	}
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for row := range seq {
		if _, ok := row.Lookup("h"); ok {
			t.Fatal("the semi step folded its side into the row")
		}
		got = append(got, evalText(row, "k.Owner")+":"+evalText(row, "k.ClusterId"))
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	sort.Strings(got)
	want, _ := wantSemiAnti(30)
	if !equalStrs(got, want) {
		t.Fatalf("chain returned\n %v\nwant\n %v", got, want)
	}
}

func TestExplainNamesTheSemiAndAntiKinds(t *testing.T) {
	cat := semiFixture(t, 10, 1)
	for kind, want := range map[JoinKind]string{JoinSemi: "semi", JoinAnti: "anti"} {
		ex, err := cat.ExplainJoin(semiSpec(kind), JoinLimits{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ex.Steps) != 1 || ex.Steps[0].Kind != want {
			t.Fatalf("explain reported %+v, want one %s step", ex.Steps, want)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The parallel walk gives each walker its own accumulated row and its own share of the
// stream. A semi or anti step's decision is per FROM row and so needs nothing shared, but
// "needs nothing shared" is the claim every data race starts as.
func TestSemiAntiAgreeSerialAndParallel(t *testing.T) {
	const jobs = 1200
	cat := semiFixture(t, jobs, 3)
	wantSemi, wantAnti := wantSemiAnti(jobs)
	for _, c := range []struct {
		kind JoinKind
		want []string
	}{{JoinSemi, wantSemi}, {JoinAnti, wantAnti}} {
		forceSerialJoinWalk = true
		serial, _ := drainOwners(t, cat, semiSpec(c.kind))
		forceSerialJoinWalk = false
		if !equalStrs(serial, c.want) {
			t.Fatalf("serial %s returned %d rows, want %d", joinKindName(c.kind),
				len(serial), len(c.want))
		}
		before := joinParallelWalks.Load()
		par, _ := drainOwners(t, cat, semiSpec(c.kind))
		if joinParallelWalks.Load() == before {
			t.Skipf("the walk did not go parallel on this machine; nothing was compared")
		}
		if !equalStrs(par, c.want) {
			t.Fatalf("parallel %s returned\n %v\nwant\n %v", joinKindName(c.kind), par, c.want)
		}
	}
}

// A semi or anti step with no equality holds its side whole and tests every row against the
// condition. The existence break still applies, which is the only thing that keeps such a
// step off the pair ceiling.
func TestSemiAntiWithoutAnEqualityStillStopAtTheFirstMatch(t *testing.T) {
	cat := semiFixture(t, 30, 4)
	spec := JoinSpec{
		Kind:  JoinSemi,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
	}
	got, st := drainOwners(t, cat, spec)
	want, _ := wantSemiAnti(30)
	if !equalStrs(got, want) {
		t.Fatalf("un-hashed semi join returned\n %v\nwant\n %v", got, want)
	}
	// 31 jobs against 100 history rows is 3,100 pairs without the break. The matching
	// jobs stop early; the rest still scan the side whole, because nothing has told them
	// a match is impossible.
	if st.Pairs >= 31*100 {
		t.Fatalf("un-hashed semi join examined %d pairs; the break is not firing", st.Pairs)
	}
}

// The pair budget is the difference that is not a percentage. MaxPairs bounds candidate
// pairs EXAMINED, and the emulation's are the matching side's fan-out while this kind's are
// one per row -- so there are joins the emulation cannot run at all and this one can.
func TestAntiJoinRunsUnderAPairBudgetTheEmulationExceeds(t *testing.T) {
	// Small on purpose: what this asserts is that the two budgets differ, and the fixture
	// only has to be big enough for the emulation's fan-out to cross a budget the native
	// kind stays under. Under -race the fixture build is most of the cost.
	const jobs, fan = 300, 8
	cat := semiFixture(t, jobs, fan)
	limits := JoinLimits{MaxPairs: 500}

	emul := semiSpec(JoinLeft)
	emul.Post = []string{"h.ClusterId =?= undefined"}
	var es JoinStats
	seq, err := cat.Join(emul, limits, &es)
	if err != nil {
		t.Fatal(err)
	}
	for range seq {
	}
	if es.Err == nil {
		t.Fatalf("the emulation finished within %d pairs (it examined %d); this test no "+
			"longer separates the two", limits.MaxPairs, es.Pairs)
	}
	if !strings.Contains(es.Err.Error(), "candidate pairs") {
		t.Fatalf("the emulation stopped for the wrong reason: %v", es.Err)
	}

	got, ns := drainOwners(t, cat, semiSpec(JoinAnti))
	_, want := wantSemiAnti(jobs)
	if !equalStrs(got, want) {
		t.Fatalf("anti join under the same budget returned %d rows, want %d", len(got), len(want))
	}
	if ns.Pairs > limits.MaxPairs {
		t.Fatalf("anti join examined %d pairs, over the %d the emulation failed on",
			ns.Pairs, limits.MaxPairs)
	}
}
