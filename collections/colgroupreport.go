package collections

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Reporting and checkpointing the derived group schemas.
//
// Phase 1 answers one question before any format commits to them: are these groups a property of
// the DATA, or of the sample? A group is worth a schema pointer only if the same set of attributes
// keeps co-occurring -- across the segments of a table, and across derivations days apart. So the
// derivation is checkpointed with its timestamp and counts, and a separate check re-derives per
// sealed segment and reports how much the segments agree with the whole.
//
// Nothing here is consulted by a read. The file is diagnostics that survive a restart, so drift is
// visible over the days it would take to appear.

const (
	groupSchemaFile    = "groupschemas.json"
	groupSchemaVersion = 1
	// groupHistoryMax bounds the retained derivations. Enough to see a trend over a week of
	// maintenance passes without the file growing without limit.
	groupHistoryMax = 16
)

// persistedGroupSchemas is the on-disk record: the current derivation plus a bounded history, so
// a drift comparison does not depend on someone having captured the earlier report.
type persistedGroupSchemas struct {
	Version int                   `json:"version"`
	History []persistedGroupDeriv `json:"history"`
}

type persistedGroupDeriv struct {
	Unix       int64              `json:"unix"`
	Sampled    int                `json:"sampled"`
	BaseFields int                `json:"baseFields"`
	BaseCells  int                `json:"baseCells"`
	TotalCells int                `json:"totalCells"`
	Groups     []GroupSchemaEntry `json:"groups"`
}

