package collections

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Keeping the BASE schema current during routine maintenance.
//
// The base schema used to be chosen once, at first enable, and held for the life of the table. That
// was deliberate while a segment's columnar block had to match the collection's schema to be read,
// but readers resolve every field against the schema the segment itself was written under, so
// segments of different schemas coexist. What held the schema still was the cost of moving: a
// columnarized segment stores its schema'd attributes ONLY in its payload, so adopting a new schema
// means rewriting it. That is a budgeted rewrite now (convergeStaleSegments), the same one a group
// change already used, so the schema can follow the data.
//
// Following it naively would churn. A fresh derivation differs from the last one in ways that do not
// matter -- an int width chosen from a different sample, an attribute hovering at the presence floor
// -- and every adoption costs a rewrite of the whole table. So a re-derivation is ADOPTED only when
// the difference is material (reviewBaseSchema), and the bar to adopt sits further from the floor
// than the floor itself, so an attribute has to move decisively before anything is rewritten.

// Hysteresis around the presence floor. The derivation admits an attribute at basePresence; the
// review only treats a NEW one as reason enough to adopt once it is comfortably above that, and an
// existing one as reason enough to drop once it is well below. An attribute sitting near the floor
// therefore never triggers an adoption in either direction -- it is picked up or dropped only when
// something else made the rewrite worth paying for.
const (
	schemaPromoteMargin = 0.05
	schemaDemoteMargin  = 0.10
	// schemaEscapeMargin is how much of the sample a re-derived slot must recover, over the current
	// one, before the escapes count as a reason. An escape here is a value PRESENT in the record but
	// not storable in its slot (wrong kind, or an int too wide for the width), which forces the
	// block's cold-tail walk on every read of that cell. Absence is not counted: no slot fixes it.
	schemaEscapeMargin = 0.10
)

// schemaFitCounts is one schema measured over a sample: per field (by index into s.fields), how many
// sampled ads carry the attribute at all, and how many carry it in a form its slot stores.
type schemaFitCounts struct {
	n       int
	present []int
	stored  []int
}

// measureSchemaFit counts, for each field of s, the sampled ads carrying the attribute and those
// whose value its slot can store -- the same storability test adSchema.encode applies. SchemaFit
// reports these as rates; the maintenance review compares them between two schemas.
func measureSchemaFit(s *adSchema, samples [][]byte) schemaFitCounts {
	fc := schemaFitCounts{n: len(samples), present: make([]int, len(s.fields)), stored: make([]int, len(s.fields))}
	for _, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			idx, ok := s.byID[id]
			if !ok {
				return true // not a schema field: lives in the cold tail either way
			}
			fc.present[idx]++
			f := &s.fields[idx]
			k, lit := nodeKind(node)
			if k == f.kind && (f.kind != akInt || intFits(lit.Int, f.width, f.unsigned)) && !realEscapes(k, lit.Real) {
				fc.stored[idx]++
			}
			return true
		})
	}
	return fc
}

// rate returns count/n, 0 for an empty sample.
func (fc schemaFitCounts) rate(count int) float64 {
	if fc.n == 0 {
		return 0
	}
	return float64(count) / float64(fc.n)
}

// recoverable is the fraction of the sample whose value is in the record but not in field i's slot.
func (fc schemaFitCounts) recoverable(i int) float64 { return fc.rate(fc.present[i] - fc.stored[i]) }

