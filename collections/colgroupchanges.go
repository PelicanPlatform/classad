package collections

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The committed-group CHANGE LOG. The derivation history (colgroupreport.go) records the sampler's
// candidate groups run to run -- which wobble by nature, so counting derivations overstates churn.
// This records the changes that actually MATTER: the moments the accelerator adopted a different
// committed set (each of which rebuilds a columnar block per affected segment). Each entry carries
// the diff -- old groups matched to new by best member overlap, so a one-attribute shift reads as a
// Changed entry (attrs added/removed) rather than an unrelated drop-plus-add -- and a reason,
// including the promoted groups' coverage from the latest derivation. That is what lets a reader see
// whether a change was cosmetic or structural, and whether the groups are earning their keep.

// groupChangeMax bounds the retained change events. Larger than the derivation history because a
// change is rare (a stable table logs none), so keeping more spans a long operating window cheaply.
const groupChangeMax = 64

// GroupSchemaChange is one committed-group-set change, for reporting.
type GroupSchemaChange struct {
	Unix    int64              `json:"unix"`
	Reason  string             `json:"reason"`
	Added   [][]string         `json:"added,omitempty"`
	Removed [][]string         `json:"removed,omitempty"`
	Changed []GroupSchemaDelta `json:"changed,omitempty"`
}

