package collections

import (
	"slices"
	"strings"
)

// The required-attribute set: attributes the schema derivation admits as fixed-slot fields
// whatever their presence rate.
//
// Membership in the base schema is decided by presence alone (adSchemaOpts.Presence), and that
// is the right default -- a slot costs bytes in every record, including the ones that lack the
// attribute, so a rare attribute would tax the common case. But presence and demand are
// different facts, and only presence is consulted: the demand ranking that picks the hot tier
// chooses among fields ALREADY in the schema, so it can widen a field or keep it uncompressed
// and can never admit one. An attribute written only once a record reaches some state therefore
// sits permanently outside the schema no matter how hard the workload reads it.
//
// This is the override, and it is nameless on purpose. Which attributes are worth a slot they
// have not earned by presence is a fact about a particular data set's lifecycle; nothing here
// knows what the records are. The use case supplies the names.
//
// Two tiers, kept apart for the same reason auto-created indexes are kept apart from human ones:
// a name that came back from a restart should say where it came from. The CONFIGURED tier is
// whatever the embedding application re-asserts from its own configuration on every open; the
// RUNTIME tier is what an operator added by hand against a live table, and it is the tier worth
// persisting, because nothing else would bring it back.

// schemaRequireSet is the two-tier required-attribute set. Names are stored folded; the derivation
// resolves them to interned ids, which fold-match by construction.
type schemaRequireSet struct {
	configured []string
	runtime    []string
}

// effective is the union of both tiers, sorted and deduplicated.
func (s *schemaRequireSet) effective() []string {
	out := append(append([]string(nil), s.configured...), s.runtime...)
	slices.Sort(out)
	return slices.Compact(out)
}

