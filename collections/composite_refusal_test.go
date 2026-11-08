package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A configured composite tuple the index configuration cannot resolve is dropped, and the
// store is built anyway. These tests cover what the drop is now expected to SAY, and --
// first and most importantly -- that it still drops exactly the same tuples it did before
// anything said anything.

// refusalCase is one index configuration and the composites it is expected to leave
// installed. The accepted column is the differential: it was captured from the behavior
// before refusals were reported at all, so a change to it is a change to what a composite
// configuration MEANS, not to what it says about itself.
type refusalCase struct {
	name    string
	cat     []string // CategoricalAttrs
	val     []string // ValueAttrs
	catComp []string // CategoricalComponentAttrs
	valComp []string // ValueComponentAttrs
	tuples  [][]string
	// accepted is the set of composites the configuration installs, compared as a SET.
	// Tuple-identity order is by encoded attribute id, and the two id spaces (interned for
	// an in-memory collection, inline for a persistent one) number the same attributes
	// differently, so the ORDER a tier reports is not a property of the configuration.
	accepted [][]string
	// refused is what the configuration is expected to SAY about the tuples it dropped,
	// in configuration order (which is the order they were refused in). Compared field for
	// field: a refusal that named the wrong attribute, or the wrong reason for the right
	// attribute, would be worse than the silence it replaces.
	refused []CompositeRefusal
}

func refusalCases() []refusalCase {
	wide := []string{"W1", "W2", "W3", "W4", "W5", "W6", "W7", "W8", "W9"}
	return []refusalCase{{
		name:     "every-component-indexed",
		cat:      []string{"Owner"},
		val:      []string{"ClusterId", "ProcId"},
		tuples:   [][]string{{"ClusterId", "ProcId"}},
		accepted: [][]string{{"ClusterId", "ProcId"}},
	}, {
		name:     "component-only-component",
		val:      []string{"ClusterId"},
		valComp:  []string{"ProcId"},
		tuples:   [][]string{{"ClusterId", "ProcId"}},
		accepted: [][]string{{"ClusterId", "ProcId"}},
	}, {
		name:   "unresolvable-component",
		val:    []string{"ClusterId"},
		tuples: [][]string{{"ClusterId", "ProcId"}},
		refused: []CompositeRefusal{{
			Attrs: []string{"ClusterId", "ProcId"}, Reason: CompositeRefusedUnresolvableComponent, Attr: "ProcId",
		}},
	}, {
		name:   "duplicate-component",
		val:    []string{"ClusterId", "ProcId"},
		tuples: [][]string{{"ClusterId", "ClusterId"}},
		refused: []CompositeRefusal{{
			Attrs: []string{"ClusterId", "ClusterId"}, Reason: CompositeRefusedDuplicateComponent, Attr: "ClusterId",
		}},
	}, {
		// The repeated component is not the leading one, so a refusal that named the
		// tuple's first attribute instead of the offending one would be visible here and
		// nowhere else.
		name:   "duplicate-component-after-another",
		cat:    []string{"Owner"},
		val:    []string{"ClusterId"},
		tuples: [][]string{{"Owner", "ClusterId", "ClusterId"}},
		refused: []CompositeRefusal{{
			Attrs: []string{"Owner", "ClusterId", "ClusterId"}, Reason: CompositeRefusedDuplicateComponent, Attr: "ClusterId",
		}},
	}, {
		name:   "one-component",
		val:    []string{"ClusterId", "ProcId"},
		tuples: [][]string{{"ClusterId"}},
		refused: []CompositeRefusal{{
			Attrs: []string{"ClusterId"}, Reason: CompositeRefusedTooFewComponents,
		}},
	}, {
		name:   "no-components",
		val:    []string{"ClusterId", "ProcId"},
		tuples: [][]string{{}},
		refused: []CompositeRefusal{{
			Reason: CompositeRefusedTooFewComponents,
		}},
	}, {
		name:   "wider-than-the-cap",
		val:    wide,
		tuples: [][]string{wide},
		refused: []CompositeRefusal{{
			Attrs: wide, Reason: CompositeRefusedTooManyComponents,
		}},
	}, {
		name:     "the-cap-exactly",
		val:      wide[:maxCompositeAttrs],
		tuples:   [][]string{wide[:maxCompositeAttrs]},
		accepted: [][]string{wide[:maxCompositeAttrs]},
	}, {
		name:     "tuple-configured-twice",
		val:      []string{"ClusterId", "ProcId"},
		tuples:   [][]string{{"ClusterId", "ProcId"}, {"ClusterId", "ProcId"}},
		accepted: [][]string{{"ClusterId", "ProcId"}},
		refused: []CompositeRefusal{{
			Attrs: []string{"ClusterId", "ProcId"}, Reason: CompositeRefusedDuplicateTuple,
		}},
	}, {
		name:     "the-same-components-reversed",
		val:      []string{"ClusterId", "ProcId"},
		tuples:   [][]string{{"ClusterId", "ProcId"}, {"ProcId", "ClusterId"}},
		accepted: [][]string{{"ClusterId", "ProcId"}, {"ProcId", "ClusterId"}},
	}, {
		name:     "a-bad-tuple-beside-good-ones",
		cat:      []string{"Owner"},
		val:      []string{"ClusterId", "ProcId"},
		catComp:  []string{"Cmd"},
		tuples:   [][]string{{"ClusterId", "ProcId"}, {"ClusterId", "Nonesuch"}, {"Owner", "Cmd"}},
		accepted: [][]string{{"ClusterId", "ProcId"}, {"Owner", "Cmd"}},
		refused: []CompositeRefusal{{
			Attrs: []string{"ClusterId", "Nonesuch"}, Reason: CompositeRefusedUnresolvableComponent, Attr: "Nonesuch",
		}},
	}, {
		name:     "component-names-fold",
		val:      []string{"ClusterId", "ProcId"},
		tuples:   [][]string{{"clusterid", "PROCID"}},
		accepted: [][]string{{"clusterid", "PROCID"}},
	}}
}

