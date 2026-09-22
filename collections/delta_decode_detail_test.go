package collections

import (
	"errors"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The counts say how often a chain would not decode; only the sampled error says what the
// decoder objected to. A deployment refusing hundreds of writes an hour with nothing but a
// count cannot tell a truncated record from one in a format the reader does not recognise.
func TestDecodeFailureIsSampled(t *testing.T) {
	lastDecodeFailure.Store(nil)
	if stage, msg := LastDeltaDecodeFailure(); stage != "" || msg != "" {
		t.Fatalf("a fresh process reports a failure: %q %q", stage, msg)
	}

	noteDecodeFailure("base", errors.New("wire: bad tag 0x7f"))
	stage, msg := LastDeltaDecodeFailure()
	if stage != "base" || msg != "wire: bad tag 0x7f" {
		t.Errorf("got (%q, %q), want (base, wire: bad tag 0x7f)", stage, msg)
	}

	// Sampled, not accumulated: the newest example replaces the old one.
	noteDecodeFailure("patch", errors.New("wire: truncated"))
	if stage, msg = LastDeltaDecodeFailure(); stage != "patch" || msg != "wire: truncated" {
		t.Errorf("got (%q, %q), want (patch, wire: truncated)", stage, msg)
	}

	// A nil error must not overwrite a real sample with an empty one.
	noteDecodeFailure("base", nil)
	if stage, msg = LastDeltaDecodeFailure(); stage != "patch" || msg != "wire: truncated" {
		t.Errorf("a nil error cleared the sample: (%q, %q)", stage, msg)
	}
}

// Base and patch decode failures are separate reasons: the base is the whole record the chain
// merges onto, a patch is a delta layered on it, and a reader that cannot decode the base has
// a different fault from one that cannot decode an increment.
func TestBaseAndPatchDecodeAreDistinctReasons(t *testing.T) {
	if failDeltaBaseDecode == failDeltaPatchDecode {
		t.Fatal("base and patch decode share a reason")
	}
	names := map[string]bool{}
	for _, n := range UnreadableBaseReasonNames() {
		names[n] = true
	}
	for _, want := range []string{"delta-base-decode", "delta-patch-decode"} {
		if !names[want] {
			t.Errorf("%q is not a published reason name; got %v", want, UnreadableBaseReasonNames())
		}
	}
	if names["delta-decode"] {
		t.Error("the merged delta-decode reason is still published")
	}
}

// Which participant failed decides the reason AND the sampled stage, and that difference is
// made by which call in the merge loop returned the error. This drives a real chain and fails
// a chosen position, because the alternative -- asserting the enum values differ -- passes with
// both call sites reporting the same thing.
func TestBaseVersusPatchDecodeFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failOn    int // 1 = the base, 2 = the first patch
		reason    string
		wantStage string
	}{
		{"base will not decode", 1, "delta-base-decode", "base"},
		{"patch will not decode", 2, "delta-patch-decode", "patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := openDelta(t, 16)
			defer c.Close()

			const key = "42.0"
			full := classad.New()
			full.InsertAttr("ClusterId", int64(42))
			full.InsertAttr("JobStatus", int64(1))
			for i := range 20 {
				full.InsertAttr(fmt.Sprintf("Pad%02d", i), int64(i))
			}
			tx := c.Begin()
			tx.Put([]byte(key), full)
			if r := tx.Commit(); r.Conflicted() {
				t.Fatal("seed conflicted")
			}
			patch := classad.New()
			patch.InsertAttr("JobStatus", int64(2))
			w := c.Begin()
			w.PatchAttrs([]byte(key), patch, nil)
			if r := w.Commit(); r.Conflicted() || r.HasUnapplied() {
				t.Fatal("patch did not land")
			}

			lastDecodeFailure.Store(nil)
			before := UnreadableBaseReasons()[tc.reason]
			n := 0
			deltaDecodeFailHook = func() error {
				if n++; n == tc.failOn {
					return errors.New("wire: synthetic decode failure")
				}
				return nil
			}
			defer func() { deltaDecodeFailHook = nil }()

			res := patchTx(t, c, key, "CompletionDate", 1789838852, "Pad00")
			if !res.HasUnapplied() {
				t.Fatalf("the write was not refused; the chain had %d participants, so position %d was never reached", n, tc.failOn)
			}
			if got := UnreadableBaseReasons()[tc.reason] - before; got != 1 {
				t.Errorf("%s moved by %d, want 1; reasons = %v", tc.reason, got, UnreadableBaseReasons())
			}
			if stage, msg := LastDeltaDecodeFailure(); stage != tc.wantStage || msg == "" {
				t.Errorf("sampled (%q, %q), want stage %q with a message", stage, msg, tc.wantStage)
			}
		})
	}
}
