package collections

import "time"

// Live progress reporting for a persistent Open.
//
// OpenIndexDiag and OpenTiming already explain a slow reopen -- afterwards. That is the wrong
// time: opening one 7.7 GB / 843-segment archive was measured at ~380 seconds, and a daemon
// mounting three of them spends twenty minutes in a state where it serves nothing, logs
// nothing, and answers no scrape, because the metrics handler is not mounted until the
// catalog is open. The post-hoc diagnostic arrives once that is over.
//
// This is the during-the-open counterpart, and is deliberately the cheapest thing that
// answers "is it progressing": a callback with a phase name and a done/total count, rate
// limited so a 843-segment loop cannot flood a log. It is reported per shard directory, since
// that is the unit the loops iterate.

// OpenPhase names a phase of a persistent Open, for OpenProgress.
type OpenPhase string

const (
	// OpenMapSegments is the readdir + mmap pass over a shard's segment files.
	OpenMapSegments OpenPhase = "map_segments"
	// OpenLoadSidecars is the per-sealed-segment sidecar load. It is the phase that dominates
	// a large archive's open: a segment whose sidecar cannot be adopted has its index rebuilt
	// by decoding every record it holds, measured at ~95% of reopen time.
	OpenLoadSidecars OpenPhase = "load_sidecars"
)

// OpenProgress is one progress report from a persistent Open.
type OpenProgress struct {
	// Dir is the shard directory being loaded.
	Dir string
	// Phase is the phase doing the work.
	Phase OpenPhase
	// Done and Total are segments processed and segments in this phase for this shard.
	Done  int
	Total int
	// Elapsed is how long this phase has been running.
	Elapsed time.Duration
}

// openProgressInterval is the minimum gap between two OpenProgressHook calls for one phase.
//
// Per segment would be 843 calls for one archive, which is a log flood and tells an operator
// nothing a count every few seconds does not. A few seconds is short enough that a phase that
// has genuinely stopped is distinguishable from one that is merely slow -- the actual question
// -- and long enough that the callback cost is irrelevant next to an mmap.
const openProgressInterval = 5 * time.Second

// OpenProgressHook, when non-nil, is called during each persistent Collection Open with the
// progress of the phase in flight, at most once per openProgressInterval per phase. Set it
// (once, at process start) to log that a long open is advancing. A package-level var rather
// than an Option, matching OpenIndexDiagHook, which answers the same question after the fact.
//
// Diagnostic only; the callback must not block (it runs on the Open path, which holds no lock
// a caller could be waiting on but is the only thing standing between the process and serving
// requests).
var OpenProgressHook func(OpenProgress)

// openProgressReporter rate-limits progress callbacks for one phase of one shard load. The
// zero value is usable and reports nothing when the hook is unset, so every call site is
// unconditional.
type openProgressReporter struct {
	fn      func(OpenProgress)
	dir     string
	phase   OpenPhase
	total   int
	started time.Time
	last    time.Time
}

func newOpenProgress(dir string, phase OpenPhase, total int) *openProgressReporter {
	now := time.Now()
	return &openProgressReporter{fn: OpenProgressHook, dir: dir, phase: phase, total: total, started: now, last: now}
}

// step reports done units, at most once per openProgressInterval.
func (r *openProgressReporter) step(done int) {
	if r == nil || r.fn == nil {
		return
	}
	now := time.Now()
	if now.Sub(r.last) < openProgressInterval {
		return
	}
	r.last = now
	r.fn(OpenProgress{Dir: r.dir, Phase: r.phase, Done: done, Total: r.total, Elapsed: now.Sub(r.started)})
}
