package db

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/PelicanPlatform/classad/classad"
)

// Materialized views are cardinality-limited, in-memory aggregates over a base table,
// maintained incrementally from the base table's change stream (db.Watch). The view
// DEFINITION (ViewSpec) is persisted in the catalog; the DATA is in-memory and is rebuilt
// from the base table on reload. Views are intended for Prometheus metrics: columns are
// typed by alias prefix -- label_* is a Prometheus label, metric_* a metric value.
//
// Because the change stream has no before-image (an upsert carries only the new ad; a
// delete carries only the key), the view keeps a per-key contribution store so it can
// subtract a key's OLD contribution on update/delete. Memory is therefore O(base rows).
// Only COUNT/SUM/AVG are supported -- they are the aggregates maintainable by delta
// (MIN/MAX would need a rescan when the current extreme is removed).

// ViewAggFunc is a delta-maintainable aggregate.
type ViewAggFunc string

const (
	ViewCount ViewAggFunc = "count"
	ViewSum   ViewAggFunc = "sum"
	ViewAvg   ViewAggFunc = "avg"
)

// ViewGroupCol is one GROUP BY column: the base-table attribute and the alias it is stored
// under in the materialized rows (e.g. attr "Owner", alias "label_owner"). When BucketWidth
// > 0, the column is time-bucketed: the (numeric, unix-seconds) attribute is floored to
// epoch-aligned buckets of that width, turning the view into a time series -- each interval
// is a distinct, permanent group rather than an overwritten gauge. This is the same shape as
// dbrpc.GroupCol.
type ViewGroupCol struct {
	Attr        string `json:"attr"`
	Alias       string `json:"alias"`
	BucketWidth int64  `json:"bucketWidth,omitempty"` // seconds; 0 = raw value
}

// ViewMetric is one aggregate output: the function, its argument attribute ("*" for
// COUNT(*)), and the alias it is stored under (e.g. "metric_jobs").
type ViewMetric struct {
	Func  ViewAggFunc `json:"func"`
	Arg   string      `json:"arg"`
	Alias string      `json:"alias"`
}

// ViewSpec is a materialized view's persisted definition.
type ViewSpec struct {
	BaseTable string         `json:"baseTable"`
	Groups    []ViewGroupCol `json:"groups"`
	Metrics   []ViewMetric   `json:"metrics"`
	// Cardinality is the hard maximum number of groups (output series). Exceeding it fails
	// the build (CreateView error) or, at runtime, moves the view to the failed state.
	Cardinality int `json:"cardinality"`
	// SelectText is the original SELECT for display (.views); not used for execution.
	SelectText string `json:"selectText"`
}

// Validate checks a spec is well-formed and delta-maintainable.
func (s ViewSpec) Validate() error {
	if s.BaseTable == "" {
		return fmt.Errorf("view: base table is required")
	}
	if len(s.Groups) == 0 {
		return fmt.Errorf("view: at least one GROUP BY column is required")
	}
	for _, g := range s.Groups {
		if g.BucketWidth < 0 {
			return fmt.Errorf("view: time_bucket width must be positive, got %d", g.BucketWidth)
		}
	}
	if len(s.Metrics) == 0 {
		return fmt.Errorf("view: at least one aggregate metric is required")
	}
	if s.Cardinality <= 0 {
		return fmt.Errorf("view: a positive cardinality limit is required")
	}
	for _, m := range s.Metrics {
		switch m.Func {
		case ViewCount, ViewSum, ViewAvg:
		default:
			return fmt.Errorf("view: unsupported aggregate %q (only COUNT/SUM/AVG are maintainable)", m.Func)
		}
		if m.Func != ViewCount && m.Arg == "*" {
			return fmt.Errorf("view: %s requires an attribute argument, not *", m.Func)
		}
	}
	return nil
}

// ViewState is a view's lifecycle state.
type ViewState uint8

const (
	// ViewBuilding: the initial catch-up is in progress.
	ViewBuilding ViewState = iota
	// ViewActive: built and maintaining live.
	ViewActive
	// ViewStale: definition loaded but the base table is absent; will bind on the next
	// activation once the base table exists.
	ViewStale
	// ViewFailed: a runtime error (e.g. cardinality exceeded); the updater has stopped.
	ViewFailed
)

