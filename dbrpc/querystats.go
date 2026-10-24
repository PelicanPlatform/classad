package dbrpc

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/PelicanPlatform/classad/collections"
)

// QueryStatsOptions configures the server's query aggregation (Server.EnableQueryStats).
// The zero value is the default of every field.
type QueryStatsOptions struct {
	// MaxShapes bounds how many distinct query shapes are tracked. A client that
	// composes a novel shape per request must not be able to grow the map without
	// limit, so past the cap the shape with the LOWEST total time is evicted to make
	// room -- the one least worth an operator's attention. Default queryStatsMaxShapes.
	MaxShapes int

	// MaxExemplars is how many slowest instances each shape keeps. Default
	// queryStatsMaxExemplars.
	MaxExemplars int

	// MinDuration records only queries at least this slow. The default (0) records
	// every query, which is deliberate: the query worth fixing is usually a 40ms one
	// run ten thousand times, and a threshold high enough to be called "slow" is
	// exactly the one that hides it. Raise it on a server where recording cost
	// matters (each recorded query parses its constraint once).
	MinDuration time.Duration
}

const (
	queryStatsMaxShapes    = 256
	queryStatsMaxExemplars = 4
)

// QueryExemplar is one actual observed query: the slowest instances of a shape are kept
// so an operator can reproduce the cost, not merely read about it.
//
// The exemplar is the point of keeping both levels. In this engine the LITERAL decides
// the work: `CompletionDate >= <now-1h>` prunes to a couple of sealed segments on their
// zone maps while `CompletionDate >= <now-365d>` touches every one of them, and the two
// are the same shape. The shape bucket says which pattern costs; the exemplar says which
// parameterization to paste into EXPLAIN.
//
// An exemplar carries the real constraint text and the requesting identity, so it can
// name a user and the job ids they asked about -- unlike a shape, which carries no data.
// The server returns exemplars only to a DAEMON-authorized peer.
type QueryExemplar struct {
	Constraint string                `json:"constraint"`
	Duration   time.Duration         `json:"duration"`
	Rows       int                   `json:"rows"`
	Projection int                   `json:"projection"`
	Scan       collections.ScanStats `json:"scan"`
	User       string                `json:"user,omitempty"`
	At         time.Time             `json:"at"`
}

// QueryShapeStat is the aggregate for one query shape.
type QueryShapeStat struct {
	Op    string `json:"op"`
	Table string `json:"table"`
	// Shape is the constraint fingerprint: the constant-folded expression with every
	// literal replaced by "?" and attribute names lowercased (see queryShape).
	Shape string `json:"shape"`
	// Limited records whether the client sent a LIMIT. It is part of the shape key
	// because the same predicate with and without one does genuinely different work.
	Limited bool `json:"limited"`

	Count int64         `json:"count"`
	Total time.Duration `json:"total"`
	Max   time.Duration `json:"max"`
	Rows  int64         `json:"rows"`

	// Unprojected counts the instances that asked for no server-side projection, i.e.
	// every attribute of every matched ad crossed the wire. ProjectionMax is the widest
	// projection any instance asked for.
	Unprojected   int64 `json:"unprojected"`
	ProjectionMax int   `json:"projectionMax"`

	// SegmentsScanned and SegmentsPruned total the scan work across the instances that
	// reported it. Their ratio is what separates "slow because the window is big" from
	// "slow because nothing pruned".
	SegmentsScanned int64 `json:"segmentsScanned"`
	SegmentsPruned  int64 `json:"segmentsPruned"`
	// ScanSamples is how many instances reported scan work; the rest ran on a path that
	// does not thread it, so a zero scan total with a non-zero Count means "not
	// measured", not "no work".
	ScanSamples int64 `json:"scanSamples"`

	// Exemplars are the slowest observed instances, slowest first. Empty for a peer not
	// authorized to see them.
	Exemplars []QueryExemplar `json:"exemplars,omitempty"`
}