// reviewBaseSchema re-derives the base schema from samples and decides whether the committed one
// should be replaced by it. It returns the schema to adopt, its hot tier and the reasons, or ok=false
// to keep the committed state.
//
// Material means one of:
//
//   - a REQUIRED attribute the derivation admits is not a field. Requiring an attribute is an explicit
//     request for a column, and before this the request did nothing until someone also ran a rebuild.
//   - an attribute that is not a field is present in at least basePresence+schemaPromoteMargin of the
//     sample.
//   - a field the derivation drops is in its slot in under basePresence-schemaDemoteMargin of the sample
//     (absent, or escaping), so it costs a slot in every record and serves few of them.
//   - a field the re-derived layout stores better by schemaEscapeMargin of the sample -- typically an int
//     whose values outgrew their width, or an attribute whose dominant kind changed.
//
// Anything else -- a width that moved without recovering escapes, an attribute near the floor -- is
// noise that would cost a full rewrite to follow, and is not adopted. When something material IS
// found, the whole re-derivation is adopted, near-floor attributes included: the rewrite is being
// paid for anyway, and the derivation is the best description of the data there is.
func (c *Collection) reviewBaseSchema(st *schemaScanState, samples [][]byte, hotTopN int) (*adSchema, []int, string, bool) {
	if st == nil || st.schema == nil {
		return nil, nil, "", false
	}
	cand, candHot, skipped, ok := c.deriveSchemaFrom(samples, hotTopN)
	if !ok {
		return nil, nil, "", false
	}
	cur := st.schema
	reqIDs, _ := c.requireIDs()
	// A required field the sample happens not to contain is kept as it is. A required attribute is
	// often a rare one (written only once a record reaches some state), and a sample can miss it
	// entirely; without this, the next adoption for any other reason would drop the field and the one
	// after would add it back, rewriting the table each way.
	cand, candHot = keepRequiredAbsent(cur, cand, candHot, skipped)

	curFit, candFit := measureSchemaFit(cur, samples), measureSchemaFit(cand, samples)
	name := func(id uint32) string {
		if n, ok := c.schemaFieldName(id); ok {
			return n
		}
		return fmt.Sprintf("#%d", id)
	}
	var reasons []string
	required := false
	for i, f := range cand.fields {
		if cur.hasField(f.id) {
			continue
		}
		if _, req := reqIDs[f.id]; req {
			required = true
			reasons = append(reasons, fmt.Sprintf("required attribute %s is not a field", name(f.id)))
			continue
		}
		if p := candFit.rate(candFit.present[i]); p >= basePresence+schemaPromoteMargin {
			reasons = append(reasons, fmt.Sprintf("%s present in %.1f%% of sampled ads", name(f.id), p*100))
		}
	}
	for i, f := range cur.fields {
		if j, kept := cand.byID[f.id]; kept {
			if gain := curFit.recoverable(i) - candFit.recoverable(j); gain >= schemaEscapeMargin {
				reasons = append(reasons, fmt.Sprintf("%s escapes its slot in %.1f%% of sampled ads (%.1f%% re-derived)",
					name(f.id), curFit.recoverable(i)*100, candFit.recoverable(j)*100))
			}
			continue
		}
		if _, req := reqIDs[f.id]; req {
			// Still required: dropping it is never itself a reason (see keepRequiredAbsent). If the
			// derivation refused it -- say it is expression-valued now -- it goes when something else
			// makes the rewrite worth it.
			continue
		}
		if r := curFit.rate(curFit.stored[i]); r < basePresence-schemaDemoteMargin {
			reasons = append(reasons, fmt.Sprintf("%s in its slot in only %.1f%% of sampled ads", name(f.id), r*100))
		}
	}
	if len(reasons) == 0 || sameSchemaLayout(cur, cand) {
		return nil, nil, "", false
	}
	// One change at a time. While segments are still being rewritten to the committed schema, a drift
	// signal is not acted on: adopting now would turn the rewrites already done into wasted work and
	// start the backlog over. A required attribute is the exception -- it is an explicit request, and
	// waiting for a long convergence to finish before honoring it is exactly the delay it exists to
	// remove.
	if !required && c.staleSegmentCount(st) > 0 {
		return nil, nil, "", false
	}
	return cand, candHot, strings.Join(reasons, "; "), true
}

// keepRequiredAbsent carries into cand every field of cur that is required but which the derivation
// skipped because the sample did not contain it, re-laying-out cand and remapping its hot indices.
func keepRequiredAbsent(cur, cand *adSchema, hot []int, skipped []requireSkip) (*adSchema, []int) {
	var keep []adField
	for _, sk := range skipped {
		if sk.Reason != requireSkipAbsent || cand.hasField(sk.ID) {
			continue
		}
		if i, ok := cur.byID[sk.ID]; ok {
			keep = append(keep, cur.fields[i])
		}
	}
	if len(keep) == 0 {
		return cand, hot
	}
	hotIDs := make(map[uint32]bool, len(hot))
	for _, i := range hot {
		hotIDs[cand.fields[i].id] = true
	}
	fs := append(append([]adField(nil), cand.fields...), keep...)
	s := layoutSchema(fs)
	var hot2 []int
	for i := range s.fields {
		if hotIDs[s.fields[i].id] {
			hot2 = append(hot2, i)
		}
	}
	return s, hot2
}