func (rc refusalCase) options() Options {
	return Options{
		Shards:                    2,
		CategoricalAttrs:          rc.cat,
		ValueAttrs:                rc.val,
		CategoricalComponentAttrs: rc.catComp,
		ValueComponentAttrs:       rc.valComp,
		CompositeAttrs:            rc.tuples,
	}
}

func (rc refusalCase) archiveOptions(dir string) ArchiveOptions {
	return ArchiveOptions{
		Dir:                       dir,
		CategoricalAttrs:          rc.cat,
		ValueAttrs:                rc.val,
		CategoricalComponentAttrs: rc.catComp,
		ValueComponentAttrs:       rc.valComp,
		CompositeAttrs:            rc.tuples,
	}
}

// tupleSet renders tuples as a comparable, order-independent set.
func tupleSet(tuples [][]string) []string {
	out := make([]string, 0, len(tuples))
	for _, t := range tuples {
		out = append(out, strings.Join(t, "+"))
	}
	sort.Strings(out)
	return out
}

func checkAccepted(t *testing.T, rc refusalCase, got [][]string) {
	t.Helper()
	want, have := tupleSet(rc.accepted), tupleSet(got)
	if fmt.Sprint(want) != fmt.Sprint(have) {
		t.Fatalf("composites installed = %v, want %v", have, want)
	}
}

// TestCompositeAcceptanceIsUnchanged pins the set of composites every configuration in
// refusalCases installs, in all three places a configuration is resolved: an in-memory
// collection (interned ids), a persistent one (inline ids), and an archive. Reporting a
// refusal must not change a single entry here.
func TestCompositeAcceptanceIsUnchanged(t *testing.T) {
	for _, rc := range refusalCases() {
		t.Run(rc.name+"/in-memory", func(t *testing.T) {
			c := New(rc.options())
			defer c.Close()
			checkAccepted(t, rc, c.CompositeAttrs())
		})
		t.Run(rc.name+"/persistent", func(t *testing.T) {
			opts := rc.options()
			opts.Dir = t.TempDir()
			c, err := Open(opts)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer c.Close()
			checkAccepted(t, rc, c.CompositeAttrs())
		})
		t.Run(rc.name+"/archive", func(t *testing.T) {
			a, err := CreateArchive(rc.archiveOptions(t.TempDir()))
			if err != nil {
				t.Fatalf("CreateArchive: %v", err)
			}
			defer a.Close()
			checkAccepted(t, rc, a.CompositeAttrs())
		})
	}
}

// refusalKey renders a refusal for comparison. %q on a nil []string and on an empty one
// print alike, which is what keeps a tuple configured as `{}` from depending on whether the
// refusal copied it as nil or as a zero-length slice.
func refusalKey(r CompositeRefusal) string {
	return fmt.Sprintf("%q|%s|%s", r.Attrs, r.Reason, r.Attr)
}

func refusalKeys(rs []CompositeRefusal) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, refusalKey(r))
	}
	return out
}

func checkRefused(t *testing.T, rc refusalCase, got []CompositeRefusal) {
	t.Helper()
	want, have := refusalKeys(rc.refused), refusalKeys(got)
	if fmt.Sprint(want) != fmt.Sprint(have) {
		t.Fatalf("refusals = %v, want %v", have, want)
	}
}