// Mean is the shape's average duration.
func (q QueryShapeStat) Mean() time.Duration {
	if q.Count == 0 {
		return 0
	}
	return time.Duration(int64(q.Total) / q.Count)
}

// QueryStatsReport is the server's aggregated query report.
type QueryStatsReport struct {
	// Enabled is false when the server is not aggregating; every other field is then zero.
	Enabled bool `json:"enabled"`
	// Since is when aggregation started (or was last reset).
	Since time.Time `json:"since"`
	// Recorded is how many queries have been aggregated, Evicted how many shapes were
	// dropped to stay inside the cap.
	Recorded int64 `json:"recorded"`
	Evicted  int64 `json:"evicted"`
	// Tracked is how many shapes are held right now.
	Tracked int `json:"tracked"`
	// Shapes, ordered by total time descending -- count x mean, not max. The query worth
	// an operator's time is usually the cheap one run constantly, not the single slowest.
	Shapes []QueryShapeStat `json:"shapes"`
	// Exemplars reports whether exemplars were included. False means the peer is not
	// DAEMON-authorized, not that none were recorded.
	Exemplars bool `json:"exemplars"`
}

// queryStats aggregates observed queries by shape. Every exported entry point is safe
// for concurrent use: the server records from whichever goroutine ran the query.
type queryStats struct {
	opts  QueryStatsOptions
	mu    sync.Mutex
	m     map[string]*shapeBucket
	since time.Time
	count int64
	evict int64
}

type shapeBucket struct {
	stat QueryShapeStat
}

func newQueryStats(opts QueryStatsOptions) *queryStats {
	if opts.MaxShapes <= 0 {
		opts.MaxShapes = queryStatsMaxShapes
	}
	if opts.MaxExemplars <= 0 {
		opts.MaxExemplars = queryStatsMaxExemplars
	}
	return &queryStats{opts: opts, m: make(map[string]*shapeBucket), since: time.Now()}
}

// record aggregates one observed query. It is called on the goroutine that served the
// query, after its last frame was written, and must stay cheap: the constraint is
// fingerprinted OUTSIDE the lock and the locked section is a map update.
func (qs *queryStats) record(q QueryLog) {
	if qs == nil || q.Duration < qs.opts.MinDuration {
		return
	}
	shape := queryShape(q.Constraint)
	limited := q.Limit > 0
	key := q.Op + "\x00" + q.Table + "\x00" + shape
	if limited {
		key += "\x00L"
	}

	qs.mu.Lock()
	defer qs.mu.Unlock()
	qs.count++
	b := qs.m[key]
	if b == nil {
		if len(qs.m) >= qs.opts.MaxShapes {
			qs.evictLowest()
		}
		b = &shapeBucket{stat: QueryShapeStat{Op: q.Op, Table: q.Table, Shape: shape, Limited: limited}}
		qs.m[key] = b
	}
	st := &b.stat
	st.Count++
	st.Total += q.Duration
	if q.Duration > st.Max {
		st.Max = q.Duration
	}
	st.Rows += int64(q.Rows)
	if q.Projection == 0 {
		st.Unprojected++
	} else if q.Projection > st.ProjectionMax {
		st.ProjectionMax = q.Projection
	}
	if q.Scan.SegmentsTotal > 0 {
		st.ScanSamples++
		st.SegmentsScanned += int64(q.Scan.SegmentsScanned)
		st.SegmentsPruned += int64(q.Scan.SegmentsPruned)
	}
	qs.keepExemplar(st, q)
}

