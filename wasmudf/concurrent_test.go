package wasmudf

import (
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestConcurrentCalls is the test the instance pool exists for. Evaluation is
// concurrent -- a scan hands the same compiled expression to several goroutines
// -- so one registered UDF is called from many at once.
//
// Each goroutine uses a DISTINCT argument and checks its own answer. That is what
// makes the test about the pool rather than about wazero: two calls sharing an
// instance would be writing each other's call frames in one linear memory, and
// the symptom is one goroutine getting another's result -- which a test using the
// same argument everywhere could not see.
func TestConcurrentCalls(t *testing.T) {
	const goroutines, each = 16, 300
	u := loadFixture(t, Options{MaxInstances: 4}, "tagecho", 1, 1)

	var wg sync.WaitGroup
	errs := make(chan string, goroutines*each)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				in := int64(g*1_000_000 + i)
				got := u.Call([]classad.Value{classad.NewIntValue(in)})
				n, err := got.IntValue()
				if err != nil {
					errs <- "not an integer"
					return
				}
				if n != in*2 {
					errs <- "crossed results"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent call: %s", e)
	}

	s := u.Stats()
	if s.Calls != goroutines*each {
		t.Errorf("counted %d calls, want %d", s.Calls, goroutines*each)
	}
	if s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
	// MaxInstances is a bound, not a target: fewer is fine, more is the pool
	// having leaked its semaphore.
	if s.Instances > 4 {
		t.Errorf("created %d instances with MaxInstances=4", s.Instances)
	}
	requireFullSemaphore(t, u)
}

// TestConcurrentStringCalls repeats it with strings, because the string path is
// the one with shared mutable state: every call writes its bytes into its own
// instance's arena and reads a pointer back out of it. A pooled arena shared by
// two calls shows up here as one goroutine reading the other's bytes.
func TestConcurrentStringCalls(t *testing.T) {
	const goroutines, each = 8, 200
	u := loadFixture(t, Options{MaxInstances: 3}, "concat", 2, 2)

	var wg sync.WaitGroup
	bad := make(chan string, goroutines*each)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct lengths as well as distinct bytes, so a crossed arena
			// shows up as a wrong length even if the bytes happened to match.
			left := strings.Repeat(string(rune('a'+g)), g+1)
			for range each {
				got := u.Call([]classad.Value{
					classad.NewStringValue(left),
					classad.NewStringValue("|tail"),
				})
				s, err := got.StringValue()
				if err != nil || s != left+"|tail" {
					bad <- s
					return
				}
			}
		}()
	}
	wg.Wait()
	close(bad)
	for s := range bad {
		t.Fatalf("concurrent string call returned %q", s)
	}
}

// TestConcurrentCallsOnATrappingModule exercises the discard path under
// concurrency, where it has to return its token as reliably as the reuse path
// does. A leaked token is a pool that shrinks by one per failure and eventually
// blocks forever -- a hang, not an error, and one that only appears after enough
// failures to use up the pool.
func TestConcurrentCallsOnATrappingModule(t *testing.T) {
	const goroutines, each = 8, 20
	u := loadFixture(t, Options{MaxInstances: 2}, "trap", 0, -1)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if got := u.Call(nil); !got.IsError() {
					t.Errorf("trap returned %v", got)
					return
				}
			}
		}()
	}
	wg.Wait()
	if s := u.Stats(); s.ByReason[ReasonTrap] != goroutines*each {
		t.Errorf("counted %v, want %d traps", s.ByReason, goroutines*each)
	}
	requireFullSemaphore(t, u)
}

// requireFullSemaphore asserts every token is back once no call is in flight.
//
// A token leaked on some path shrinks the pool by one per occurrence and
// eventually blocks a caller forever, which shows up as a HANG -- a test that
// never fails, just stops. Reading the semaphore directly turns that into a named
// failure, and catches a leak long before it has consumed the whole pool.
func requireFullSemaphore(t *testing.T, u *UDF) {
	t.Helper()
	if got, want := len(u.tokens), cap(u.tokens); got != want {
		t.Errorf("%d of %d instance tokens returned; the pool leaked %d", got, want, want-got)
	}
}

// TestConcurrentLoads -- Load mutates the loader's table and compiles into its
// runtime, both from whatever goroutine is configuring UDFs. Nothing says that is
// one goroutine.
func TestConcurrentLoads(t *testing.T) {
	l := newLoader(t, Options{})
	wasm := fixture(t, "identity")

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := string(rune('a' + i))
			u, err := l.Load(t.Context(), name, 1, 1, wasm)
			if err != nil {
				t.Errorf("loading %s: %v", name, err)
				return
			}
			if got := u.Call([]classad.Value{classad.NewIntValue(5)}); !got.IsInteger() {
				t.Errorf("%s(5) = %v", name, got)
			}
		}()
	}
	wg.Wait()
	for i := range 8 {
		if _, ok := l.Lookup(string(rune('a' + i))); !ok {
			t.Errorf("%s is missing from the loader", string(rune('a'+i)))
		}
	}
}