// TestCompositeRefusalNamesTheReason is the point of the whole change: every tuple the
// configuration drops is reported, with the reason, and with the component at fault when
// the reason has one -- identically in all three places a configuration is resolved.
//
// The cases that expect NO refusal are not on their own evidence of anything (a method
// that always returned nil would pass them), so they only mean something beside the cases
// that expect one. TestCompositeRefusalsAreReportableAtAll makes that pairing explicit.
func TestCompositeRefusalNamesTheReason(t *testing.T) {
	for _, rc := range refusalCases() {
		t.Run(rc.name+"/in-memory", func(t *testing.T) {
			c := New(rc.options())
			defer c.Close()
			checkRefused(t, rc, c.CompositeRefusals())
		})
		t.Run(rc.name+"/persistent", func(t *testing.T) {
			opts := rc.options()
			opts.Dir = t.TempDir()
			c, err := Open(opts)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer c.Close()
			checkRefused(t, rc, c.CompositeRefusals())
		})
		t.Run(rc.name+"/archive", func(t *testing.T) {
			a, err := CreateArchive(rc.archiveOptions(t.TempDir()))
			if err != nil {
				t.Fatalf("CreateArchive: %v", err)
			}
			defer a.Close()
			checkRefused(t, rc, a.CompositeRefusals())
		})
	}
}

// TestCompositeRefusalsAreReportableAtAll proves, in each tier, that a refusal CAN come
// back before anything is read into one not coming back. Without this pair the "a good
// configuration refuses nothing" half of the table above is satisfied by an accessor that
// reports nothing ever.
func TestCompositeRefusalsAreReportableAtAll(t *testing.T) {
	good := refusalCase{val: []string{"ClusterId", "ProcId"}, tuples: [][]string{{"ClusterId", "ProcId"}}}
	bad := refusalCase{val: []string{"ClusterId"}, tuples: [][]string{{"ClusterId", "ProcId"}}}

	tiers := map[string]func(*testing.T, refusalCase) []CompositeRefusal{
		"in-memory": func(t *testing.T, rc refusalCase) []CompositeRefusal {
			c := New(rc.options())
			defer c.Close()
			return c.CompositeRefusals()
		},
		"persistent": func(t *testing.T, rc refusalCase) []CompositeRefusal {
			opts := rc.options()
			opts.Dir = t.TempDir()
			c, err := Open(opts)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer c.Close()
			return c.CompositeRefusals()
		},
		"archive": func(t *testing.T, rc refusalCase) []CompositeRefusal {
			a, err := CreateArchive(rc.archiveOptions(t.TempDir()))
			if err != nil {
				t.Fatalf("CreateArchive: %v", err)
			}
			defer a.Close()
			return a.CompositeRefusals()
		},
	}
	for name, open := range tiers {
		t.Run(name, func(t *testing.T) {
			// The precondition first: this tier reports refusals at all.
			if got := open(t, bad); len(got) != 1 {
				t.Fatalf("a configuration naming an unindexed component reported %d refusals, want 1 -- the negative half of this test would be vacuous", len(got))
			}
			if got := open(t, good); len(got) != 0 {
				t.Fatalf("a configuration that resolves reported %v, want none", got)
			}
		})
	}
}