// keepExemplar inserts q into the shape's slowest-instance list, kept sorted slowest
// first and capped at MaxExemplars.
func (qs *queryStats) keepExemplar(st *QueryShapeStat, q QueryLog) {
	keep := qs.opts.MaxExemplars
	if len(st.Exemplars) >= keep && q.Duration <= st.Exemplars[len(st.Exemplars)-1].Duration {
		return
	}
	ex := QueryExemplar{
		Constraint: q.Constraint,
		Duration:   q.Duration,
		Rows:       q.Rows,
		Projection: q.Projection,
		Scan:       q.Scan,
		User:       q.User,
		At:         time.Now(),
	}
	i := sort.Search(len(st.Exemplars), func(i int) bool { return st.Exemplars[i].Duration < q.Duration })
	st.Exemplars = append(st.Exemplars, QueryExemplar{})
	copy(st.Exemplars[i+1:], st.Exemplars[i:])
	st.Exemplars[i] = ex
	if len(st.Exemplars) > keep {
		st.Exemplars = st.Exemplars[:keep]
	}
}

// evictLowest drops the tracked shape with the least total time. Called with the lock
// held, only when a new shape arrives at the cap.
func (qs *queryStats) evictLowest() {
	var worstKey string
	var worst time.Duration
	first := true
	for k, b := range qs.m {
		if first || b.stat.Total < worst {
			worstKey, worst, first = k, b.stat.Total, false
		}
	}
	if !first {
		delete(qs.m, worstKey)
		qs.evict++
	}
}

// report snapshots the aggregate. table filters to one table ("" = all); exemplars
// includes the per-shape exemplars (authorization is the caller's decision). Shapes come
// back ordered by total time descending.
func (qs *queryStats) report(table string, exemplars bool) QueryStatsReport {
	if qs == nil {
		return QueryStatsReport{}
	}
	qs.mu.Lock()
	defer qs.mu.Unlock()
	rep := QueryStatsReport{
		Enabled:   true,
		Since:     qs.since,
		Recorded:  qs.count,
		Evicted:   qs.evict,
		Tracked:   len(qs.m),
		Exemplars: exemplars,
	}
	for _, b := range qs.m {
		if table != "" && b.stat.Table != table {
			continue
		}
		st := b.stat // copy; the exemplar slice is then replaced, never shared
		if exemplars {
			st.Exemplars = append([]QueryExemplar(nil), b.stat.Exemplars...)
		} else {
			st.Exemplars = nil
		}
		rep.Shapes = append(rep.Shapes, st)
	}
	sort.Slice(rep.Shapes, func(i, j int) bool {
		if rep.Shapes[i].Total != rep.Shapes[j].Total {
			return rep.Shapes[i].Total > rep.Shapes[j].Total
		}
		return rep.Shapes[i].Shape < rep.Shapes[j].Shape
	})
	return rep
}

// reset clears the aggregate and restarts the observation window.
func (qs *queryStats) reset() {
	if qs == nil {
		return
	}
	qs.mu.Lock()
	defer qs.mu.Unlock()
	qs.m = make(map[string]*shapeBucket)
	qs.since = time.Now()
	qs.count = 0
	qs.evict = 0
}

// --- client ---

// QueryStats fetches the server's aggregated query report: every query it has served
// bucketed by shape and ranked by total time, each bucket carrying the slowest actual
// instances it saw. table filters to one table ("" = every table).
//
// Exemplars (the real constraint text, the requesting identity) come back only on a
// DAEMON-authorized connection; QueryStatsReport.Exemplars says whether they did.
// A server that is not aggregating answers with Enabled false rather than an error.
func (c *Client) QueryStats(ctx context.Context, table string) (*QueryStatsReport, error) {
	return c.queryStats(ctx, table, false)
}

// ResetQueryStats clears the aggregate and restarts the observation window, returning
// the report as it stood before the reset. DAEMON-authorized.
func (c *Client) ResetQueryStats(ctx context.Context, table string) (*QueryStatsReport, error) {
	return c.queryStats(ctx, table, true)
}

func (c *Client) queryStats(ctx context.Context, table string, reset bool) (*QueryStatsReport, error) {
	var flag byte
	if reset {
		flag = 1
	}
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putU8(putStr(req(id, opQueryStats), table), flag)
	})
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var rep QueryStatsReport
	if err := json.Unmarshal([]byte(body.str()), &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}