// foldUnique folds, sorts and deduplicates names, dropping empties.
func foldUnique(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SchemaRequireAttrs returns the effective required-attribute set: the attributes that enter the
// derived schema regardless of how often they appear. Folded, sorted, deduplicated.
func (c *Collection) SchemaRequireAttrs() []string {
	c.requireMu.Lock()
	defer c.requireMu.Unlock()
	return c.require.effective()
}

// SchemaRequireRuntimeAttrs returns only the entries added at runtime (AddSchemaRequireAttrs),
// which is the tier an embedding application persists -- the configured tier comes back from its
// own configuration on the next open, and persisting it too would freeze a config change out.
func (c *Collection) SchemaRequireRuntimeAttrs() []string {
	c.requireMu.Lock()
	defer c.requireMu.Unlock()
	return append([]string(nil), c.require.runtime...)
}

// SetSchemaRequireAttrs replaces the CONFIGURED tier, leaving anything added at runtime in place.
// It reports whether the effective set changed.
//
// A true return means the committed schema may no longer be the one this set produces. Nothing here
// acts on it -- a configuration reload must not start a rewrite of the table by itself. The next
// routine maintenance review (BuildAndEnableSchemaScan) re-derives the schema, adopts it when a
// required attribute the derivation admits is not yet a field, and converges the sealed segments
// onto it a budget's worth per pass. ReschemaScan adopts immediately.
func (c *Collection) SetSchemaRequireAttrs(names []string) bool {
	c.requireMu.Lock()
	defer c.requireMu.Unlock()
	before := c.require.effective()
	c.require.configured = foldUnique(names)
	return !slices.Equal(before, c.require.effective())
}

// AddSchemaRequireAttrs adds to the RUNTIME tier. Reports whether the effective set changed; see
// SetSchemaRequireAttrs on what a change implies.
func (c *Collection) AddSchemaRequireAttrs(names ...string) bool {
	c.requireMu.Lock()
	defer c.requireMu.Unlock()
	before := c.require.effective()
	c.require.runtime = foldUnique(append(append([]string(nil), c.require.runtime...), names...))
	return !slices.Equal(before, c.require.effective())
}

// DropSchemaRequireAttrs removes names from BOTH tiers. Reports whether the effective set changed.
//
// Dropping one the application configured is effective for this process only: the next open
// re-asserts the configured tier, and the attribute is required again. That is the honest
// outcome -- the configuration still says to require it -- and the way to make it stick is to
// change the configuration.
func (c *Collection) DropSchemaRequireAttrs(names ...string) bool {
	drop := foldUnique(names)
	keep := func(n string) bool { return !slices.Contains(drop, n) }
	c.requireMu.Lock()
	defer c.requireMu.Unlock()
	before := c.require.effective()
	c.require.configured = slices.DeleteFunc(c.require.configured, func(n string) bool { return !keep(n) })
	c.require.runtime = slices.DeleteFunc(c.require.runtime, func(n string) bool { return !keep(n) })
	return !slices.Equal(before, c.require.effective())
}

// requireIDs resolves the effective set to interned ids for the derivation, together with the
// id->name map the diagnostics need. Resolution interns a name that has never been seen, so an
// attribute absent from the data still has an id to be reported absent UNDER -- otherwise the
// one case most worth reporting (a misspelled name) would be the one case with nothing to report.
func (c *Collection) requireIDs() (map[uint32]struct{}, map[uint32]string) {
	names := c.SchemaRequireAttrs()
	if len(names) == 0 || c.intern == nil {
		return nil, nil
	}
	ids := make(map[uint32]struct{}, len(names))
	byID := make(map[uint32]string, len(names))
	for _, n := range names {
		id := c.intern.Intern(n)
		ids[id] = struct{}{}
		byID[id] = n
	}
	return ids, byID
}

// baseSchemaOpts is the derivation configuration the BASE schema is built with, carrying the
// collection's required set so every caller that re-derives a base schema agrees with the live
// one. A group derivation that disagreed would propose a required attribute as a group member and
// store it twice.
func (c *Collection) baseSchemaOpts() (adSchemaOpts, map[uint32]string) {
	ids, byID := c.requireIDs()
	opts := adSchemaOpts{Presence: basePresence, Fit: baseFit, Strings: true, Require: ids}
	if cc := newCellComputer(c); cc != nil {
		opts.Computed = cc.computedKind
	}
	if c.sealer != nil {
		opts.Exclude = c.schemaExcludes
	}
	return opts, byID
}

// schemaExcludes reports whether the attribute with the collection-wide id is one the sealing policy
// names, which is never made a column. A sealed value is no column's literal anyway -- it escapes
// whatever slot it lands in -- but a record written before its attribute was sealed holds the value
// in the clear, and a column of those would put them in every block built from then on, outside the
// record the next rewrite seals. A required attribute is excluded too: no configuration makes a
// sealed value columnar.
func (c *Collection) schemaExcludes(id uint32) bool {
	if c.sealer == nil || c.intern == nil {
		return false
	}
	name, ok := c.intern.Name(id)
	return ok && c.shouldEncrypt(name)
}

// basePresence / baseFit are the base schema's presence floor and int-width fit percentile.
const (
	basePresence = 0.90
	baseFit      = 0.95
)

// SchemaRequireSkip is one required attribute the last derivation could not make a field of, and
// why. A required attribute that silently does nothing is indistinguishable from one that is
// present and not helping, so the refusals are reported rather than dropped.
type SchemaRequireSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// schemaRequireReport is the last derivation's refusals, published for diagnostics.
type schemaRequireReport struct{ skipped []SchemaRequireSkip }

// recordRequireSkips publishes the derivation's refusals, naming each id.
func (c *Collection) recordRequireSkips(skips []requireSkip, byID map[uint32]string) {
	rep := &schemaRequireReport{}
	for _, s := range skips {
		name, ok := byID[s.ID]
		if !ok {
			if name, ok = c.schemaFieldName(s.ID); !ok {
				name = "?"
			}
		}
		rep.skipped = append(rep.skipped, SchemaRequireSkip{Name: name, Reason: s.Reason})
	}
	c.requireReport.Store(rep)
}

// SchemaRequireSkipped returns the required attributes the last schema derivation refused.
func (c *Collection) SchemaRequireSkipped() []SchemaRequireSkip {
	rep := c.requireReport.Load()
	if rep == nil {
		return nil
	}
	return append([]SchemaRequireSkip(nil), rep.skipped...)
}
