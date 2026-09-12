package collections

import "runtime/debug"

// GuardWrites runs fn -- the body of a long-lived goroutine that writes to the store -- with an
// mmap write fault turned into a clean, attributable crash instead of an opaque runtime fault.
//
// Even with fallocate reserving a segment's blocks up front, a copy-on-write filesystem (btrfs,
// ZFS, XFS reflinks) allocates a NEW block at mmap writeback, so a full disk can fault (SIGBUS) at
// the store instruction. Go's runtime turns that into a fatal "unexpected fault address" crash, and
// os/signal cannot intercept a synchronous fault. The only hook is runtime/debug.SetPanicOnFault,
// which is per-goroutine.
//
// Call GuardWrites ONCE around a writer goroutine's loop -- NOT per write -- so SetPanicOnFault is
// set a single time for the goroutine's lifetime and there is zero per-commit cost. On a fault it
// invokes onFault (e.g. to log "disk full on a copy-on-write filesystem; the store's durable data
// is intact -- free space and restart") and then re-panics, so the process still terminates. This
// is "crash responsibly", not recover-and-continue: a faulted write means the disk is full and the
// store cannot make progress, but the crash is now logged and attributable rather than opaque, and
// no durable data is lost (a faulted write never reached msync, so a restart recovers cleanly).
//
// onFault may be nil. Faults are rare and terminal, so re-panicking (not os.Exit) is used to
// preserve the original fault's stack for diagnosis.
func GuardWrites(onFault func(fault any), fn func()) {
	debug.SetPanicOnFault(true)
	defer func() {
		if r := recover(); r != nil {
			if onFault != nil {
				onFault(r)
			}
			panic(r) // re-raise: crash responsibly (logged), do not resume a store on a full disk
		}
	}()
	fn()
}