// sameSchemaLayout reports whether two schemas lay records out identically: the same fields, kinds,
// widths and signedness. layoutSchema is deterministic in the field set, so equal field lists mean
// equal offsets. This, not pointer identity, is what decides whether a segment is current: a
// segment's payload carries its own decoded copy of the schema it was written under.
func sameSchemaLayout(a, b *adSchema) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || len(a.fields) != len(b.fields) {
		return false
	}
	for i := range a.fields {
		fa, fb := &a.fields[i], &b.fields[i]
		if fa.id != fb.id || fa.kind != fb.kind || fa.width != fb.width || fa.unsigned != fb.unsigned {
			return false
		}
	}
	return true
}

// sameHotSet reports whether two hot tiers name the same fields, each against its own schema.
func sameHotSet(as *adSchema, ah []int, bs *adSchema, bh []int) bool {
	if len(ah) != len(bh) {
		return false
	}
	ids := make(map[uint32]bool, len(ah))
	for _, i := range ah {
		if i < 0 || i >= len(as.fields) {
			return false
		}
		ids[as.fields[i].id] = true
	}
	for _, i := range bh {
		if i < 0 || i >= len(bs.fields) || !ids[bs.fields[i].id] {
			return false
		}
	}
	return true
}

// staleCheck decides whether a segment's columnar payload was built under a different committed
// state than st. Built once per pass so the committed group set is resolved to names once, not per
// segment.
type staleCheck struct {
	c         *Collection
	st        *schemaScanState
	groupKeys map[string]struct{}
}

func (c *Collection) newStaleCheck(st *schemaScanState) *staleCheck {
	return &staleCheck{c: c, st: st, groupKeys: c.groupSetNames(st.groups)}
}

// payloadStale reports whether cs differs from the committed state in base schema, hot tier or group
// set, or is a columnarized payload without a remnant-name list. Any of these is a reason to rebuild:
// the base schema decides which attributes are columns, the hot tier which of them are read without
// decompression, the group set which attributes a group column carries, and the list whether absence
// is answered from the payload or by reading every record.
func (k *staleCheck) payloadStale(cs *colSegment) bool {
	sch := cs.schema()
	if sch == nil {
		return false // nothing columnar to compare; covering it is coverSealedSegments' job
	}
	if !sameSchemaLayout(sch, k.st.schema) || !sameHotSet(sch, cs.hotNum(), k.st.schema, k.st.hot) {
		return true
	}
	if cs.schemaOnly && cs.remnants == nil {
		// A columnarized segment written before payloads listed their remnant names: readable, but a
		// column reader cannot tell absence there without reading every record. Rewriting it adds the
		// list, so it converges like any other stale payload.
		return true
	}
	if len(cs.groups) != len(k.st.groups) {
		return true
	}
	got := k.c.groupSetNames(cs.groups)
	if len(got) != len(k.groupKeys) {
		return true
	}
	for key := range got {
		if _, ok := k.groupKeys[key]; !ok {
			return true
		}
	}
	return false
}

// segStale reports whether seg is a sealed segment whose payload is stale and still worth an attempt:
// one already tried against this exact state (convergeTried) is left alone until the state changes.
func (k *staleCheck) segStale(seg *segment) bool {
	if seg.colDamaged.Load() || seg.convergeTried.Load() == k.st {
		return false
	}
	cs := seg.colblk.Load()
	return cs != nil && k.payloadStale(cs)
}

