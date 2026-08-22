package collections

// OpenIndexDiag summarizes, for a single Collection Open, how each sealed segment's persisted
// index sidecar was handled: adopted (mmapped, no work) vs. left for the post-Open Reindex to
// rebuild -- which decodes every record the index covers and is the dominant reopen cost (~95%
// of reopen time on a large archive). It is emitted once per Open via OpenIndexDiagHook.
//
// It exists to answer, from a production log, "why is startup rebuilding the archive indexes
// instead of adopting the ones on disk?" -- distinguishing an archive whose segments predate
// sidecar persistence (no files) from one whose sidecars are present but rejected (format/version)
// or whose attribute index is absent/stale. Purely observational; it changes no behavior.
type OpenIndexDiag struct {
	Dir              string // the collection's directory
	SealedSegments   int    // sealed (non-active) segments seen at open
	SidecarFiles     int    // of those, how many had a .idx sidecar file on disk
	KeyIndexAdopted  int    // key-index sidecar mmapped (the primary-key index; no key rebuild)
	AttrIndexAdopted int    // attribute-index sidecar mmapped -- THESE need no Reindex rebuild
	// Reasons counts, per segment whose attribute index was NOT adopted (and so will be rebuilt),
	// why it was not:
	//   "no-sidecar-file"              -- no .idx on disk (segment predates sidecar persistence)
	//   "sidecar-rejected"            -- .idx present but the container/key index was invalid
	//                                    (older on-disk format, truncation, or a version bump)
	//   "attr-section-missing-or-stale" -- key index adopted, but the attribute-index section was
	//                                    absent, CRC-bad, or did not cover the segment
	Reasons map[string]int
}

// OpenIndexDiagHook, when non-nil, is called once at the end of each Collection Open with a
// summary of sidecar adoption for that store. Set it (once, at process start) to log why a
// reopen rebuilds indexes -- the slow-startup path -- rather than adopting the persisted ones.
// Diagnostic only; the callback must not block (it runs on the Open path).
var OpenIndexDiagHook func(OpenIndexDiag)

func (d *OpenIndexDiag) note(reason string) {
	if d.Reasons == nil {
		d.Reasons = map[string]int{}
	}
	d.Reasons[reason]++
}