func (s ViewState) String() string {
	switch s {
	case ViewBuilding:
		return "building"
	case ViewActive:
		return "active"
	case ViewStale:
		return "stale"
	case ViewFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// metricInput is a key's evaluated contribution to one metric.
type metricInput struct {
	defined bool    // whether the metric's argument was defined for this key
	val     float64 // the numeric argument value (SUM/AVG); unused for COUNT
}

// contribution is what one base-table key contributes to the view, remembered so the OLD
// contribution can be subtracted on update/delete (the stream carries no before-image). A
// contribution with valid == false belongs to no group (a time-bucketed column's attribute
// was non-numeric); such an ad is dropped from the view entirely.
type contribution struct {
	valid    bool
	groupKey string
	labels   []string
	inputs   []metricInput
}

func (c contribution) equal(o contribution) bool {
	if c.valid != o.valid {
		return false
	}
	if !c.valid {
		return true
	}
	if c.groupKey != o.groupKey || len(c.inputs) != len(o.inputs) {
		return false
	}
	for i := range c.inputs {
		if c.inputs[i] != o.inputs[i] {
			return false
		}
	}
	return true
}

// metricAcc is one group's running accumulator for one metric.
type metricAcc struct {
	rows int64   // COUNT(*): every contributing key
	defN int64   // rows where the argument is defined (COUNT(col) and AVG denominator)
	sum  float64 // running sum (SUM/AVG)
}

// groupAcc is one group's state: its label values, how many base keys contribute (so it
// can be evicted when empty), and a per-metric accumulator.
type groupAcc struct {
	labels  []string
	members int64
	accs    []metricAcc
}

// View is a live materialized view: its spec, the per-key contribution store, per-group
// accumulators, and an in-memory backing DB holding one rendered ad per group (so the view
// is queried like a table). All mutable state is guarded by mu.
type View struct {
	spec    ViewSpec
	backing *DB // in-memory; one ad per group, keyed by groupKey

	mu      sync.Mutex
	state   ViewState
	failErr error
	contrib map[string]contribution // base key -> contribution
	groups  map[string]*groupAcc    // groupKey -> accumulator
	cursor  []byte                  // last durable watch cursor (for resume/persist)

	cancel   context.CancelFunc // stops the live updater
	stopOnce sync.Once
}

// newView constructs a view around a spec and an (empty, in-memory) backing DB.
func newView(spec ViewSpec, backing *DB) *View {
	return &View{
		spec:    spec,
		backing: backing,
		state:   ViewBuilding,
		contrib: make(map[string]contribution),
		groups:  make(map[string]*groupAcc),
	}
}

// Spec returns the view's definition.
func (v *View) Spec() ViewSpec { return v.spec }

// Backing returns the in-memory table holding the materialized rows (read-only in intent).
func (v *View) Backing() *DB { return v.backing }

// State reports the view's lifecycle state and any failure error.
func (v *View) State() (ViewState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state, v.failErr
}

// SeriesCount returns the current number of groups (Prometheus series).
func (v *View) SeriesCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.groups)
}

// contributionOf evaluates a base ad into this view's group key, label values, and metric
// inputs.
func (v *View) contributionOf(ad *classad.ClassAd) contribution {
	labels := make([]string, len(v.spec.Groups))
	for i, g := range v.spec.Groups {
		if g.BucketWidth > 0 {
			num, ok := ad.EvaluateAttrNumber(g.Attr)
			if !ok {
				// Non-numeric bucket attribute: this ad has no time bucket, so it
				// contributes to no group (dropped from the series).
				return contribution{valid: false}
			}
			labels[i] = bucketLabel(num, g.BucketWidth)
			continue
		}
		labels[i] = renderLabel(ad.EvaluateAttr(g.Attr))
	}
	inputs := make([]metricInput, len(v.spec.Metrics))
	for i, m := range v.spec.Metrics {
		if m.Func == ViewCount && m.Arg == "*" {
			inputs[i] = metricInput{defined: true}
			continue
		}
		val := ad.EvaluateAttr(m.Arg)
		if val.IsUndefined() || val.IsError() {
			inputs[i] = metricInput{defined: false}
			continue
		}
		if m.Func == ViewCount {
			inputs[i] = metricInput{defined: true} // COUNT(col): defined-only
			continue
		}
		num, ok := ad.EvaluateAttrNumber(m.Arg)
		inputs[i] = metricInput{defined: ok, val: num}
	}
	return contribution{valid: true, groupKey: strings.Join(labels, "\x00"), labels: labels, inputs: inputs}
}

