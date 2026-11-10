package db

import (
	"fmt"
	"testing"
	"time"
)

// ArchiveTable.Maintain retrains the dictionary on a clock of its own, much longer than the
// pass interval and off unless configured. These cover WHEN it fires. That an append-only
// retrain is cheap in the steady state is a property of RetrainDict, pinned in collections by
// TestRetrainDoesNotRewriteArchive; that the FIRST one is not is why it is opt-in.

// retrainFixture is an archive of dictionary-friendly ads: repeated attribute names and a long
// shared command path, so training reliably succeeds. A fixture of random or near-unique
// records makes BuildDict decline, and a declined retrain is indistinguishable from one that
// was never attempted.
func retrainFixture(t *testing.T) (*ArchiveTable, func()) {
	t.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 13})
	if err != nil {
		cat.Close()
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"user_%d\"\nJobStatus = \"Completed\"\n"+
			"Cmd = \"/usr/bin/some_long_repeated_command_path\"\nArgs = \"--flag=value --other=thing\"",
			i, i%20)
		if appendErr := a.AppendOld(ad); appendErr != nil {
			cat.Close()
			t.Fatal(appendErr)
		}
	}
	return a, func() { cat.Close() }
}

// retrainPass runs one maintenance pass with the given retrain interval, with everything that
// re-encodes on its own turned off so what the pass does to the dictionary is isolated.
func retrainPass(a *ArchiveTable, every time.Duration) {
	a.Maintain(MaintainOptions{
		SampleMax:                 64,
		ArchiveRetrainInterval:    every,
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
	})
}

// retrainedAt is when this process last retrained the archive, zero if never.
func retrainedAt(a *ArchiveTable) time.Time { return a.CodecStats(16).LastRetrain }

// TestArchiveRetrainsOnlyOnceAnIntervalHasElapsed: the first pass after an open must not
// retrain, because the clock starts at the open. Counting from the zero time instead would
// make every archive look overdue on its first pass, so a daemon that restarts often would
// retrain on every start -- ignoring the cadence exactly where it matters most, since the
// first retrain of an inline archive rewrites its whole history.
func TestArchiveRetrainsOnlyOnceAnIntervalHasElapsed(t *testing.T) {
	a, cleanup := retrainFixture(t)
	defer cleanup()

	retrainPass(a, time.Hour)
	if at := retrainedAt(a); !at.IsZero() {
		t.Fatalf("the first pass after the open retrained at %v although the interval is an "+
			"hour: a restarting daemon would retrain on every start", at)
	}

	// Age the clock past the interval. Nothing else about the archive changes.
	a.retrainMu.Lock()
	a.lastRetrain = time.Now().Add(-2 * time.Hour)
	a.retrainMu.Unlock()

	retrainPass(a, time.Hour)
	first := retrainedAt(a)
	if first.IsZero() {
		t.Fatal("no retrain after the interval elapsed. If BuildDict declined, this fixture's " +
			"ads are no longer dictionary-friendly and the test is not exercising the cadence")
	}

	// Immediately again: the interval is now counted from the retrain just recorded.
	retrainPass(a, time.Hour)
	if again := retrainedAt(a); !again.Equal(first) {
		t.Errorf("a second pass retrained again at %v (first %v); the interval is not counted "+
			"from the last retrain", again, first)
	}
}

// TestArchiveRetrainIsOffUnlessConfigured: the zero value means never, however overdue the
// clock is. This is the default, so it is what an existing deployment gets on an upgrade.
func TestArchiveRetrainIsOffUnlessConfigured(t *testing.T) {
	a, cleanup := retrainFixture(t)
	defer cleanup()

	a.retrainMu.Lock()
	a.lastRetrain = time.Now().Add(-365 * 24 * time.Hour) // overdue by any measure
	a.retrainMu.Unlock()

	retrainPass(a, 0)
	if at := retrainedAt(a); !at.IsZero() {
		t.Errorf("retrained at %v with the interval unset; it must be opt-in", at)
	}
	// The same pass with an interval set does retrain, so the case above is the default doing
	// its job rather than the fixture failing to train.
	retrainPass(a, time.Nanosecond)
	if retrainedAt(a).IsZero() {
		t.Fatal("no retrain even with a one-nanosecond interval: the off case proved nothing")
	}
}

// TestArchiveRetrainReportsWhatItDid: the pass does not log, so a caller learns what happened
// only through the hook -- including a decline, which leaves the dictionary in place and is
// not a failure of the pass.
func TestArchiveRetrainReportsWhatItDid(t *testing.T) {
	a, cleanup := retrainFixture(t)
	defer cleanup()
	a.retrainMu.Lock()
	a.lastRetrain = time.Now().Add(-2 * time.Hour)
	a.retrainMu.Unlock()

	var got []RetrainResult
	a.Maintain(MaintainOptions{
		SampleMax:                 64,
		ArchiveRetrainInterval:    time.Hour,
		ArchiveMergeDisabled:      true,
		ArchiveIndexBackfillBytes: -1,
		OnRetrain:                 func(r RetrainResult) { got = append(got, r) },
	})
	if len(got) != 1 {
		t.Fatalf("the hook fired %d times, want once per attempted retrain", len(got))
	}
	if got[0].Err != nil {
		t.Fatalf("training declined: %v", got[0].Err)
	}
	if got[0].DictBytes <= 0 {
		t.Errorf("reported a %d-byte dictionary for a successful retrain", got[0].DictBytes)
	}
	if got[0].Took <= 0 {
		t.Errorf("reported a non-positive duration %v", got[0].Took)
	}

	// A pass that does not retrain says nothing at all.
	got = nil
	retrainPass(a, time.Hour)
	if len(got) != 0 {
		t.Errorf("the hook fired %d times on a pass that did not retrain", len(got))
	}
}

// TestArchiveRetrainEveryResolvesTheInterval covers how the option reads.
func TestArchiveRetrainEveryResolvesTheInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
		on   bool
	}{
		{"unset means off", 0, 0, false},
		{"negative means off", -time.Second, 0, false},
		{"a set interval is used as given", 90 * time.Minute, 90 * time.Minute, true},
		{"the recommended cadence is a day", DefaultArchiveRetrainInterval, 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, on := MaintainOptions{ArchiveRetrainInterval: tc.set}.ArchiveRetrainEvery()
			if on != tc.on || got != tc.want {
				t.Errorf("got (%v, %v), want (%v, %v)", got, on, tc.want, tc.on)
			}
		})
	}
}
