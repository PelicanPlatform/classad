//go:build unix

package collections

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestGuardWritesReportsFaultThenCrashes confirms GuardWrites runs its body normally, and on a real
// memory fault (a write to an unmapped page -- the shape a COW-writeback ENOSPC takes) reports the
// fault via onFault and then re-panics (crashes) rather than silently recovering or opaquely
// faulting. The test catches the re-panic so it can assert onFault fired.
func TestGuardWritesReportsFaultThenCrashes(t *testing.T) {
	// Normal path: fn runs, no fault, GuardWrites returns.
	ran := false
	GuardWrites(nil, func() { ran = true })
	if !ran {
		t.Fatal("GuardWrites did not run fn on the normal path")
	}

	// Fault path: onFault is invoked, then GuardWrites re-panics (which we recover here to keep the
	// test process alive -- in production the re-panic terminates the daemon).
	var got any
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				crashed = true
			}
		}()
		GuardWrites(func(f any) { got = f }, func() {
			data, err := unix.Mmap(-1, 0, unix.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
			if err != nil {
				t.Skipf("mmap unavailable: %v", err)
			}
			if err := unix.Munmap(data); err != nil {
				t.Fatal(err)
			}
			data[0] = 1 // store to unmapped memory -> fault
		})
	}()
	if got == nil {
		t.Fatal("onFault was not called on a fault")
	}
	if !crashed {
		t.Fatal("GuardWrites should re-panic (crash) after a fault, not swallow it")
	}
	t.Logf("faulted with: %v", got)
}
