package wasmudf

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// fixture reads a committed .wasm. The binaries are in testdata next to the .wat
// they were built from (see testdata/build.sh); nothing here assembles wasm, so
// the suite needs no toolchain and cannot quietly skip itself on a machine that
// lacks one.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := readFixture(name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

func readFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name+".wasm"))
}

// loadFixture builds a Loader with opts and loads one fixture into it, failing the
// test if either step fails.
func loadFixture(t *testing.T, opts Options, name string, minArgs, maxArgs int) *UDF {
	t.Helper()
	l := newLoader(t, opts)
	u, err := l.Load(context.Background(), name, minArgs, maxArgs, fixture(t, name))
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	return u
}

func newLoader(t *testing.T, opts Options) *Loader {
	t.Helper()
	l, err := NewLoader(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	t.Cleanup(func() {
		if err := l.Close(context.Background()); err != nil {
			t.Errorf("closing loader: %v", err)
		}
	})
	return l
}

// loadErr loads a fixture expecting failure, and returns the error for inspection.
func loadErr(t *testing.T, opts Options, name string) error {
	t.Helper()
	l := newLoader(t, opts)
	_, err := l.Load(context.Background(), name, 0, -1, fixture(t, name))
	if err == nil {
		t.Fatalf("loading %s succeeded; want an error", name)
	}
	return err
}

// requireReason asserts the UDF's failure counters show exactly one failure and
// that it is want. Asserting on the COUNTER rather than only on the returned
// ERROR is the point: every failure path returns the same ERROR value, so a test
// that checks the value alone passes whether the code took the path it meant to
// test or any other one.
func requireReason(t *testing.T, u *UDF, want Reason) {
	t.Helper()
	s := u.Stats()
	if s.Failures != 1 {
		t.Fatalf("got %d failures %v, want exactly 1 (%v)", s.Failures, s.ByReason, want)
	}
	if n := s.ByReason[want]; n != 1 {
		t.Fatalf("got failures %v, want 1 x %v", s.ByReason, want)
	}
}

func mustInt(t *testing.T, v classad.Value) int64 {
	t.Helper()
	n, err := v.IntValue()
	if err != nil {
		t.Fatalf("value %v is not an integer: %v", v, err)
	}
	return n
}

func mustString(t *testing.T, v classad.Value) string {
	t.Helper()
	s, err := v.StringValue()
	if err != nil {
		t.Fatalf("value %v is not a string: %v", v, err)
	}
	return s
}

func TestCallInteger(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	for _, in := range []int64{0, 1, -1, 7, 1 << 40, -(1 << 40)} {
		got := u.Call([]classad.Value{classad.NewIntValue(in)})
		if n := mustInt(t, got); n != in*2 {
			t.Errorf("tagecho(%d) = %d, want %d", in, n, in*2)
		}
	}
	if s := u.Stats(); s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestCallIntegerIsNotTruncated pins the payload at 64 bits. An ABI that moved an
// integer through a float, or through an i32, would pass every test above with a
// small input and silently lose the high bits of a real one -- and ClassAd
// integers in job ads are 64-bit (an epoch in nanoseconds, a byte count).
func TestCallIntegerIsNotTruncated(t *testing.T) {
	u := loadFixture(t, Options{}, "identity", 1, 1)
	for _, in := range []int64{1 << 62, -(1 << 62), (1 << 53) + 1, -9007199254740993} {
		got := u.Call([]classad.Value{classad.NewIntValue(in)})
		if n := mustInt(t, got); n != in {
			t.Errorf("identity(%d) = %d", in, n)
		}
	}
}

func TestCallReal(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	got := u.Call([]classad.Value{classad.NewRealValue(2.5)})
	if !got.IsReal() {
		t.Fatalf("tagecho(2.5) is %v, want a real", got.Type())
	}
	f, _ := got.RealValue()
	if f != 5.0 {
		t.Errorf("tagecho(2.5) = %v, want 5", f)
	}
}

// TestCallRealStaysReal guards the int/real distinction across the ABI. 7 and 7.0
// are different ClassAd values -- 7/2 is 3 and 7.0/2 is 3.5 -- so a tag that got
// normalized on the way through would change the meaning of every expression
// containing the call.
func TestCallRealStaysReal(t *testing.T) {
	u := loadFixture(t, Options{}, "identity", 1, 1)
	got := u.Call([]classad.Value{classad.NewRealValue(7)})
	if !got.IsReal() || got.IsInteger() {
		t.Fatalf("identity(7.0) came back as %v", got.Type())
	}
	gotInt := u.Call([]classad.Value{classad.NewIntValue(7)})
	if !gotInt.IsInteger() || gotInt.IsReal() {
		t.Fatalf("identity(7) came back as %v", gotInt.Type())
	}
}

func TestCallBoolean(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	for _, in := range []bool{true, false} {
		got := u.Call([]classad.Value{classad.NewBoolValue(in)})
		if !got.IsBool() {
			t.Fatalf("tagecho(%v) is %v, want a boolean", in, got.Type())
		}
		b, _ := got.BoolValue()
		if b != !in {
			t.Errorf("tagecho(%v) = %v, want %v", in, b, !in)
		}
	}
}

// TestUndefinedAndErrorRoundTrip is the test the tag space exists for. UNDEFINED
// and ERROR are VALUES in ClassAd: a UDF has to be able to receive each one and
// to return each one, distinctly, and neither may collapse into the other or into
// a failure of the call.
func TestUndefinedAndErrorRoundTrip(t *testing.T) {
	u := loadFixture(t, Options{}, "identity", 1, 1)

	gotUndef := u.Call([]classad.Value{classad.NewUndefinedValue()})
	if !gotUndef.IsUndefined() {
		t.Errorf("identity(UNDEFINED) = %v, want UNDEFINED", gotUndef.Type())
	}
	gotErr := u.Call([]classad.Value{classad.NewErrorValue()})
	if !gotErr.IsError() {
		t.Errorf("identity(ERROR) = %v, want ERROR", gotErr.Type())
	}
	if gotUndef.IsError() || gotErr.IsUndefined() {
		t.Error("UNDEFINED and ERROR are not distinct through the ABI")
	}

	// A guest's own ERROR is its answer, not a failure of the call: nothing is
	// counted, so an operator reading the counters is not sent looking for a
	// broken module that is working as written.
	if s := u.Stats(); s.Failures != 0 {
		t.Errorf("a guest-returned ERROR counted as a failure: %v", s.ByReason)
	}
}

// TestUndefinedInErrorOut proves a UDF DECIDES about UNDEFINED rather than having
// the decision made for it. tagecho passes the tag through, so this asserts the
// host does not substitute its own three-valued rule on the way in or out.
func TestUndefinedInErrorOut(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	got := u.Call([]classad.Value{classad.NewUndefinedValue()})
	if !got.IsUndefined() {
		t.Fatalf("got %v, want UNDEFINED", got.Type())
	}
}

func TestCallStringEchoesHostArena(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	for _, in := range []string{"", "x", "a longer string with spaces", "\x00\xff binary"} {
		got := u.Call([]classad.Value{classad.NewStringValue(in)})
		if s := mustString(t, got); s != in {
			t.Errorf("tagecho(%q) = %q", in, s)
		}
	}
}

// TestCallStringFromGuestMemory covers the other direction: a result the guest
// built in memory it owns, at an address the host never handed it. It also pins
// the region placement -- concat writes at offset 1024, and if the host's region
// overlapped that, the result would come back corrupted rather than wrong in some
// obvious way.
func TestCallStringFromGuestMemory(t *testing.T) {
	u := loadFixture(t, Options{}, "concat", 2, 2)
	got := u.Call([]classad.Value{
		classad.NewStringValue("Memory"),
		classad.NewStringValue("Usage"),
	})
	if s := mustString(t, got); s != "MemoryUsage" {
		t.Errorf("concat = %q, want %q", s, "MemoryUsage")
	}
}

// TestResultIsCopiedOutOfGuestMemory pins that a returned string does not alias
// the guest's memory. tagecho returns a pointer into the host's own arena, so the
// NEXT call overwrites those bytes: a result that aliased would mutate after it
// was returned, and an ad holding it would change value without being written to.
func TestResultIsCopiedOutOfGuestMemory(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	first := u.Call([]classad.Value{classad.NewStringValue("first")})
	u.Call([]classad.Value{classad.NewStringValue("SECOND")})
	if s := mustString(t, first); s != "first" {
		t.Errorf("the first result became %q after a second call", s)
	}
}

// TestCallFrameDoesNotOverlapGuestData pins WHERE the host writes.
//
// The frame goes in pages added by memory.grow, which by construction cannot
// overlap a data segment or the guest's stack. sentinel keeps 32 known bytes at
// offset 0 and returns them, so a host writing its slots at a low address
// corrupts the first 48 of them and this comes back wrong.
//
// This test exists because a mutation that placed the region at offset 0 passed
// every other test in the package: all the other fixtures have an empty data
// section, so the overlap had nothing to collide with.
func TestCallFrameDoesNotOverlapGuestData(t *testing.T) {
	const sentinel = "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	u := loadFixture(t, Options{}, "sentinel", 1, 1)
	// A string argument, so the frame holds slots AND arena bytes -- the widest
	// write the host makes for one argument.
	got := u.Call([]classad.Value{classad.NewStringValue("an argument with some length to it")})
	if s := mustString(t, got); s != sentinel {
		t.Errorf("guest data at offset 0 came back as %q, want %q", s, sentinel)
	}
}

func TestLoaderLookup(t *testing.T) {
	l := newLoader(t, Options{})
	if _, ok := l.Lookup("tagecho"); ok {
		t.Fatal("Lookup found a UDF before anything was loaded")
	}
	u, err := l.Load(context.Background(), "tagecho", 1, 1, fixture(t, "tagecho"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := l.Lookup("tagecho")
	if !ok || got != u {
		t.Fatalf("Lookup = %v, %v; want the loaded UDF", got, ok)
	}
	if u.Name() != "tagecho" {
		t.Errorf("Name = %q", u.Name())
	}
	if lo, hi := u.Arity(); lo != 1 || hi != 1 {
		t.Errorf("Arity = %d..%d, want 1..1", lo, hi)
	}
}

// TestFixtureBinariesAreCurrent -- every .wat was compiled by build.sh since it
// was last edited.
//
// The fixtures are committed twice: the .wat a reviewer reads and the .wasm the
// tests load. The suite loads the binary on purpose, so that it needs no wasm
// toolchain and no test can skip itself on a machine without one -- and nothing
// in it would otherwise notice a source edited without a rebuild. The tests would
// stay green against the old binary while the source said something else, which
// is the worst shape this can take: the documentation and the thing under test
// disagree and everything reports success.
//
// So build.sh records the hash of every SOURCE it compiled, and this compares.
// Hashing the sources rather than rebuilding and comparing bytes is deliberate:
// wat2wasm's output is only comparable against the same wabt, so a byte
// comparison in CI would fail on an encoder change with nothing actually stale.
func TestFixtureBinariesAreCurrent(t *testing.T) {
	const manifest = "testdata/fixtures.sha256"
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading %s: %v (run wasmudf/testdata/build.sh)", manifest, err)
	}

	// name -> hex digest, as `sha256sum *.wat` writes it.
	recorded := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("%s: cannot parse %q", manifest, line)
		}
		recorded[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}

	sources, err := filepath.Glob(filepath.Join("testdata", "*.wat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("no .wat fixtures found")
	}

	for _, src := range sources {
		name := filepath.Base(src)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(b))
		want, ok := recorded[name]
		if !ok {
			t.Errorf("%s is not in %s: run wasmudf/testdata/build.sh", name, manifest)
			continue
		}
		if sum != want {
			t.Errorf("%s has changed since it was last compiled; %s.wasm is stale. Run wasmudf/testdata/build.sh",
				name, strings.TrimSuffix(name, ".wat"))
		}
		delete(recorded, name)

		// And the binary the tests actually load exists.
		if _, err := os.Stat(filepath.Join("testdata", strings.TrimSuffix(name, ".wat")+".wasm")); err != nil {
			t.Errorf("%s has no compiled binary: %v", name, err)
		}
	}
	for name := range recorded {
		t.Errorf("%s is in %s but no longer exists: run wasmudf/testdata/build.sh", name, manifest)
	}
}
