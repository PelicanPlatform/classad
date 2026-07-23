package collections

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

var projTestAdTexts = []string{
	`[MyType = "Machine"; TargetType = "Job"; Name = "slot1@a"; State = "Unclaimed";
	  Cpus = 8; Memory = 16384; OpSys = "LINUX"; Arch = "X86_64";
	  ClaimId = "<secret>";
	  Start = (LoadAvg < 0.5) && (KeyboardIdle > 900); LoadAvg = 0.25; KeyboardIdle = 1200;
	  Requirements = MY.Start && (TARGET.RequestCpus <= Cpus)]`,
	`[MyType = "Machine"; TargetType = "Job"; Name = "slot1@b"; State = "Claimed";
	  Cpus = 4; Memory = 8192; OpSys = "LINUX"; Arch = "ARM64";
	  Start = SuspendReason isnt undefined; SuspendReason = undefined]`,
}

func projTestCollection(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{})
	t.Cleanup(func() { c.Close() })
	for i, text := range projTestAdTexts {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte{byte('a' + i)}, ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func adsByName(t *testing.T, ras []RawAd) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, ra := range ras {
		m := map[string]string{}
		for _, e := range ra.Exprs {
			name, val, ok := strings.Cut(string(e), " = ")
			if !ok {
				t.Fatalf("malformed expr %q", e)
			}
			m[name] = val
		}
		key, ok := m["Name"]
		if !ok {
			t.Fatalf("ad missing Name: %v", m)
		}
		key = strings.Trim(key, `"`) // rendered value text carries its quotes
		if ra.MyType != "Machine" || ra.TargetType != "Job" {
			t.Fatalf("MyType/TargetType not lifted: %q/%q", ra.MyType, ra.TargetType)
		}
		out[key] = m
	}
	return out
}

// collectProjected materializes a projected scan (copying the reused buffers).
func collectProjected(c *Collection, projection []string, chase, redact bool) []RawAd {
	var out []RawAd
	for ra := range c.ScanRawProjected(projection, chase, redact) {
		cp := RawAd{MyType: ra.MyType, TargetType: ra.TargetType}
		for _, e := range ra.Exprs {
			cp.Exprs = append(cp.Exprs, append([]byte(nil), e...))
		}
		out = append(out, cp)
	}
	return out
}

// TestScanRawProjected verifies the in-walk projection emits exactly the
// projected attributes with the same rendered values as the unprojected scan,
// respects redaction, and never emits unprojected attributes.
func TestScanRawProjected(t *testing.T) {
	t.Parallel()
	c := projTestCollection(t)

	// Reference values from the unprojected scan.
	var full []RawAd
	for ra := range c.ScanRaw() {
		cp := RawAd{MyType: ra.MyType, TargetType: ra.TargetType}
		for _, e := range ra.Exprs {
			cp.Exprs = append(cp.Exprs, append([]byte(nil), e...))
		}
		full = append(full, cp)
	}
	want := adsByName(t, full)

	proj := []string{"Name", "State", "Cpus", "NoSuchAttr"}
	got := adsByName(t, collectProjected(c, proj, false, false))
	if len(got) != 2 {
		t.Fatalf("projected scan returned %d ads, want 2", len(got))
	}
	for name, attrs := range got {
		if len(attrs) != 3 { // Name, State, Cpus; NoSuchAttr matches nothing
			t.Errorf("%s: got %d attrs (%v), want exactly Name/State/Cpus", name, len(attrs), attrs)
		}
		for a, v := range attrs {
			if wv := want[name][a]; v != wv {
				t.Errorf("%s.%s: projected %q != unprojected %q", name, a, v, wv)
			}
		}
	}

	// Redaction: ClaimId projected explicitly must still be stripped.
	red := adsByName(t, collectProjected(c, []string{"Name", "ClaimId"}, false, true))
	for name, attrs := range red {
		if _, leak := attrs["ClaimId"]; leak {
			t.Errorf("%s: redacted projection leaked ClaimId", name)
		}
	}
	unred := adsByName(t, collectProjected(c, []string{"Name", "ClaimId"}, false, false))
	if _, ok := unred["slot1@a"]["ClaimId"]; !ok {
		t.Error("unredacted projection dropped ClaimId (should only be stripped when redact=true)")
	}
}