// staleSegmentCount counts the sealed segments still built under an earlier committed state that
// convergence would rewrite (columnarized) or rebuild (sidecar block).
func (c *Collection) staleSegmentCount(st *schemaScanState) int {
	if st == nil {
		return 0
	}
	k := c.newStaleCheck(st)
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.used > 0 && k.segStale(seg) {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

// schemaReviewInterval is how often maintenance re-derives the base schema when nothing asked it
// to. Drift -- an attribute becoming common, a field outgrowing its width -- moves over hours or
// days, while a re-derivation samples and decodes a few thousand records (~200ms on a
// 150-attribute archive), so paying it on every pass of a minutes-cadence table would cost far
// more than the drift it can catch. A change to the required set is reviewed on the next pass
// regardless. A var so a test can make every pass a review.
var schemaReviewInterval = time.Hour

// schemaReviewMark records the last review: when, and against which required set.
type schemaReviewMark struct {
	at      time.Time
	require []string
}

// schemaReviews counts base-schema re-derivations by maintenance, so a test can tell a pass that
// reviewed from one that only compared.
var schemaReviews atomic.Int64

// schemaReviewDue reports whether this pass should re-derive the base schema: the required set
// changed since the last review, or the last one is schemaReviewInterval old (or never ran in this
// process -- the first pass after a restart or an upgrade always reviews). Caller holds schemaMu.
func (c *Collection) schemaReviewDue(now time.Time, require []string) bool {
	m := &c.reviewed
	return m.at.IsZero() || !slices.Equal(m.require, require) || now.Sub(m.at) >= schemaReviewInterval
}

// refreshSchemaState is the routine maintenance review of the committed schema-scan state: adopt a
// re-derived base schema when the change is material, otherwise review the group set alone. Returns
// with the committed state published; bringing segments current is convergeStaleSegments' job.
func (c *Collection) refreshSchemaState(sampleMax, hotTopN int) {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	st := c.schemaScan.Load()
	if st == nil {
		return
	}
	now, require := time.Now(), c.SchemaRequireAttrs()
	if c.schemaReviewDue(now, require) {
		schemaReviews.Add(1)
		c.reviewed = schemaReviewMark{at: now, require: require}
		if s, hot, reason, ok := c.reviewBaseSchema(st, c.schemaSamples(sampleMax), hotTopN); ok {
			// A new base schema re-derives the group set against it too (installSchemaScanLocked): a
			// group is drawn from the attributes the base does NOT carry, so one the base has just
			// taken in is no longer a group member.
			c.installSchemaScanLocked(s, hot, "maintenance: "+reason)
			return
		}
	}
	if !c.baseCommitted.Load() {
		// A table enabled before the committed schema was recorded separately: record what it is
		// running under now, so the next restart reopens with this rather than with whichever schema
		// its last segment happens to carry. No change event -- nothing changed.
		c.commitBaseSchema(nil, nil, st.schema, st.hot, "")
	}
	c.refreshGroupSchemas(st)
}

// convergeStaleSegments brings segments built under an earlier committed state current, bounded per
// pass: columnarized segments are rewritten (recolumnarizeStale, ColumnarSegmentBudget of them) and
// sidecar blocks rebuilt (rebuildStaleSidecars). Returns how many of each. A steady table finds
// nothing stale and pays a comparison per segment.
//
// Each half has its own allowance rather than sharing one. A table sealing a budget's worth of new
// segments per pass would otherwise spend every pass columnarizing those and never converge the old
// ones.
func (c *Collection) convergeStaleSegments() (rewritten, rebuilt int) {
	rewritten = c.recolumnarizeStale()
	rebuilt = c.rebuildStaleSidecars()
	return rewritten, rebuilt
}

// rebuildStaleSidecars rebuilds the sidecar columnar blocks of non-columnarized sealed segments whose
// block was built under an earlier committed state, dropping the old block for the new one and
// persisting it. These are segments the columnar rewrite does not take: an in-memory collection, one
// with the rewrite disabled, or a segment the rewrite refused. The old block stays readable until the
// new one replaces it -- a reader resolves fields against whichever block it loaded.
//
// Bounded by ColumnarSegmentBudget, or by the default when the rewrite is disabled: a rebuild is a
// transcode beside the segment rather than a rewrite of it, so turning the rewrite off is not a
// reason to leave the accelerator on a schema the table no longer has.
func (c *Collection) rebuildStaleSidecars() int {
	budget := c.columnarBudget()
	if budget <= 0 {
		budget = defaultColumnarBudget
	}
	return c.rebuildStaleSidecarsN(budget)
}

// rebuildStaleSidecarsAll rebuilds every stale sidecar block, unbudgeted: ReschemaScan's synchronous
// half, which has always rebuilt the whole table's sidecar blocks before returning.
func (c *Collection) rebuildStaleSidecarsAll() int { return c.rebuildStaleSidecarsN(math.MaxInt) }

func (c *Collection) rebuildStaleSidecarsN(budget int) int {
	st := c.schemaScan.Load()
	if st == nil || st.schema == nil {
		return 0
	}
	k := c.newStaleCheck(st)
	total := 0
	for _, sh := range c.shards {
		if total >= budget || c.maintStopping() {
			break
		}
		segs := sh.pinSealed(func(seg *segment) bool { return !seg.columnarized() && k.segStale(seg) })
		done := 0
		func() {
			defer func() { unpinAll(segs[done:]) }()
			for i, seg := range segs {
				if total < budget && !c.maintStopping() {
					if cs := c.buildColSegment(seg, st.schema, st.hot); cs != nil {
						seg.colblk.Store(cs)
						c.persistColSeg(sh, seg)
						total++
					}
					if cs := seg.colblk.Load(); cs != nil && k.payloadStale(cs) {
						seg.convergeTried.Store(st)
					}
				}
				seg.unpin()
				done = i + 1
			}
		}()
	}
	return total
}

// --- the committed base schema, on disk ---

// baseSchemaFile records the committed base schema and its change log. The schema is the thing a
// reopen must restore exactly; the change log is the record of why it is what it is.
const (
	baseSchemaFile      = "baseschema.json"
	baseSchemaVersion   = 1
	baseSchemaChangeMax = 64
)

type persistedBaseSchema struct {
	Version int                    `json:"version"`
	Fields  []persistedSchemaField `json:"fields"`
	Hot     []string               `json:"hot,omitempty"`
	Changes []BaseSchemaChange     `json:"changes,omitempty"`
}

// persistedSchemaField is one field, keyed by NAME: global intern ids are renumbered at every Open.
type persistedSchemaField struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Width    int    `json:"width,omitempty"`
	Unsigned bool   `json:"unsigned,omitempty"`
}

// BaseSchemaChange is one adoption of a different base schema: when, why, and what moved. Changed
// lists fields whose slot was re-laid-out, as "Name: kind/width -> kind/width".
type BaseSchemaChange struct {
	Unix    int64    `json:"unix"`
	Reason  string   `json:"reason"`
	Fields  int      `json:"fields"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
	HotAdd  []string `json:"hotAdded,omitempty"`
	HotDrop []string `json:"hotRemoved,omitempty"`
}

// commitBaseSchema writes s and hot as the committed base schema, appending a change event (diffed
// against prev) when reason is non-empty. Durable before it returns: written to a temporary file,
// synced, renamed over the old one and the directory synced. Best-effort on failure -- the in-memory
// state is still adopted, and a reopen then falls back to a segment's schema, which is a valid
// schema to run under, only possibly not the newest.
func (c *Collection) commitBaseSchema(prev *adSchema, prevHot []int, s *adSchema, hot []int, reason string) {
	if c.dir == "" || s == nil {
		return
	}
	rec, _ := c.readBaseSchemaFile()
	rec.Version = baseSchemaVersion
	rec.Fields = rec.Fields[:0]
	for _, f := range s.fields {
		n, _ := c.schemaFieldName(f.id)
		rec.Fields = append(rec.Fields, persistedSchemaField{Name: n, Kind: f.kind.String(), Width: f.width, Unsigned: f.unsigned})
	}
	// In the tier's own order, not sorted: it is handed back to the block builder as is.
	rec.Hot = rec.Hot[:0]
	for _, i := range hot {
		if i >= 0 && i < len(s.fields) {
			if n, ok := c.schemaFieldName(s.fields[i].id); ok {
				rec.Hot = append(rec.Hot, n)
			}
		}
	}
	if reason != "" {
		ev := c.diffBaseSchemas(prev, prevHot, s, hot)
		ev.Reason = reason
		rec.Changes = append(rec.Changes, ev)
		if len(rec.Changes) > baseSchemaChangeMax {
			rec.Changes = rec.Changes[len(rec.Changes)-baseSchemaChangeMax:]
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if writeFileAtomic(filepath.Join(c.dir, baseSchemaFile), data) != nil {
		return
	}
	if syncDir(c.dir) != nil {
		return
	}
	c.baseCommitted.Store(true)
}

// schemaHotNames resolves a hot tier to field names, sorted.
func (c *Collection) schemaHotNames(s *adSchema, hot []int) []string {
	var out []string
	for _, i := range hot {
		if i >= 0 && i < len(s.fields) {
			if n, ok := c.schemaFieldName(s.fields[i].id); ok {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// diffBaseSchemas describes the change from prev to s by field name.
func (c *Collection) diffBaseSchemas(prev *adSchema, prevHot []int, s *adSchema, hot []int) BaseSchemaChange {
	ev := BaseSchemaChange{Unix: time.Now().Unix(), Fields: len(s.fields)}
	desc := func(f adField) string { return fmt.Sprintf("%s/%d", f.kind, f.width) }
	name := func(id uint32) string {
		n, _ := c.schemaFieldName(id)
		return n
	}
	if prev == nil {
		return ev // a first enable: the schema itself is the record, a list of every field adds nothing
	}
	for _, f := range s.fields {
		i, ok := prev.byID[f.id]
		if !ok {
			ev.Added = append(ev.Added, name(f.id))
			continue
		}
		if p := prev.fields[i]; p.kind != f.kind || p.width != f.width || p.unsigned != f.unsigned {
			ev.Changed = append(ev.Changed, fmt.Sprintf("%s: %s -> %s", name(f.id), desc(p), desc(f)))
		}
	}
	for _, f := range prev.fields {
		if !s.hasField(f.id) {
			ev.Removed = append(ev.Removed, name(f.id))
		}
	}
	ev.HotAdd, ev.HotDrop = strDiff(c.schemaHotNames(prev, prevHot), c.schemaHotNames(s, hot))
	sort.Strings(ev.Added)
	sort.Strings(ev.Removed)
	sort.Strings(ev.Changed)
	return ev
}

// readBaseSchemaFile reads the committed-schema record; ok=false when absent or unreadable.
func (c *Collection) readBaseSchemaFile() (persistedBaseSchema, bool) {
	var rec persistedBaseSchema
	if c.dir == "" {
		return rec, false
	}
	data, err := os.ReadFile(filepath.Join(c.dir, baseSchemaFile))
	if err != nil {
		return persistedBaseSchema{}, false
	}
	if json.Unmarshal(data, &rec) != nil || rec.Version != baseSchemaVersion {
		return persistedBaseSchema{}, false
	}
	return rec, true
}

// loadCommittedBaseSchema decodes the committed base schema, binding each field to this process's
// intern id. ok=false for anything that does not decode to a valid layout, so a damaged record falls
// back to recovering the schema from a segment rather than running under a wrong one.
func (c *Collection) loadCommittedBaseSchema() (*adSchema, []int, bool) {
	if c.intern == nil {
		return nil, nil, false
	}
	rec, ok := c.readBaseSchemaFile()
	if !ok || len(rec.Fields) == 0 || len(rec.Fields) > adSchemaMaxFields {
		return nil, nil, false
	}
	// Validate everything before interning anything, so a bad record leaves no names behind.
	for _, pf := range rec.Fields {
		if pf.Name == "" {
			return nil, nil, false
		}
		k, ok := parseAdKind(pf.Kind)
		if !ok {
			return nil, nil, false
		}
		switch k {
		case akInt:
			if pf.Width != 1 && pf.Width != 2 && pf.Width != 4 && pf.Width != 6 && pf.Width != 8 {
				return nil, nil, false
			}
		case akReal:
			if pf.Width != 8 {
				return nil, nil, false
			}
		default:
			if pf.Width < 0 || pf.Width > 8 || pf.Unsigned {
				return nil, nil, false
			}
		}
	}
	fields := make([]adField, 0, len(rec.Fields))
	seen := make(map[uint32]bool, len(rec.Fields))
	for _, pf := range rec.Fields {
		k, _ := parseAdKind(pf.Kind)
		id := c.intern.Intern(pf.Name)
		if seen[id] {
			return nil, nil, false // two entries for one attribute: not a schema this store wrote
		}
		seen[id] = true
		fields = append(fields, adField{id: id, kind: k, width: pf.Width, unsigned: pf.Unsigned})
	}
	s := layoutSchema(fields)
	var hot []int
	for _, n := range rec.Hot {
		id, ok := c.intern.LookupID(n)
		if !ok {
			continue
		}
		if i, ok := s.byID[id]; ok && numericKind(s.fields[i].kind) && !slices.Contains(hot, i) {
			hot = append(hot, i)
		}
	}
	return s, hot, true
}

// parseAdKind is adKind.String inverted, for the stored kinds only.
func parseAdKind(s string) (adKind, bool) {
	switch s {
	case "bool":
		return akBool, true
	case "int":
		return akInt, true
	case "real":
		return akReal, true
	case "string":
		return akString, true
	}
	return akNone, false
}

// BaseSchemaChanges returns the base-schema change log, oldest first: every adoption of a different
// base schema, with its reason and diff. Read from disk only, so it is available to a READ-level client.
func (c *Collection) BaseSchemaChanges() []BaseSchemaChange {
	rec, ok := c.readBaseSchemaFile()
	if !ok {
		return nil
	}
	return rec.Changes
}