// GroupSchemas derives group schemas from a fresh sample and returns the report. It does not
// change how anything is stored or read; see deriveGroupSchemas.
//
// k <= 0 uses defaultGroupSchemas. The report is also checkpointed to the collection's directory
// when it has one, so a later call can show what moved.
func (c *Collection) GroupSchemas(sampleMax, k int) GroupSchemaInfo {
	if k <= 0 {
		k = defaultGroupSchemas
	}
	if sampleMax <= 0 {
		sampleMax = maxDistinctSample
	}
	samples := c.CollectSamplesRecentN(sampleMax)
	if len(samples) == 0 {
		return GroupSchemaInfo{}
	}
	// buildAdSchema and the group derivation both read the id-keyed wire form; a persistent
	// (inline) collection stores names, so canonicalize first -- otherwise every ad looks
	// empty and the report is silently all zeroes.
	samples = c.normalizeSamples(samples)
	if len(samples) == 0 {
		return GroupSchemaInfo{}
	}
	// Derive against the LIVE base schema when the accelerator is on, so the groups describe
	// what the base actually leaves uncovered rather than what a fresh derivation would.
	var base *adSchema
	if st := c.schemaScan.Load(); st != nil {
		base = st.schema
	} else {
		base = buildAdSchema(samples, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	}
	groups := c.deriveGroupSchemas(samples, base, k)

	info := GroupSchemaInfo{Sampled: len(samples), BaseFields: len(base.fields)}
	for _, w := range samples {
		wire.Ad(w).ForEach(func(id uint32, _ []byte) bool {
			info.TotalCells++
			if base.hasField(id) {
				info.BaseCells++
			}
			return true
		})
	}
	n := float64(len(samples))
	for _, g := range groups {
		e := GroupSchemaEntry{
			InFrac:      float64(g.in) / n,
			NoneFrac:    float64(g.none) / n,
			PartialFrac: float64(g.partial) / n,
			Cells:       g.cells,
		}
		if info.TotalCells > 0 {
			e.CellsFrac = float64(g.cells) / float64(info.TotalCells)
		}
		for _, id := range g.ids {
			if name, ok := c.schemaFieldName(id); ok {
				e.Attrs = append(e.Attrs, name)
			}
		}
		sort.Strings(e.Attrs)
		info.Groups = append(info.Groups, e)
	}
	c.saveGroupSchemas(info)
	return info
}

// saveGroupSchemas appends a derivation to the checkpoint, keeping the last groupHistoryMax.
// Best-effort, as for the other sidecar metadata: losing a diagnostic costs a comparison, not
// correctness.
func (c *Collection) saveGroupSchemas(info GroupSchemaInfo) {
	if c.dir == "" {
		return
	}
	rec := persistedGroupSchemas{Version: groupSchemaVersion}
	if data, err := os.ReadFile(filepath.Join(c.dir, groupSchemaFile)); err == nil {
		var got persistedGroupSchemas
		if json.Unmarshal(data, &got) == nil && got.Version == groupSchemaVersion {
			rec = got
		}
	}
	rec.History = append(rec.History, persistedGroupDeriv{
		Unix: time.Now().Unix(), Sampled: info.Sampled, BaseFields: info.BaseFields,
		BaseCells: info.BaseCells, TotalCells: info.TotalCells, Groups: info.Groups,
	})
	if len(rec.History) > groupHistoryMax {
		rec.History = rec.History[len(rec.History)-groupHistoryMax:]
	}
	if data, err := json.Marshal(rec); err == nil {
		_ = writeFileSync(filepath.Join(c.dir, groupSchemaFile), data)
	}
}

// GroupSchemaDrift compares the earliest and latest retained derivations, so an operator can see
// whether the groups are holding without having captured the earlier report themselves.
type GroupSchemaDrift struct {
	Derivations int   `json:"derivations"`
	FirstUnix   int64 `json:"firstUnix,omitempty"`
	LastUnix    int64 `json:"lastUnix,omitempty"`
	// Retained is how many of the FIRST derivation's groups still appear, by exact member
	// set, in the last -- the number that matters, because a group whose membership changed
	// is a different group and its block would have to be rebuilt.
	Retained int `json:"retained"`
	OfFirst  int `json:"ofFirst"`
	// MaxPartialFrac is the worst partial fraction across the last derivation's groups.
	// Nonzero means co-occurrence has decayed: some ad now holds part of a group.
	MaxPartialFrac float64 `json:"maxPartialFrac"`
}

// GroupSchemaDrift reads the checkpoint and reports what moved between the first and last
// retained derivations.
func (c *Collection) GroupSchemaDrift() GroupSchemaDrift {
	var out GroupSchemaDrift
	if c.dir == "" {
		return out
	}
	data, err := os.ReadFile(filepath.Join(c.dir, groupSchemaFile))
	if err != nil {
		return out
	}
	var rec persistedGroupSchemas
	if json.Unmarshal(data, &rec) != nil || rec.Version != groupSchemaVersion || len(rec.History) == 0 {
		return out
	}
	out.Derivations = len(rec.History)
	first, last := rec.History[0], rec.History[len(rec.History)-1]
	out.FirstUnix, out.LastUnix = first.Unix, last.Unix
	key := func(e GroupSchemaEntry) string {
		s := ""
		for _, a := range e.Attrs {
			s += a + "\x00"
		}
		return s
	}
	have := map[string]bool{}
	for _, g := range last.Groups {
		have[key(g)] = true
		if g.PartialFrac > out.MaxPartialFrac {
			out.MaxPartialFrac = g.PartialFrac
		}
	}
	out.OfFirst = len(first.Groups)
	for _, g := range first.Groups {
		if have[key(g)] {
			out.Retained++
		}
	}
	return out
}

// GroupSchemaAgreement reports how well per-segment derivations agree with a whole-table one --
// the other half of "is this a property of the data". A group derived from the table but absent
// from most segments is a sampling artifact, and a schema pointer spent on it would buy coverage
// in some segments and nothing in others.
type GroupSchemaAgreement struct {
	Segments int `json:"segments"`
	// PerGroup[i] is the fraction of segments whose own derivation produced group i's exact
	// member set, for the table-level groups in report order.
	PerGroup []float64 `json:"perGroup,omitempty"`
}

// GroupSchemaAgreement derives groups from each sealed segment independently and reports how often
// each table-level group reappears. Diagnostic only, and O(segments x sample) -- for an operator
// deciding whether to commit to a group, not for a maintenance pass.
func (c *Collection) GroupSchemaAgreement(sampleMax, k int) GroupSchemaAgreement {
	if k <= 0 {
		k = defaultGroupSchemas
	}
	table := c.GroupSchemas(sampleMax, k)
	if len(table.Groups) == 0 {
		return GroupSchemaAgreement{}
	}
	key := func(attrs []string) string {
		s := ""
		for _, a := range attrs {
			s += a + "\x00"
		}
		return s
	}
	wanted := make([]string, len(table.Groups))
	for i, g := range table.Groups {
		wanted[i] = key(g.Attrs)
	}
	hits := make([]int, len(wanted))
	segs := 0
	for _, sh := range c.shards {
		// snapshot() PINS each segment it returns a window over; reading seg.data without
		// that pin races a merge or compaction unmapping it underneath us.
		_, wins := sh.snapshot()
		for _, w := range wins {
			samples := c.normalizeSamples(c.windowSamples(w, sampleMax))
			if len(samples) == 0 {
				continue
			}
			segs++
			base := buildAdSchema(samples, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
			found := map[string]bool{}
			for _, g := range c.deriveGroupSchemas(samples, base, k) {
				var attrs []string
				for _, id := range g.ids {
					if name, ok := c.schemaFieldName(id); ok {
						attrs = append(attrs, name)
					}
				}
				sort.Strings(attrs)
				found[key(attrs)] = true
			}
			for i, k := range wanted {
				if found[k] {
					hits[i]++
				}
			}
		}
		releaseWindows(wins)
	}
	out := GroupSchemaAgreement{Segments: segs}
	if segs == 0 {
		return out
	}
	out.PerGroup = make([]float64, len(hits))
	for i, h := range hits {
		out.PerGroup[i] = float64(h) / float64(segs)
	}
	return out
}

// windowSamples draws up to max records from one pinned segment window, in the same wire
// convention CollectSamplesRecentN produces (inline-flattened for a persistent collection), so
// the caller normalizes both the same way.
func (c *Collection) windowSamples(w segWindow, max int) [][]byte {
	var out [][]byte
	var buf []byte
	dict := w.dict()
	for off := 0; off < w.used && len(out) < max; {
		o := uint32(off)
		rl := recTotalLen(w.data, o)
		if rl == 0 {
			break
		}
		off += int(rl)
		if recIsMarker(w.data, o) {
			continue
		}
		raw, err := w.codec.Decompress(buf[:0], recAd(w.data, o))
		if err != nil {
			continue
		}
		buf = raw
		iw := c.wireToInline(dict, raw)
		out = append(out, append([]byte(nil), iw...))
	}
	return out
}

// normalizeSamples canonicalizes sampled records to the id-keyed wire form the schema
// derivation reads. A persistent collection stores names inline, so without this every ad
// looks empty and a report is silently all zeroes.
func (c *Collection) normalizeSamples(samples [][]byte) [][]byte {
	if !c.inline {
		return samples
	}
	out := make([][]byte, 0, len(samples))
	for _, w := range samples {
		if iw, ok := c.recordToInterned(nil, w); ok {
			out = append(out, iw)
		}
	}
	return out
}

// retainedGroupSets returns the member sets of each of the last `runs` retained derivations, or
// ok=false when fewer than that many are retained -- a group nobody has watched twice is not
// established, and the safe answer is to build none.
func (c *Collection) retainedGroupSets(runs int) (hist [][][]string, ok bool) {
	if runs <= 1 {
		return nil, true // gate off
	}
	if c.dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(c.dir, groupSchemaFile))
	if err != nil {
		return nil, false
	}
	var rec persistedGroupSchemas
	if json.Unmarshal(data, &rec) != nil || rec.Version != groupSchemaVersion || len(rec.History) < runs {
		return nil, false
	}
	for _, d := range rec.History[len(rec.History)-runs:] {
		var sets [][]string
		for _, g := range d.Groups {
			sets = append(sets, g.Attrs)
		}
		hist = append(hist, sets)
	}
	return hist, true
}

// groupRecurs reports whether a candidate's members keep showing up together across the retained
// derivations -- the gate on committing storage to a group.
//
// Matched by OVERLAP, not by an identical member list. Identity is too strict, and measurably so:
// across three snapshots of a production queue, 3 of 4 exactly-derived groups reproduced their
// member set but 0 of 4 widened ones did, because widening perturbs membership with the sample. An
// identity gate therefore rejected every widened group -- including one holding 16.7% of the
// table's attribute occurrences at a 0.1% partial rate -- to avoid a drifted one holding 0.68%.
//
// What matters is that the same STRUCTURE keeps appearing, so a half-shared member set counts. At
// that threshold the measured groups sort correctly: the two that kept paying matched at 0.94 and
// 0.87, and the one whose members stopped co-occurring matched nothing at all.
func groupRecurs(cand []string, hist [][][]string, minOverlap float64) bool {
	if len(hist) == 0 {
		return true // gate off
	}
	want := make(map[string]bool, len(cand))
	for _, a := range cand {
		want[strings.ToLower(a)] = true
	}
	for _, deriv := range hist {
		found := false
		for _, set := range deriv {
			inter := 0
			seen := make(map[string]bool, len(set))
			for _, a := range set {
				la := strings.ToLower(a)
				if seen[la] {
					continue
				}
				seen[la] = true
				if want[la] {
					inter++
				}
			}
			union := len(want) + len(seen) - inter
			if union > 0 && float64(inter)/float64(union) >= minOverlap {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
