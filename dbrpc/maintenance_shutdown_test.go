package dbrpc

import (
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// TestCloseWaitsForInFlightMaintenance proves Server.Close blocks until an in-flight
// background maintenance pass has actually returned -- not merely been signalled to stop.
// Without the wait, Close returns while a Compact/reindex pass is still reading segments,
// and the caller's catalog Close munmaps them out from under it (SIGSEGV on restart).
func TestCloseWaitsForInFlightMaintenance(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	s := NewServerCatalog(cat)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	compactPassHook = func() {
		once.Do(func() { close(entered) })
		<-release // hold this pass in flight until the test releases it
	}
	defer func() { compactPassHook = nil }()

	// Large Maintain floor so only the compact pass fires within the test window.
	s.StartMaintenance(time.Hour, db.MaintainOptions{CompactInterval: time.Millisecond})

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("a compaction pass never started")
	}

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()

	select {
	case <-closed:
		t.Fatal("Close returned while a compaction pass was still in flight (would race catalog Close/munmap)")
	case <-time.After(200 * time.Millisecond):
		// expected: Close is blocked in bgWG.Wait()
	}

	close(release) // let the held pass finish; the goroutines then observe stop and exit
	select {
	case <-closed:
		// correct: Close returned only after the in-flight pass drained
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the in-flight pass was released")
	}
}