// TestScanRawProjectedChasesRefs verifies the reference-closure "elevator":
// projecting Start must pull in the attributes Start references (transitively,
// per ad), including ones stored EARLIER in the ad than the reference, while
// TARGET.-scoped references are not chased.
func TestScanRawProjectedChasesRefs(t *testing.T) {
	t.Parallel()
	c := projTestCollection(t)

	got := adsByName(t, collectProjected(c, []string{"Name", "Requirements"}, true, false))
	a := got["slot1@a"]
	// Requirements references MY.Start and Cpus; Start references LoadAvg and
	// KeyboardIdle (a second elevator pass); TARGET.RequestCpus must NOT appear.
	for _, needed := range []string{"Requirements", "Start", "Cpus", "LoadAvg", "KeyboardIdle"} {
		if _, ok := a[needed]; !ok {
			t.Errorf("slot1@a: closure missing %s (have %v)", needed, a)
		}
	}
	if _, ok := a["RequestCpus"]; ok {
		t.Error("slot1@a: TARGET.RequestCpus was chased into the closure")
	}
	if _, ok := a["Memory"]; ok {
		t.Error("slot1@a: unreferenced Memory leaked into the closure")
	}
	// slot1@b has no Requirements; only Name should appear.
	if b := got["slot1@b"]; len(b) != 1 {
		t.Errorf("slot1@b: got %v, want only Name", b)
	}
}

// TestScanRawProjectedHotFastPath pins the projected attributes (and the type
// fields) into the hot set, re-encodes the ads, and verifies the hot fast path
// produces exactly the walk path's output -- including when one projected
// attribute is absent from an ad (forcing that ad back onto the walk).
func TestScanRawProjectedHotFastPath(t *testing.T) {
	t.Parallel()
	c := projTestCollection(t)

	proj := []string{"Name", "State", "Cpus", "SuspendReason"} // SuspendReason absent from slot1@a
	before := adsByName(t, collectProjected(c, proj, false, false))

	c.AddHotAttrs("Name", "State", "Cpus", "SuspendReason", "MyType", "TargetType")
	// Re-put the ads so their hot headers carry the pinned attributes (production
	// converges the same way: daemons re-advertise and rewrite their ads).
	for i, key := range []string{"a", "b"} {
		_ = i
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("ad %s missing", key)
		}
		if err := c.Put([]byte(key), ad); err != nil {
			t.Fatal(err)
		}
	}

	after := adsByName(t, collectProjected(c, proj, false, false))
	if len(after) != len(before) {
		t.Fatalf("hot path returned %d ads, walk returned %d", len(after), len(before))
	}
	for name, attrs := range before {
		got := after[name]
		if len(got) != len(attrs) {
			t.Errorf("%s: hot path %v != walk %v", name, got, attrs)
			continue
		}
		for a, v := range attrs {
			if got[a] != v {
				t.Errorf("%s.%s: hot %q != walk %q", name, a, got[a], v)
			}
		}
	}
	// Redaction still composes on the hot path.
	c.AddHotAttrs("ClaimId")
	ad, _ := c.Get([]byte("a"))
	if err := c.Put([]byte("a"), ad); err != nil {
		t.Fatal(err)
	}
	red := adsByName(t, collectProjected(c, []string{"Name", "ClaimId"}, false, true))
	if _, leak := red["slot1@a"]["ClaimId"]; leak {
		t.Error("hot fast path leaked ClaimId under redaction")
	}
}