// bucketLabel floors unix-epoch seconds to an epoch-aligned bucket of the given width
// (seconds) and renders it as the bucket start in decimal seconds.
func bucketLabel(sec float64, width int64) string {
	return strconv.FormatInt(int64(math.Floor(sec/float64(width)))*width, 10)
}

// add folds a key's contribution into its group (+1 member, +metrics). Creates the group if
// absent; returns an error if creating it would exceed the cardinality limit.
func (v *View) add(c contribution) error {
	g := v.groups[c.groupKey]
	if g == nil {
		if len(v.groups) >= v.spec.Cardinality {
			return fmt.Errorf("view: cardinality limit %d exceeded", v.spec.Cardinality)
		}
		g = &groupAcc{labels: c.labels, accs: make([]metricAcc, len(v.spec.Metrics))}
		v.groups[c.groupKey] = g
	}
	g.members++
	for i, in := range c.inputs {
		g.accs[i].rows++
		if in.defined {
			g.accs[i].defN++
			g.accs[i].sum += in.val
		}
	}
	return nil
}

// sub removes a key's contribution from its group, evicting the group when empty.
func (v *View) sub(c contribution) {
	g := v.groups[c.groupKey]
	if g == nil {
		return
	}
	g.members--
	for i, in := range c.inputs {
		g.accs[i].rows--
		if in.defined {
			g.accs[i].defN--
			g.accs[i].sum -= in.val
		}
	}
	if g.members <= 0 {
		delete(v.groups, c.groupKey)
	}
}

// applyUpsert applies an upsert of key with ad, maintaining the accumulators and the
// backing rows. Idempotent: a re-delivered identical ad is a no-op.
func (v *View) applyUpsert(key string, ad *classad.ClassAd) error {
	newC := v.contributionOf(ad)
	old, existed := v.contrib[key]
	if existed && old.equal(newC) {
		return nil // duplicate delivery
	}
	if existed {
		v.sub(old)
		delete(v.contrib, key)
	}
	if newC.valid {
		if err := v.add(newC); err != nil {
			// Roll back the subtraction so the accumulators stay consistent, then fail.
			if existed {
				_ = v.add(old)
				v.contrib[key] = old
			}
			return err
		}
		v.contrib[key] = newC
	}
	// Re-render the affected groups.
	if existed && old.groupKey != newC.groupKey {
		v.renderGroup(old.groupKey)
	}
	if newC.valid {
		v.renderGroup(newC.groupKey)
	}
	return nil
}

// applyDelete applies a delete of key.
func (v *View) applyDelete(key string) {
	old, existed := v.contrib[key]
	if !existed {
		return
	}
	v.sub(old)
	delete(v.contrib, key)
	v.renderGroup(old.groupKey)
}

// renderGroup syncs the backing row for groupKey: writes the rendered ad, or deletes it if
// the group was evicted.
func (v *View) renderGroup(groupKey string) {
	g := v.groups[groupKey]
	if g == nil {
		_, _ = v.backing.Delete(groupKey)
		return
	}
	ad := classad.New()
	for i, gc := range v.spec.Groups {
		// A time-bucketed column is stored as a number (the unix-seconds bucket start)
		// so downstream time-axis and archive zone-map handling see a number, not text.
		if gc.BucketWidth > 0 {
			if n, err := strconv.ParseInt(g.labels[i], 10, 64); err == nil {
				ad.InsertAttr(gc.Alias, n)
				continue
			}
		}
		ad.InsertAttrString(gc.Alias, g.labels[i])
	}
	for i, m := range v.spec.Metrics {
		acc := g.accs[i]
		switch m.Func {
		case ViewCount:
			if m.Arg == "*" {
				ad.InsertAttr(m.Alias, acc.rows)
			} else {
				ad.InsertAttr(m.Alias, acc.defN)
			}
		case ViewSum:
			ad.InsertAttrFloat(m.Alias, acc.sum)
		case ViewAvg:
			avg := 0.0
			if acc.defN > 0 {
				avg = acc.sum / float64(acc.defN)
			}
			ad.InsertAttrFloat(m.Alias, avg)
		}
	}
	_ = v.backing.Put(groupKey, ad)
}

