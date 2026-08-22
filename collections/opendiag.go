package collections

import "time"

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

	// Timing breaks a persistent Open into its phases so a slow reopen names the phase that
	// dominated, instead of surfacing only as one elapsed number. Durations accumulate across the
	// collection's shards; all zero for an in-memory Open. See OpenTiming. Purely observational.
	Timing OpenTiming
}

// OpenTiming records how long each phase of a persistent Open took, accumulated across shards.
// A reopen that adopts every sidecar (OpenIndexDiag reasons empty) but is still slow is explained
// here: the cost is in mapping segments, rebuilding the directory (a snapshot miss forces a full
// record walk), recomputing zone maps for segments that lack one, or the post-recovery index/order
// rebuilds -- and this says which.
type OpenTiming struct {
	MapSegments    time.Duration // readdir + mmap each segment file + derive its write extent
	DirRestore     time.Duration // load the dir snapshot, or (on a miss) rebuild it by walking records
	DirRebuilt     bool          // true if the directory was rebuilt (snapshot missing/stale) not restored
	PublishDict    time.Duration // restore each interned segment's attribute dictionary
	PublishColumns time.Duration // publish each columnarized segment's columnar payload
	LoadSidecars   time.Duration // map each sealed segment's index sidecar (adopt zone map + indexes)
	ZoneRecompute  time.Duration // of LoadSidecars, time decoding records to rebuild a missing zone map
	Reindex        time.Duration // post-recovery rebuild of indexes not adopted from sidecars
	RebuildOrdered time.Duration // post-recovery rebuild of the maintained ordered indexes
	AdoptSchema    time.Duration // re-enable columnar schema-scan from persisted blocks
	LoadDicts      time.Duration // load on-disk dictionaries before segment recovery
	LoadDemand     time.Duration // load checkpointed query demand
}

// add accumulates o2 into o (used to fold each shard's phase timings into the collection total).
func (o *OpenTiming) add(o2 OpenTiming) {
	o.MapSegments += o2.MapSegments
	o.DirRestore += o2.DirRestore
	o.DirRebuilt = o.DirRebuilt || o2.DirRebuilt
	o.PublishDict += o2.PublishDict
	o.PublishColumns += o2.PublishColumns
	o.LoadSidecars += o2.LoadSidecars
	o.ZoneRecompute += o2.ZoneRecompute
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