// projTestCollectionInline is projTestCollection persisted to disk -- an
// inline-names collection, the mode htcondordb tables run in.
func projTestCollectionInline(t *testing.T) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i, text := range projTestAdTexts {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte{byte('a' + i)}, ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestScanRawProjectedInline verifies the projected scan on a persistent
// (inline-names) collection: exact projected attributes, values identical to
// the unprojected scan, redaction (both projected-private and wantAll), and the
// inline hot fast path after pinning + re-encoding.
func TestScanRawProjectedInline(t *testing.T) {
	t.Parallel()
	c := projTestCollectionInline(t)

	var full []RawAd
	for ra := range c.ScanRaw() {
		cp := RawAd{MyType: ra.MyType, TargetType: ra.TargetType}
		for _, e := range ra.Exprs {
			cp.Exprs = append(cp.Exprs, append([]byte(nil), e...))
		}
		full = append(full, cp)
	}
	want := adsByName(t, full)

	proj := []string{"Name", "State", "Cpus", "NoSuchAttr"}
	got := adsByName(t, collectProjected(c, proj, false, false))
	if len(got) != 2 {
		t.Fatalf("inline projected scan returned %d ads, want 2", len(got))
	}
	for name, attrs := range got {
		if len(attrs) != 3 {
			t.Errorf("%s: got %v, want exactly Name/State/Cpus", name, attrs)
		}
		for a, v := range attrs {
			if wv := want[name][a]; v != wv {
				t.Errorf("%s.%s: inline projected %q != unprojected %q", name, a, v, wv)
			}
		}
	}

	// Redaction: projected-private stripped; unredacted keeps it.
	red := adsByName(t, collectProjected(c, []string{"Name", "ClaimId"}, false, true))
	for name, attrs := range red {
		if _, leak := attrs["ClaimId"]; leak {
			t.Errorf("%s: inline redacted projection leaked ClaimId", name)
		}
	}
	unred := adsByName(t, collectProjected(c, []string{"Name", "ClaimId"}, false, false))
	if _, ok := unred["slot1@a"]["ClaimId"]; !ok {
		t.Error("inline unredacted projection dropped ClaimId")
	}

	// wantAll + redact (the dbrpc unprojected-with-redaction shape).
	all := adsByName(t, collectProjected(c, nil, false, true))
	if _, leak := all["slot1@a"]["ClaimId"]; leak {
		t.Error("inline wantAll redaction leaked ClaimId")
	}
	if _, ok := all["slot1@a"]["Memory"]; !ok {
		t.Error("inline wantAll dropped a public attribute")
	}

	// Hot fast path: pin the projection + type fields, re-encode, same output.
	before := adsByName(t, collectProjected(c, proj, false, false))
	c.AddHotAttrs("Name", "State", "Cpus", "NoSuchAttr", "MyType", "TargetType")
	for _, key := range []string{"a", "b"} {
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("ad %s missing", key)
		}
		if err := c.Put([]byte(key), ad); err != nil {
			t.Fatal(err)
		}
	}
	after := adsByName(t, collectProjected(c, proj, false, false))
	for name, attrs := range before {
		for a, v := range attrs {
			if after[name][a] != v {
				t.Errorf("%s.%s: inline hot %q != walk %q", name, a, after[name][a], v)
			}
		}
		if len(after[name]) != len(attrs) {
			t.Errorf("%s: inline hot %v != walk %v", name, after[name], attrs)
		}
	}
}

// TestWholeAdQueriesRecordNoHotDemand locks in the "condor_status -l" rule: a
// whole-ad query (no projection) -- text or wire-form, with or without a
// constraint -- must record NO read demand, so requesting everything can never
// drag every attribute into the demand-ranked hot set. A constraint's attrs may
// gain filter (eq/rng) demand for index suggestions, but never reads; only a
// real projection records reads, and only for its own attributes.
func TestWholeAdQueriesRecordNoHotDemand(t *testing.T) {
	t.Parallel()
	c := projTestCollectionInline(t)

	reads := func(name string) int64 {
		if v, ok := c.demand.m.Load(strings.ToLower(name)); ok {
			return v.(*demandCounts).reads.Load()
		}
		return 0
	}

	// Whole-ad, every flavor.
	for range c.ScanRaw() {
	}
	for range c.ScanRawRedacted() {
	}
	for range c.ScanRawWire(nil, true) {
	}
	if q, err := vm.Parse(`Cpus == 8`); err == nil {
		for range c.QueryRawWire(q, nil, true) {
		}
		for range c.QueryRaw(q) {
		}
	} else {
		t.Fatal(err)
	}
	for _, attr := range []string{"Name", "State", "Cpus", "Memory", "OpSys", "MyType"} {
		if n := reads(attr); n != 0 {
			t.Errorf("whole-ad queries recorded read demand for %s (%d); -l must not shape the hot set", attr, n)
		}
	}
	// The constraint attr gained only filter demand.
	if v, ok := c.demand.m.Load("cpus"); ok {
		if v.(*demandCounts).eq.Load() == 0 {
			t.Error("constraint attr recorded no filter demand")
		}
	}

	// A real projection records reads for exactly its attributes (+ type fields).
	for range c.ScanRawProjected([]string{"Name", "State"}, false, false) {
	}
	for range c.ScanRawWire([]string{"Name", "State"}, false) {
	}
	if reads("Name") == 0 || reads("State") == 0 || reads("MyType") == 0 {
		t.Error("projected queries failed to record read demand for their attributes")
	}
	if reads("Memory") != 0 {
		t.Error("projected query leaked read demand for an unprojected attribute")
	}
}
