package wasmudf

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestLoadRejections -- everything checkable is a load error, because a UDF that
// cannot work is better refused once, with a message, than registered and turned
// into one ERROR per record with nothing naming the cause. Each case asserts the
// message says WHICH thing is wrong: a uniform "could not load" would pass this
// table while telling the author of the module nothing.
func TestLoadRejections(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    string
	}{
		{"badsig", exportCall + " must be (func (param i32 i32 i32) (result i32))"},
		{"badversion", "module speaks ABI version 7, host speaks 1"},
		{"nomemory", `no exported memory "memory"`},
		{"hostimport", "wasi_snapshot_preview1.fd_write"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			err := loadErr(t, Options{}, tc.fixture)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestLoadRejectionRegistersNothing -- a refused module leaves no trace, so a
// caller can retry the name with a fixed module.
func TestLoadRejectionRegistersNothing(t *testing.T) {
	l := newLoader(t, Options{})
	if _, err := l.Load(context.Background(), "f", 1, 1, fixture(t, "badversion")); err == nil {
		t.Fatal("badversion loaded")
	}
	if _, ok := l.Lookup("f"); ok {
		t.Error("a refused module is in the loader")
	}
	if _, err := l.Load(context.Background(), "f", 1, 1, fixture(t, "identity")); err != nil {
		t.Errorf("reloading the name after a refusal: %v", err)
	}
}

// TestStartFunctionIsNotCalled -- withstart exports a trapping _start, which is
// wazero's default start function. If the host let it run, instantiation would
// fail and this module would not load at all; worse, a module whose _start did
// something useful-looking would have its main run once per pooled instance.
func TestStartFunctionIsNotCalled(t *testing.T) {
	u := loadFixture(t, Options{}, "withstart", 0, -1)
	if n := mustInt(t, u.Call(nil)); n != 11 {
		t.Errorf("withstart = %d, want 11", n)
	}
	// And again, because a second instance instantiates the same way.
	u2 := loadFixture(t, Options{MaxInstances: 1}, "withstart", 0, -1)
	for range 3 {
		if n := mustInt(t, u2.Call(nil)); n != 11 {
			t.Errorf("withstart = %d, want 11", n)
		}
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	l := newLoader(t, Options{})
	for _, b := range [][]byte{nil, {}, []byte("not wasm at all"), fixture(t, "identity")[:8]} {
		if _, err := l.Load(context.Background(), "f", 1, 1, b); err == nil {
			t.Errorf("%d bytes of garbage loaded as a module", len(b))
		}
	}
}

func TestLoadRejectsDuplicateName(t *testing.T) {
	l := newLoader(t, Options{})
	if _, err := l.Load(context.Background(), "f", 1, 1, fixture(t, "identity")); err != nil {
		t.Fatal(err)
	}
	_, err := l.Load(context.Background(), "f", 1, 1, fixture(t, "identity"))
	if err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Errorf("second load of the same name: %v", err)
	}
}

func TestLoadRejectsBadArity(t *testing.T) {
	l := newLoader(t, Options{})
	for _, tc := range [2][2]int{{-1, 1}, {3, 2}} {
		if _, err := l.Load(context.Background(), "f", tc[0], tc[1], fixture(t, "identity")); err == nil {
			t.Errorf("arity %d..%d was accepted", tc[0], tc[1])
		}
	}
	if _, err := l.Load(context.Background(), "", 1, 1, fixture(t, "identity")); err == nil {
		t.Error("an empty name was accepted")
	}
}

// TestClosedLoaderStaysCallable -- the registry is append-only, so a UDF
// registered from a Loader outlives any Close of it and will still be called by
// an expression. It has to return ERROR rather than panic or block.
func TestClosedLoaderStaysCallable(t *testing.T) {
	l, err := NewLoader(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := l.Load(context.Background(), "identity", 1, 1, fixture(t, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if n := mustInt(t, u.Call([]classad.Value{classad.NewIntValue(1)})); n != 1 {
		t.Fatalf("precondition: identity(1) = %d", n)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Twice, because the two post-Close paths differ: the first call takes the
	// pooled instance, which Close closed under it, and the second has to
	// instantiate from a closed runtime.
	for i := range 2 {
		if got := u.Call([]classad.Value{classad.NewIntValue(1)}); !got.IsError() {
			t.Errorf("call %d after Close returned %v, want ERROR", i, got)
		}
	}

	// Close is idempotent: a second one is not an error, because a process
	// shutting down may well do it twice.
	if err := l.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := l.Load(context.Background(), "g", 1, 1, fixture(t, "identity")); err == nil {
		t.Error("loading into a closed loader succeeded")
	}
}
