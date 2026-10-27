package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// wireExprAdTexts exercise the part of the relay an in-memory collection makes load-bearing:
// an interned record spells attribute references, function names and selectors as INTERNED
// IDS inside the expression nodes, so a row assembled from one has to rewrite the node
// grammar, not just the attribute keys. Every name-carrying node tag is represented here
// (nAttrRef with both scopes, nFunc, nSelect, and a nested record whose own keys are interned),
// alongside the literal and operator tags that must pass through byte-identical.
var wireExprAdTexts = []string{
	`[MyType = "Machine"; TargetType = "Job"; Name = "slot1@x"; State = "Unclaimed";
	  Cpus = 8; Memory = 16384; LoadAvg = 0.25; Neg = -3; Big = 123456789012;
	  ClaimId = "<secret>";
	  Start = (LoadAvg < 0.5) && (KeyboardIdle > 900); KeyboardIdle = 1200;
	  Requirements = MY.Start && (TARGET.RequestCpus <= Cpus);
	  Joined = strcat(Name, "-", State);
	  Nested = [Inner = Cpus * 2; Deeper = [Leaf = Name]];
	  Picked = Nested.Inner;
	  Listy = {1, 2.5, "three", Cpus, undefined, Nested.Inner};
	  Cond = Cpus > 4 ? "big" : "small";
	  Negated = !(Cpus > 4); Parens = (Cpus + 1) * 2;
	  Sub = Listy[2]; Err = error; Undef = undefined; Yes = true; No = false]`,
	`[MyType = "Machine"; TargetType = "Job"; Name = "slot1@y"; State = "Claimed";
	  Cpus = 4; Memory = 8192;
	  Start = SuspendReason isnt undefined; SuspendReason = undefined;
	  Requirements = TARGET.RequestMemory < Memory;
	  Joined = toUpper(State)]`,
}

// wireExprCollection is the IN-MEMORY (interned) fixture for the relay tests. It asserts its
// own storage mode: a test that silently ran against an inline collection would prove nothing
// about the path it is here to cover.
func wireExprCollection(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{})
	t.Cleanup(func() { c.Close() })
	if c.inline {
		t.Fatal("fixture is not an in-memory collection: records are inline-encoded")
	}
	for i, text := range wireExprAdTexts {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatalf("ad %d: %v", i, err)
		}
		if err := c.Put([]byte{byte('a' + i)}, ad); err != nil {
			t.Fatal(err)
		}
	}
	// And the records really are interned: the encoder the write path uses must produce a
	// record WITHOUT flagInlineNames, or the transcode under test never runs.
	probe, err := classad.Parse(wireExprAdTexts[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, inline, ok := wire.Ad(c.encodeAd(probe.AST())).SplitBody(); !ok || inline {
		t.Fatalf("stored records are inline (ok=%v): the fixture is not interned", ok)
	}
	return c
}

// TestScanRawWireInMemory is the point of serving wire rows from a RAM table: the relay rows
// an in-memory collection produces must render to EXACTLY what its own text scan produces,
// expression-valued attributes included -- and must not be empty, which is the failure mode
// that looks like success (a scan that yields nothing is indistinguishable from a query that
// matched nothing).
func TestScanRawWireInMemory(t *testing.T) {
	t.Parallel()
	c := wireExprCollection(t)

	if !c.SupportsRawWire() {
		t.Fatal("SupportsRawWire() = false for an in-memory collection")
	}

	cases := []struct {
		name   string
		proj   []string
		redact bool
	}{
		{"whole-ad", nil, false},
		{"whole-ad-redacted", nil, true},
		{"projected-expressions", []string{"Name", "Requirements", "Start", "Joined", "Picked",
			"Listy", "Cond", "Negated", "Parens", "Sub", "Nested", "Neg", "Big", "LoadAvg",
			"Err", "Undef", "Yes", "No", "NoSuchAttr"}, false},
		{"projected-redacted", []string{"Name", "ClaimId", "Requirements"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := adsByName(t, collectProjected(c, tc.proj, false, tc.redact))
			got := adsByName(t, collectWireRendered(t, c, tc.proj, tc.redact))
			if len(got) == 0 {
				t.Fatal("wire relay returned no rows at all for an in-memory collection")
			}
			if len(got) != len(want) {
				t.Fatalf("wire relay returned %d ads, direct scan %d", len(got), len(want))
			}
			for name, attrs := range want {
				g, ok := got[name]
				if !ok {
					t.Errorf("%s: missing from the wire relay", name)
					continue
				}
				if len(g) != len(attrs) {
					t.Errorf("%s: wire %v != direct %v", name, g, attrs)
					continue
				}
				for a, v := range attrs {
					if g[a] != v {
						t.Errorf("%s.%s: wire %q != direct %q", name, a, g[a], v)
					}
				}
			}
		})
	}

	// A redacted row must not carry the private value's bytes even before rendering.
	rows := 0
	for row := range c.ScanRawWire(nil, true) {
		rows++
		if strings.Contains(string(row), "<secret>") {
			t.Fatal("redacted wire row carries the private value bytes")
		}
	}
	if rows != len(wireExprAdTexts) {
		t.Fatalf("redacted whole-ad scan yielded %d rows, want %d", rows, len(wireExprAdTexts))
	}

	// An unredacted whole-ad row DOES carry it: otherwise the check above would pass for the
	// trivial reason that the relay ships nothing.
	found := false
	for row := range c.ScanRawWire(nil, false) {
		if strings.Contains(string(row), "<secret>") {
			found = true
		}
	}
	if !found {
		t.Fatal("an entitled whole-ad relay row dropped the private attribute")
	}
}

// TestQueryRawWireInMemory covers the constrained relay scan on a RAM table, including the
// expression attributes the constraint does not name.
func TestQueryRawWireInMemory(t *testing.T) {
	t.Parallel()
	c := wireExprCollection(t)

	q, err := vm.Parse(`State == "Claimed"`)
	if err != nil {
		t.Fatal(err)
	}
	proj := []string{"Name", "Requirements", "Joined"}

	var wantAds []RawAd
	for ra := range c.QueryRawProjected(q, proj, false, false) {
		cp := RawAd{MyType: ra.MyType, TargetType: ra.TargetType}
		for _, e := range ra.Exprs {
			cp.Exprs = append(cp.Exprs, append([]byte(nil), e...))
		}
		wantAds = append(wantAds, cp)
	}
	want := adsByName(t, wantAds)
	if len(want) != 1 {
		t.Fatalf("text query matched %d ads, want 1", len(want))
	}

	var gotAds []RawAd
	var buf []byte
	var offs []int
	for row := range c.QueryRawWire(q, proj, false) {
		var mt, tt string
		var ok bool
		buf, offs, mt, tt, ok = RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("RenderRawAdInline failed on a shipped row")
		}
		cp := RawAd{MyType: mt, TargetType: tt}
		for i := 0; i+1 < len(offs); i++ {
			cp.Exprs = append(cp.Exprs, append([]byte(nil), buf[offs[i]:offs[i+1]]...))
		}
		gotAds = append(gotAds, cp)
	}
	got := adsByName(t, gotAds)
	if len(got) != 1 {
		t.Fatalf("wire query returned %d ads, want 1", len(got))
	}
	for name, attrs := range want {
		for a, v := range attrs {
			if got[name][a] != v {
				t.Errorf("%s.%s: wire %q != text %q", name, a, got[name][a], v)
			}
		}
	}
}