// TestCompositeRefusalString locks the one-line log rendering, including that it names the
// component when there is one and does not invent one when there is not.
func TestCompositeRefusalString(t *testing.T) {
	for _, tc := range []struct {
		r    CompositeRefusal
		want string
	}{{
		CompositeRefusal{Attrs: []string{"ClusterId", "ProcId"}, Reason: CompositeRefusedUnresolvableComponent, Attr: "ProcId"},
		"ClusterId+ProcId: unresolvable-component ProcId",
	}, {
		CompositeRefusal{Attrs: []string{"ClusterId", "ProcId"}, Reason: CompositeRefusedDuplicateTuple},
		"ClusterId+ProcId: duplicate-tuple",
	}} {
		if got := tc.r.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// composedArchiveOptions is the configuration of an archive whose (ClusterId, ProcId)
// composite resolves, and -- with indexProc false -- the same configuration after ProcId's
// index has been taken away, which is what a later DropIndex or an edited config leaves.
func composedArchiveOptions(dir string, indexProc bool) ArchiveOptions {
	opts := ArchiveOptions{
		Dir:            dir,
		SegmentSize:    8 << 10,
		ValueAttrs:     []string{"ClusterId"},
		CompositeAttrs: [][]string{{"ClusterId", "ProcId"}},
	}
	if indexProc {
		opts.ValueAttrs = append(opts.ValueAttrs, "ProcId")
	}
	return opts
}

func appendJobs(t *testing.T, a *Archive, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ID=%d; ClusterId=%d; ProcId=%d ]`, i, i/4, i%4))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

// TestArchiveWithAnUnresolvableCompositeStillOpens is the constraint the diagnostic had to
// be built around. An archive created when every component resolved carries that tuple in
// the configuration it is reopened with; if the component's index is later taken out of
// that configuration, the tuple stops resolving. Reopening must still work -- a store that
// refuses to open its own data because an index went missing is a far worse outcome than the
// missing index -- and the records must still read back.
func TestArchiveWithAnUnresolvableCompositeStillOpens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(composedArchiveOptions(dir, true))
	if err != nil {
		t.Fatalf("CreateArchive: %v", err)
	}
	// The precondition: the composite was really there to lose.
	if got := a.CompositeAttrs(); len(got) != 1 {
		t.Fatalf("composites at creation = %v, want one -- nothing would be lost by the reopen below", got)
	}
	if got := a.CompositeRefusals(); len(got) != 0 {
		t.Fatalf("refusals at creation = %v, want none", got)
	}
	appendJobs(t, a, 64)
	// err is the CreateArchive error above; reuse it rather than shadowing it.
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := OpenArchive(composedArchiveOptions(dir, false))
	if err != nil {
		t.Fatalf("reopen with an unresolvable composite component: %v", err)
	}
	defer b.Close()
	if got := b.CompositeAttrs(); len(got) != 0 {
		t.Fatalf("composites after reopen = %v, want none", got)
	}
	ref := b.CompositeRefusals()
	if len(ref) != 1 || ref[0].Reason != CompositeRefusedUnresolvableComponent || ref[0].Attr != "ProcId" {
		t.Fatalf("refusals after reopen = %v, want one unresolvable-component naming ProcId", ref)
	}
	// The store is not merely open, it answers: the composite is gone, the query it would
	// have accelerated still returns the same rows by scanning.
	if got := archiveQueryIDs(t, b, `ClusterId == 3 && ProcId == 2`); len(got) != 1 || got[0] != 14 {
		t.Fatalf("query after reopen = %v, want [14]", got)
	}
}

// TestCompositeRefusalsDoNotAccumulateAcrossReopens: a configuration is re-applied on every
// open, so a diagnostic that appended would report the same refused tuple once per restart
// and grow forever on a store that is never reconfigured.
func TestCompositeRefusalsDoNotAccumulateAcrossReopens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(composedArchiveOptions(dir, true))
	if err != nil {
		t.Fatalf("CreateArchive: %v", err)
	}
	appendJobs(t, a, 16)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		b, err := OpenArchive(composedArchiveOptions(dir, false))
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		got := b.CompositeRefusals()
		if len(got) != 1 {
			t.Fatalf("reopen %d: refusals = %v (%d), want exactly one", i, got, len(got))
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOpenIndexDiagCarriesCompositeRefusals: the production surface. An operator does not
// call CompositeRefusals on a store they cannot reach -- they read the open-time diagnostic
// their process already logs, and the refusal has to be in it.
func TestOpenIndexDiagCarriesCompositeRefusals(t *testing.T) {
	var got OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { got = d }
	defer func() { OpenIndexDiagHook = prev }()

	dir := t.TempDir()
	a, err := CreateArchive(composedArchiveOptions(dir, true))
	if err != nil {
		t.Fatalf("CreateArchive: %v", err)
	}
	// The precondition: a resolvable configuration puts nothing in the diagnostic, so the
	// entry seen after the reopen below came from the refusal and not from the open path.
	if len(got.CompositeRefusals) != 0 {
		t.Fatalf("diag at creation = %v, want no refusals", got.CompositeRefusals)
	}
	appendJobs(t, a, 16)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := OpenArchive(composedArchiveOptions(dir, false))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b.Close()
	if n := len(got.CompositeRefusals); n != 1 {
		t.Fatalf("diag refusals = %v (%d), want one", got.CompositeRefusals, n)
	}
	if s := got.CompositeRefusals[0].String(); s != "ClusterId+ProcId: unresolvable-component ProcId" {
		t.Fatalf("diag refusal = %q", s)
	}
}

// TestAutoCompositeRefusalsAreNotReported: the tuner's proposals are not configuration.
// AddAutoComposites refuses a tuple whose component is not indexed, and that refusal is the
// rule working -- there is nothing for an operator to fix, and the tuner runs on a timer, so
// recording them is the unbounded-growth shape this diagnostic exists to avoid.
func TestAutoCompositeRefusalsAreNotReported(t *testing.T) {
	c := New(Options{Shards: 2, ValueAttrs: []string{"ClusterId"}})
	defer c.Close()
	if got := c.CompositeRefusals(); len(got) != 0 {
		t.Fatalf("refusals before any tuner call = %v", got)
	}
	// The precondition: this tuple really is refused -- ProcId has no index and no
	// component-only declaration, so nothing is installed.
	if c.AddAutoComposites([][]string{{"ClusterId", "ProcId"}}) {
		t.Fatalf("AddAutoComposites installed a composite over an unindexed component")
	}
	if got := c.CompositeRefusals(); len(got) != 0 {
		t.Fatalf("a refused tuner proposal was reported as a configuration refusal: %v", got)
	}
}