// reset clears all view state and the backing (WatchReset: a full rebuild follows).
func (v *View) reset() {
	v.contrib = make(map[string]contribution)
	v.groups = make(map[string]*groupAcc)
	v.backing.Truncate()
}

// run does the initial catch-up and then live maintenance over a SINGLE watch (replay then
// tail), so there is never a second concurrent watch racing the base's segment windows. It
// signals ready exactly once: nil on the first WatchSynced (the view is built), or an error
// if the build failed (e.g. a cardinality overflow). After signaling built it keeps
// maintaining until ctx is cancelled. On WatchReset it rebuilds; on WatchResync it
// reconnects from the last durable cursor.
func (v *View) run(ctx context.Context, base *DB, ready chan<- error) {
	var once sync.Once
	signal := func(err error) { once.Do(func() { ready <- err }) }

	cursor := []byte(nil) // start from the beginning
	for {
		seq, err := base.Watch(ctx, cursor)
		if err != nil {
			signal(err)
			return
		}
		resync := false
		for ev := range seq {
			v.mu.Lock()
			var applyErr error
			switch ev.Kind {
			case WatchReset:
				v.reset()
			case WatchUpsert:
				if ev.Ad != nil { // a bare upsert with a nil ad is a synced marker at this layer
					applyErr = v.applyUpsert(ev.Key, ev.Ad)
				}
			case WatchDelete:
				v.applyDelete(ev.Key)
			case WatchSynced:
				v.cursor = ev.Cursor
				if v.state == ViewBuilding {
					v.state = ViewActive
				}
			case WatchResync:
				resync = true
			}
			if applyErr != nil {
				v.state, v.failErr = ViewFailed, applyErr
				v.mu.Unlock()
				signal(applyErr) // the initial build failed (no-op if already live)
				return
			}
			synced := ev.Kind == WatchSynced
			cur := v.cursor
			v.mu.Unlock()
			if synced {
				signal(nil) // built (first synced); no-op afterwards
			}
			if resync {
				cursor = cur // reconnect from the last durable cursor
				break
			}
		}
		if ctx.Err() != nil || !resync {
			signal(ctx.Err()) // stream ended before we ever synced
			return
		}
	}
}

// Cursor returns the last durable resume cursor (for persistence).
func (v *View) Cursor() []byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cursor
}

// stop cancels the live updater and closes the backing. Idempotent (called from CreateView
// rollback, DropView, and catalog Close).
func (v *View) stop() {
	v.stopOnce.Do(func() {
		if v.cancel != nil {
			v.cancel()
		}
		if v.backing != nil {
			_ = v.backing.Close()
		}
	})
}

// renderLabel converts an evaluated ClassAd value to a label string. Undefined/error render
// empty so an absent attribute groups as "".
func renderLabel(v classad.Value) string {
	switch {
	case v.IsUndefined(), v.IsError():
		return ""
	case v.IsString():
		s, _ := v.StringValue()
		return s
	case v.IsBool():
		b, _ := v.BoolValue()
		return strconv.FormatBool(b)
	case v.IsInteger():
		i, _ := v.IntValue()
		return strconv.FormatInt(i, 10)
	case v.IsReal():
		r, _ := v.RealValue()
		return strconv.FormatFloat(r, 'g', -1, 64)
	default:
		return v.String()
	}
}

// sortedViewNames returns view names sorted (for deterministic listings).
func sortedViewNames(m map[string]*View) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// --- Catalog view management ---