// TestScanRawWireInMemoryParallel runs the same equivalence through the FAN-OUT path. The
// parallel scan assembles rows itself rather than calling the serial emitter, so a dispatch
// fixed only in one of them would leave RAM tables silently empty on every scan big enough to
// matter -- which is every scan that matters.
func TestScanRawWireInMemoryParallel(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 4, SegmentSize: 1 << 14, QueryParallelism: 4})
	defer c.Close()
	if c.inline {
		t.Fatal("fixture is not an in-memory collection")
	}
	const n = 4000
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(strings.Replace(wireExprAdTexts[0], `Name = "slot1@x"`,
			fmt.Sprintf(`Name = "slot%d@x"; Slot = %d`, i, i), 1))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%04d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	proj := []string{"Name", "Slot", "Requirements", "Joined"}
	sel := c.newWireSubsetSelector(proj, false)
	rows := 0
	engaged := c.runParallelWireScan(sel, sel.neededCount(), func(row []byte) bool {
		rows++
		buf, offs, mt, _, ok := RenderRawAdInline(row, nil, nil)
		if !ok {
			t.Error("RenderRawAdInline failed on a shipped row")
			return false
		}
		if mt != "Machine" {
			t.Errorf("row lost MyType: %q", mt)
			return false
		}
		if len(offs) != 5 { // Name, Slot, Requirements, Joined (+ the leading 0 offset)
			t.Errorf("row carries %d attributes, want 4: %q", len(offs)-1, buf)
			return false
		}
		return true
	})
	if !engaged {
		t.Fatal("the parallel wire scan declined to run; this test would not cover it")
	}
	if rows != n {
		t.Fatalf("parallel in-memory wire scan yielded %d rows, want %d", rows, n)
	}

	// The public entry point agrees with it.
	pub := 0
	for range c.ScanRawWire(proj, false) {
		pub++
	}
	if pub != n {
		t.Fatalf("ScanRawWire yielded %d rows, want %d", pub, n)
	}
}