// GroupSchemaDelta is one surviving group whose membership shifted across a change.
type GroupSchemaDelta struct {
	Before  []string `json:"before"`
	After   []string `json:"after"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// GroupSchemaLastAgreement is the last per-segment agreement result, read from the sidecar so a
// READ-level client can see it without recomputing (see GroupSchemaAgreement).
type GroupSchemaLastAgreement struct {
	Unix     int64                `json:"unix"`
	Segments int                  `json:"segments"`
	Groups   []GroupAgreementItem `json:"groups,omitempty"`
}

// GroupAgreementItem pairs a group's members with the fraction of segments that re-derived it.
type GroupAgreementItem struct {
	Attrs []string `json:"attrs"`
	Frac  float64  `json:"frac"`
}

// recordGroupChange appends a committed-group change event when newGroups differs from oldGroups.
// No-op when unchanged or the collection has no directory. Called at the two points the committed
// set is (re)published: a first/rebuild enable (installSchemaScan) and the routine auto-promote
// (refreshGroupSchemas).
func (c *Collection) recordGroupChange(oldGroups, newGroups []*colGroup, context string) {
	if c.dir == "" {
		return
	}
	ev, changed := diffGroupSets(c.groupAttrLists(oldGroups), c.groupAttrLists(newGroups))
	if !changed {
		return
	}
	c.mutateGroupSchemaFile(func(rec *persistedGroupSchemas) {
		ev.Reason = explainGroupChange(context, ev, rec.lastDeriv())
		rec.Changes = append(rec.Changes, ev)
		if len(rec.Changes) > groupChangeMax {
			rec.Changes = rec.Changes[len(rec.Changes)-groupChangeMax:]
		}
	})
}

// groupAttrLists resolves each group's member ids to sorted attribute-name lists.
func (c *Collection) groupAttrLists(gs []*colGroup) [][]string {
	out := make([][]string, 0, len(gs))
	for _, g := range gs {
		var attrs []string
		for _, id := range g.ids {
			if name, ok := c.schemaFieldName(id); ok {
				attrs = append(attrs, name)
			}
		}
		sort.Strings(attrs)
		out = append(out, attrs)
	}
	return out
}

// diffGroupSets matches old groups to new by best member overlap (Jaccard >= 0.5) so a small
// membership shift is a Changed entry rather than an unrelated drop-plus-add; unmatched old groups
// are Removed and unmatched new ones Added. Attr lists are assumed sorted (groupAttrLists).
func diffGroupSets(old, nw [][]string) (persistedGroupChange, bool) {
	ev := persistedGroupChange{Unix: time.Now().Unix()}
	usedNew := make([]bool, len(nw))
	for _, o := range old {
		best, bestJac := -1, 0.0
		for j, n := range nw {
			if usedNew[j] {
				continue
			}
			if s := jaccardStrs(o, n); s > bestJac {
				best, bestJac = j, s
			}
		}
		if best >= 0 && bestJac >= 0.5 {
			usedNew[best] = true
			if n := nw[best]; !sameStrs(o, n) {
				add, rem := strDiff(o, n)
				ev.Changed = append(ev.Changed, persistedGroupDelta{Before: o, After: n, Added: add, Removed: rem})
			}
		} else {
			ev.Removed = append(ev.Removed, o)
		}
	}
	for j, n := range nw {
		if !usedNew[j] {
			ev.Added = append(ev.Added, n)
		}
	}
	return ev, len(ev.Added) > 0 || len(ev.Removed) > 0 || len(ev.Changed) > 0
}

// explainGroupChange builds the human reason: the change's shape, plus the promoted groups'
// coverage from the latest derivation (the "why it was worth committing"), when known.
func explainGroupChange(context string, ev persistedGroupChange, last *persistedGroupDeriv) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: +%d group(s), -%d, ~%d changed",
		context, len(ev.Added), len(ev.Removed), len(ev.Changed))
	for _, g := range ev.Added {
		if e := findDerivGroup(last, g); e != nil {
			fmt.Fprintf(&b, "; +{%s} %.1f%% cells, %.2f%% partial",
				strings.Join(g, ","), e.CellsFrac*100, e.PartialFrac*100)
		}
	}
	return b.String()
}

// GroupSchemaChanges returns the committed-group change log, oldest first. Reads the sidecar only --
// no sampling -- so it is available to a READ-level client.
func (c *Collection) GroupSchemaChanges() []GroupSchemaChange {
	rec, ok := c.readGroupSchemaFile()
	if !ok {
		return nil
	}
	out := make([]GroupSchemaChange, 0, len(rec.Changes))
	for _, ch := range rec.Changes {
		gc := GroupSchemaChange{Unix: ch.Unix, Reason: ch.Reason, Added: ch.Added, Removed: ch.Removed}
		for _, d := range ch.Changed {
			gc.Changed = append(gc.Changed, GroupSchemaDelta(d))
		}
		out = append(out, gc)
	}
	return out
}

// GroupSchemaLastAgreement returns the last persisted per-segment agreement, if any.
func (c *Collection) GroupSchemaLastAgreement() (GroupSchemaLastAgreement, bool) {
	rec, ok := c.readGroupSchemaFile()
	if !ok || rec.LastAgreement == nil {
		return GroupSchemaLastAgreement{}, false
	}
	pa := rec.LastAgreement
	out := GroupSchemaLastAgreement{Unix: pa.Unix, Segments: pa.Segments}
	for _, g := range pa.Groups {
		out.Groups = append(out.Groups, GroupAgreementItem{Attrs: g.Attrs, Frac: g.Frac})
	}
	return out, true
}

// readGroupSchemaFile reads and validates the sidecar record.
func (c *Collection) readGroupSchemaFile() (persistedGroupSchemas, bool) {
	var rec persistedGroupSchemas
	if c.dir == "" {
		return rec, false
	}
	data, err := os.ReadFile(filepath.Join(c.dir, groupSchemaFile))
	if err != nil {
		return rec, false
	}
	if json.Unmarshal(data, &rec) != nil || rec.Version != groupSchemaVersion {
		return rec, false
	}
	return rec, true
}

func (r *persistedGroupSchemas) lastDeriv() *persistedGroupDeriv {
	if len(r.History) == 0 {
		return nil
	}
	return &r.History[len(r.History)-1]
}

// findDerivGroup finds the derivation entry whose (sorted) members equal attrs.
func findDerivGroup(d *persistedGroupDeriv, attrs []string) *GroupSchemaEntry {
	if d == nil {
		return nil
	}
	for i := range d.Groups {
		if sameStrs(d.Groups[i].Attrs, attrs) {
			return &d.Groups[i]
		}
	}
	return nil
}

// jaccardStrs is the Jaccard similarity of two sorted string sets.
func jaccardStrs(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	inter := 0
	for _, x := range b {
		if set[x] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func sameStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// strDiff returns the members in after but not before (added) and before but not after (removed).
func strDiff(before, after []string) (added, removed []string) {
	bs := make(map[string]bool, len(before))
	for _, x := range before {
		bs[x] = true
	}
	as := make(map[string]bool, len(after))
	for _, x := range after {
		as[x] = true
	}
	for _, x := range after {
		if !bs[x] {
			added = append(added, x)
		}
	}
	for _, x := range before {
		if !as[x] {
			removed = append(removed, x)
		}
	}
	return added, removed
}
