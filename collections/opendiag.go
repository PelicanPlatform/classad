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

	// StrandedSealedDeltas is how many LIVE delta records were found in sealed segments, and
	// StrandedSealedKeys names up to openStrandedSampleMax of them. Both are zero on a healthy
	// store: the seal-collapse invariant says a live fragment only ever sits in an active
	// segment, and every collapse pass relies on that. A non-zero count means those keys are
	// invisible to the collapse and will lose their base to the next compaction.
	StrandedSealedDeltas int
	StrandedSealedKeys   [][]byte

	// IgnoredFiles names files in a shard directory that recovery did not load because
	// their names do not parse as "seg-<n>.d<dict>.dat". Recovery skips them in silence,
	// which is how a mangled segment name becomes invisible -- and an invisible segment's
	// dictionary looks unreferenced, so pruning retires it. The data is still there and
	// still recoverable, but nothing says so. Non-empty means someone should look; see
	// Fsck, which reports the same files with the reason spelled out.
	IgnoredFiles []string

	// DamageStops lists, up to openDamageStopMax, segments whose recovery walk stopped at a
	// damaged record with records that verify after it. Those records are not loaded (see
	// RecordsPastDamage), unless the stop is marked Recovered: recovery then continued past the
	// damage and set the damaged records aside. Empty on a healthy store; Fsck reports the same
	// damage.
	DamageStops []OpenDamageStop

	// ChainLinksRepaired is how many bucket-chain links between records of a write-target
	// segment Open had to rewrite (see relinkActiveChains). Zero on a store this build wrote. A
	// non-zero count means an earlier crash recovery had cut those links, so until this Open a
	// read of the affected keys could merge too few delta records and return an older value.
	ChainLinksRepaired int
	// SupersessionsUndone counts supersession stamps crash recovery undid because the commit that
	// wrote them did not survive (see horizon.go), and DeletesReplayed delete markers whose stamp
	// it re-applied. Both are zero after a clean shutdown.
	SupersessionsUndone int
	DeletesReplayed     int

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
	// ZoneRecomputed is how many segments needed that rebuild, and ZoneUpgraded how many
	// then had the result written back into their sidecar so the next open adopts it
	// instead of decoding again.
	//
	// These exist because the two situations look identical in a timing log and are
	// completely different problems. Recomputed == Upgraded is a ONE-TIME cost that
	// disappears on the next start. Upgraded short of Recomputed means the write-back is
	// not sticking, and every start pays the full decode forever -- which is invisible
	// without this, since the upgrade is best-effort and its errors used to be discarded.
	ZoneRecomputed    int
	ZoneUpgraded      int
	ZoneUpgradeFailed int
	// TrailerFlagsUpgraded is how many sealed segments had the absence of their trailer
	// records written into their sidecar by this open (kidxNoDictRec / kidxNoColRec), and
	// TrailerFlagsUpgradeFailed how many could not be. Same reasoning as the zone counters
	// above: an upgrade that does not land leaves every start walking every record to learn
	// what the sidecar was supposed to say, and nothing else would show it.
	TrailerFlagsUpgraded      int
	TrailerFlagsUpgradeFailed int
	// RecordsWalked is how many records this Open examined ONE AT A TIME, summed over every
	// phase in this struct that has a per-record step.
	//
	// It is the invariant's own measurement: opening a store is O(segments), not
	// O(records), so this must stay bounded by the segment count (plus whatever the
	// one active segment holds, which has no sidecar and must be walked). A value that
	// tracks the store's record count means a phase is walking the whole archive --
	// the regression this counter exists to catch, which a wall-clock number hides
	// because it looks merely slow.
	RecordsWalked int

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
	o.ZoneRecomputed += o2.ZoneRecomputed
	o.ZoneUpgraded += o2.ZoneUpgraded
	o.ZoneUpgradeFailed += o2.ZoneUpgradeFailed
	o.TrailerFlagsUpgraded += o2.TrailerFlagsUpgraded
	o.TrailerFlagsUpgradeFailed += o2.TrailerFlagsUpgradeFailed
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

// openStrandedSampleMax bounds how many stranded keys an open reports. The count is the signal;
// the keys are there to make a handful chaseable, not to reproduce the whole set.
const openStrandedSampleMax = 20

// openStrandedRepairMax bounds how many stranded fragments one open will collect and try to
// collapse. Bounded because the repair writes: an unbounded set would turn a pathological store's
// open into an arbitrarily long write pass. What it does not finish, the next open picks up.
const openStrandedRepairMax = 10000

// openIgnoredSampleMax bounds IgnoredFiles: a directory that somehow filled with
// unparseable names should produce a diagnostic, not an unbounded slice.
const openIgnoredSampleMax = 32

// ignoreFile records a file recovery skipped. Safe on a nil receiver so a collection
// opened without diagnostics costs nothing.
func (d *OpenIndexDiag) ignoreFile(path string) {
	if d == nil || len(d.IgnoredFiles) >= openIgnoredSampleMax {
		return
	}
	d.IgnoredFiles = append(d.IgnoredFiles, path)
}

// OpenDamageStop is one segment whose recovery walk stopped at damage in the middle.
type OpenDamageStop struct {
	Path   string // the segment file
	Offset int    // where the walk stopped: the first record that failed its checksum
	// Records is how many keyed records after Offset verify and were not loaded, and Damaged
	// how many keyed records from Offset on frame but fail their checksum (at least the one at
	// Offset, unless that is the dictionary or payload). Records inside damage whose framing is
	// lost cannot be counted.
	Records int
	Damaged int
	// Dictionary is set when the segment's attribute dictionary is among them. Every record of
	// the segment is keyed by it, so the records before Offset fail to read too.
	Dictionary bool
	// ColumnarPayload is set when the segment's columnar payload is among them. The records
	// before Offset are remnants that need it, so they fail to read too.
	ColumnarPayload bool
	// Recovered is set when recovery continued past the damage because the framing past it
	// could be proven (a verified columnar payload after it, or the segment's sidecar key index).
	// Loaded is then how many keyed records after Offset verify and were loaded, Records is 0, and
	// Damaged is how many damaged spans every walk skips (DamagedRecordsSkipped): one per damaged
	// record, plus one for a damaged dictionary. Dictionary and ColumnarPayload still say whether
	// those lie past Offset; a dictionary that does not verify leaves the segment's records
	// unreadable (SegDictsUnusable).
	Recovered bool
	Loaded    int
}

// openDamageStopMax bounds DamageStops.
const openDamageStopMax = 32

func (d *OpenIndexDiag) notePastDamage(s OpenDamageStop) {
	if d == nil || len(d.DamageStops) >= openDamageStopMax {
		return
	}
	d.DamageStops = append(d.DamageStops, s)
}