// CreateView creates a materialized view named name over spec.BaseTable, materializing it
// synchronously (so a cardinality-limit overflow or an aggregation error fails the create
// and leaves nothing behind) and then maintaining it live from the base table's change
// stream. The definition is persisted for a persistent catalog.
func (cat *Catalog) CreateView(name string, spec ViewSpec) error {
	if !ValidTableName(name) {
		return fmt.Errorf("catalog: invalid view name %q", name)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if _, ok := cat.views[name]; ok {
		return fmt.Errorf("catalog: view %q already exists", name)
	}
	if _, ok := cat.tables[name]; ok {
		return fmt.Errorf("catalog: %q already exists as a table", name)
	}
	if _, ok := cat.archives[name]; ok {
		return fmt.Errorf("catalog: %q already exists as an archive table", name)
	}
	base, ok := cat.tables[spec.BaseTable]
	if !ok {
		return fmt.Errorf("catalog: view base table %q does not exist", spec.BaseTable)
	}
	backing, err := OpenConfig(cat.tableConfig("")) // in-memory
	if err != nil {
		return fmt.Errorf("catalog: creating view backing for %q: %w", name, err)
	}
	v := newView(spec, backing)

	// Start the single build+maintain goroutine and wait for the initial build to finish.
	// A cardinality/aggregation error during the build fails the create and leaves nothing
	// behind; on success the goroutine keeps maintaining the view live.
	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel
	ready := make(chan error, 1)
	go v.run(ctx, base, ready)
	if err := <-ready; err != nil {
		v.stop()
		return fmt.Errorf("catalog: building view %q: %w", name, err)
	}

	if cat.dir != "" {
		if err := saveViewDef(cat.dir, name, spec); err != nil {
			v.stop()
			return fmt.Errorf("catalog: persisting view %q: %w", name, err)
		}
	}
	cat.views[name] = v
	return nil
}

// DropView removes a view: it stops the updater, drops the in-memory data, and deletes the
// persisted definition.
func (cat *Catalog) DropView(name string) error {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	v, ok := cat.views[name]
	if !ok {
		return fmt.Errorf("catalog: no such view %q", name)
	}
	delete(cat.views, name)
	v.stop()
	if cat.dir != "" {
		if err := os.RemoveAll(filepath.Join(cat.dir, viewsSubdir, name)); err != nil {
			return fmt.Errorf("catalog: removing view %q: %w", name, err)
		}
	}
	return nil
}

// Views returns the view names, sorted.
func (cat *Catalog) Views() []string {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	return sortedViewNames(cat.views)
}

// View returns the named view.
func (cat *Catalog) View(name string) (*View, bool) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	v, ok := cat.views[name]
	return v, ok
}

// ViewBacking returns the in-memory backing table of a view, so reads (SELECT/query)
// resolve a view name to its materialized rows.
func (cat *Catalog) ViewBacking(name string) (*DB, bool) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	v, ok := cat.views[name]
	if !ok {
		return nil, false
	}
	return v.backing, true
}

// recoverViews reconstructs views from <dir>/views on catalog open. Data is not persisted,
// so each view is rebuilt from its base table. A view whose base table is absent loads
// stale (rebound on a later restart once the table exists); a view whose rebuild fails
// (e.g. cardinality) loads failed. Neither fails catalog open. Runs single-threaded during
// open (no cat.mu needed). Returns an error only for an infrastructure failure.
func (cat *Catalog) recoverViews() error {
	root := filepath.Join(cat.dir, viewsSubdir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("catalog: reading views dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidTableName(e.Name()) {
			continue
		}
		name := e.Name()
		spec, err := loadViewDef(cat.dir, name)
		if err != nil {
			continue // skip an unreadable/corrupt definition rather than fail open
		}
		backing, err := OpenConfig(cat.tableConfig(""))
		if err != nil {
			return fmt.Errorf("catalog: creating view backing for %q: %w", name, err)
		}
		v := newView(spec, backing)
		base, ok := cat.tables[spec.BaseTable]
		if !ok {
			v.state = ViewStale
			v.failErr = fmt.Errorf("base table %q does not exist", spec.BaseTable)
			cat.views[name] = v
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		v.cancel = cancel
		ready := make(chan error, 1)
		go v.run(ctx, base, ready)
		if berr := <-ready; berr != nil {
			// Rebuild failed (e.g. cardinality). Do not fail catalog open; the run
			// goroutine has already marked the view failed and returned; stop it (cancel +
			// close backing) and register the failed view so the operator can see it.
			v.stop()
			v.state, v.failErr = ViewFailed, berr
		}
		cat.views[name] = v
	}
	return nil
}
