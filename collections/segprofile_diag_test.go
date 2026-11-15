package collections

import "testing"

func TestSegmentProfilesReportsWhatIsOrderable(t *testing.T) {
	a := profiledArchive(t, t.TempDir(), 1200)
	defer a.Close()

	profiled, sealed := a.ProfiledSegments()
	if sealed == 0 {
		t.Fatal("no sealed segments")
	}
	if profiled != sealed {
		t.Errorf("%d/%d sealed segments are orderable by %q", profiled, sealed, a.EvictionAttr())
	}
	for _, p := range a.SegmentProfiles() {
		if p.Attr != "T" || !p.HasValues || p.Bytes <= 0 {
			t.Errorf("segment %d: attr=%q hasValues=%v bytes=%d", p.Segment, p.Attr, p.HasValues, p.Bytes)
		}
		if p.Buckets == 0 {
			t.Errorf("segment %d carries no breakpoints: a cutoff cannot be placed inside it", p.Segment)
		}
	}
}

// The anomaly the diagnostic exists for: a source that stamps its OWN clock instead of
// deriving the value from the record produces segments whose values sit inside their own
// receipt window. Such a segment orders correctly against others from that source and
// wrongly against everything else.
func TestClockSuspectDistinguishesSourceStampedValues(t *testing.T) {
	const recv = 1_700_000_000

	// Derived from the records: job times long before this store heard of them.
	derived := &profileAcc{attr: "T"}
	for i := 0; i < 6; i++ {
		derived.add(float64(1_600_000_000+i), 50, true)
	}
	if clockSuspect(derived.finish(100, recv, recv)) {
		t.Error("a segment of genuinely old records was flagged")
	}

	// Stamped on receipt: every value lands in the receipt window.
	stamped := &profileAcc{attr: "T"}
	for i := 0; i < 6; i++ {
		stamped.add(float64(recv+i), 50, true)
	}
	if !clockSuspect(stamped.finish(100, recv, recv)) {
		t.Error("a segment whose values track its own receipt time was NOT flagged")
	}

	// No receipt window recorded (a legacy profile): nothing to compare against, so no claim.
	if clockSuspect(stamped.finish(100, 0, 0)) {
		t.Error("flagged a segment with no recorded receipt window")
	}
}
