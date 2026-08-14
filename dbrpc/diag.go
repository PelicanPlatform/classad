package dbrpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// Diagnostics is a snapshot of the store's storage, hot set, indexes, and index
// tuning advice -- the payload of the diagnostic ".stats"/".indexes"/".hot"
// commands.
type Diagnostics struct {
	Stats              db.Stats             `json:"stats"`
	OpStats            db.OpStats           `json:"opStats"`
	Hot                []string             `json:"hot"`
	CategoricalIndexes []string             `json:"categoricalIndexes"`
	ValueIndexes       []string             `json:"valueIndexes"`
	IndexSizes         db.IndexSizes        `json:"indexSizes"`
	Codec              db.CodecStats        `json:"codec"`
	Suggestions        []db.IndexSuggestion `json:"suggestions"`
	DropSuggestions    []db.DropSuggestion  `json:"dropSuggestions"`
	// EncryptionEnabled reports whether encryption at rest is active; EncryptedAttrs is
	// the explicit encrypted-attribute set (private attributes are always encrypted and
	// are not listed here).
	EncryptionEnabled bool     `json:"encryptionEnabled"`
	EncryptedAttrs    []string `json:"encryptedAttrs,omitempty"`

	// SchemaScan reports the per-segment columnar (adschema) accelerator's state: whether it is
	// enabled (a numeric COUNT(*) WHERE takes the columnar fast path), its uncompressed hot
	// columns, and how many sealed segments carry a block. Reported for BOTH table kinds --
	// archives carry columnar blocks too, and reporting it for only one made an archive look
	// like it had no accelerator at all.
	SchemaScan db.SchemaScanInfo `json:"schemaScan"`

	// Archive marks an append-only history table (vs. a mutable one), and Retention carries
	// its rotation bounds -- the one genuinely kind-specific pair, since a mutable table has no
	// rotation. SidecarSizes reports sealed-segment sidecar index bytes, for both kinds.
	Archive      bool            `json:"archive,omitempty"`
	Retention    *db.Retention   `json:"retention,omitempty"`
	SidecarSizes db.SidecarSizes `json:"sidecarSizes"`

	// ZoneAttrs are the archive attributes carrying per-segment [min,max] zone maps: a
	// range query on one of these prunes whole segments, not just postings.
	ZoneAttrs []string `json:"zoneAttrs,omitempty"`
	// SealedSegments is how many segments are sealed to an immutable index sidecar, and
	// StaleIndexSegments how many of those were sealed under an older index configuration --
	// i.e. how much of the table a runtime .addindex/.dropindex has NOT reached yet (only a
	// rewrite reaches them). Both kinds.
	SealedSegments     int `json:"sealedSegments,omitempty"`
	StaleIndexSegments int `json:"staleIndexSegments,omitempty"`
}

// diagSampleMax bounds the ad sample the server takes for index suggestions.
const diagSampleMax = 2000

// defaultSchemaHotTopN is the hot-column count schema.rebuild uses when the server carries no
// configured HotTopN, matching the size a default maintenance pass would choose.
const defaultSchemaHotTopN = 32

// defaultAnalyzeHotTopN is the hot-set size an on-demand "analyze" uses when the server was
// started without a maintenance hot-set target (HotTopN == 0), so analyze still refreshes the
// hot set from read demand.
const defaultAnalyzeHotTopN = 32

// diagJSON gathers a table's diagnostics into JSON.
func (s *Server) diagJSON(t *db.DB) ([]byte, error) {
	cat, val := t.IndexedAttrs()
	d := Diagnostics{
		Stats:              t.Stats(),
		OpStats:            t.OpStats(),
		Hot:                t.HotAttrs(),
		CategoricalIndexes: cat,
		ValueIndexes:       val,
		IndexSizes:         t.IndexSizes(),
		Codec:              t.CodecStats(diagSampleMax),
		Suggestions:        t.SuggestIndexes(diagSampleMax),
		DropSuggestions:    t.SuggestDrops(diagSampleMax),
		EncryptionEnabled:  t.EncryptionEnabled(),
		EncryptedAttrs:     t.EncryptedAttrNames(),
		SchemaScan:         t.SchemaScanInfo(),
		// Reported for a mutable table as well as an archive: it has sidecars too, and without them
		// its .stats could not account for its on-disk footprint the way an archive's could.
		SidecarSizes: t.SidecarSizes(),
	}
	d.StaleIndexSegments, d.SealedSegments = t.StaleIndexSegments()
	return json.Marshal(d)
}

// archiveDiagJSON assembles the diagnostics for an append-only archive (history) table, from
// the same stat surface as a mutable table plus the retention bounds -- so `.stats history`
// reads like `.stats <table>` (storage bytes, segments, codec, op timings) instead of just a
// row count.
func (s *Server) archiveDiagJSON(a *db.ArchiveTable) ([]byte, error) {
	cat, val := a.IndexedAttrs()
	ret := a.Retention()
	stale, sealed := a.StaleIndexSegments()
	d := Diagnostics{
		Stats:              a.Stats(),
		OpStats:            a.OpStats(),
		CategoricalIndexes: cat,
		ValueIndexes:       val,
		IndexSizes:         a.IndexSizes(),
		Codec:              a.CodecStats(diagSampleMax),
		Archive:            true,
		Retention:          &ret,
		SidecarSizes:       a.SidecarSizes(),
		ZoneAttrs:          a.ZoneAttrs(),
		SealedSegments:     sealed,
		StaleIndexSegments: stale,
		SchemaScan:         a.SchemaScanInfo(),
		// The same three a mutable table reports. Encryption in particular is reported rather than
		// left zero: an archive is NOT sealed today (its open path passes no data key), and a missing
		// line reads as "not applicable" when the truth is "not protected".
		Hot:               a.HotAttrs(),
		EncryptionEnabled: a.EncryptionEnabled(),
		EncryptedAttrs:    a.EncryptedAttrNames(),
	}
	return json.Marshal(d)
}

// admin runs a management action. Actions:
//
//	index.add.categorical <attr>...   add categorical (string eq/membership) indexes
//	index.add.value <attr>...         add value (numeric + range) indexes
//	index.drop <attr>...              drop indexes on the given attributes
//	index.reindex                     rebuild all indexes from live ads (archives: rebuild
//	                                  stale segment sidecars in place, no data rewrite)
//	hot.add <attr>...                 pin attributes into the hot set
//	hot.refresh <sampleMax> <topN>    recompute the hot set from sampled frequency
//	schema.fit [sampleMax]            report how well the derived schema still fits the data
//	schema.rebuild [sampleMax topN]   re-derive the schema and rebuild every columnar block
//	                                  (both also available on an archive table)
//	compact                           reclaim dead space in warranted shards
//	rewrite                           re-encode all ads with the current hot set
//	codec.retrain [sampleMax]         train/refresh the ZSTD dictionary + recompress
//	encrypt.set <attr>...             set the explicit encrypted-at-rest attributes
//	                                  (DAEMON-only; private attrs always encrypted)
//	truncate                          remove every ad (DAEMON-only, DB-wide locked)
//	backup.key                        export the backup key, hex (DAEMON-only escrow key)
func (s *Server) admin(t *db.DB, action string, args []string, privileged bool) (string, error) {
	// Every admin action mutates storage policy or physical layout -- indexes, the hot
	// set, the codec dictionary, compaction, encryption policy, truncation. These are
	// administrative operations in HTCondor's model, so the whole table is DAEMON-gated:
	// an ordinary WRITE-level session may read and write ads but must not retune or
	// restructure the store. (dispatch already blocks opAdmin on READ-level connections;
	// this additionally blocks WRITE-level ones. Read-only diagnostics use opDiag and are
	// unaffected.) Authorize before touching args so action existence is not probed.
	if !privileged {
		return "", fmt.Errorf("admin action %q requires DAEMON authorization", action)
	}
	switch action {
	case "encrypt.set":
		// Changing which attributes are encrypted at rest is a security-policy change.
		// args is the new explicit set (private attributes are always encrypted
		// regardless). An empty args clears the explicit set.
		if err := t.SetEncryptedAttrs(args); err != nil {
			return "", err
		}
		return "encrypted attributes: " + join(t.EncryptedAttrNames()), nil
	case "timetravel.enable":
		// Enable/retune point-in-time queries: args = <maxDistanceSeconds>
		// [checkpointSeconds]. Persisted so it survives a restart.
		if len(args) < 1 {
			return "", fmt.Errorf("timetravel.enable needs <maxDistanceSeconds> [checkpointSeconds]")
		}
		maxS, err := strconv.Atoi(args[0])
		if err != nil || maxS <= 0 {
			return "", fmt.Errorf("timetravel.enable: bad maxDistanceSeconds %q", args[0])
		}
		ckpt := 0
		if len(args) > 1 {
			if ckpt, err = strconv.Atoi(args[1]); err != nil || ckpt < 0 {
				return "", fmt.Errorf("timetravel.enable: bad checkpointSeconds %q", args[1])
			}
		}
		t.SetTimeTravel(time.Duration(maxS)*time.Second, time.Duration(ckpt)*time.Second)
		md, cp, _ := t.TimeTravel()
		return fmt.Sprintf("time travel enabled: window %s, checkpoint %s", md, cp), nil
	case "timetravel.disable":
		t.SetTimeTravel(0, 0)
		return "time travel disabled", nil
	case "truncate":
		// Removing every ad is a destructive, DB-wide-locked operation.
		t.Truncate()
		return "database truncated", nil
	case "backup.key":
		// Export the backup key (hex) so an operator can escrow it and decrypt/restore
		// encrypted snapshots without the pool keys: a secret that opens every backup. It
		// is NOT the live-data key and cannot read the store.
		k := t.BackupKey()
		if k == nil {
			return "", fmt.Errorf("encryption at rest is not enabled")
		}
		return hex.EncodeToString(k), nil
	case "index.add.categorical":
		if len(args) == 0 {
			return "", fmt.Errorf("index.add.categorical needs at least one attribute")
		}
		return addIndex(t, "categorical index on "+join(args), args, nil), nil
	case "index.add.value":
		if len(args) == 0 {
			return "", fmt.Errorf("index.add.value needs at least one attribute")
		}
		return addIndex(t, "value index on "+join(args), nil, args), nil
	case "index.drop":
		if len(args) == 0 {
			return "", fmt.Errorf("index.drop needs at least one attribute")
		}
		changed := t.DropIndex(args...)
		if changed {
			t.Reindex() // rebuild segment indexes so the dropped postings are reclaimed
		}
		return changedMsg("dropped index on "+join(args), changed), nil
	case "index.reindex":
		t.Reindex()
		return "reindexed", nil
	case "compact":
		n := t.Compact()
		return fmt.Sprintf("compacted %d shard(s)", n), nil
	case "rewrite":
		n := t.Rewrite()
		return fmt.Sprintf("rewrote %d ad(s) with the current hot set and compacted", n), nil
	case "codec.retrain":
		sampleMax := diagSampleMax
		if len(args) == 1 {
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
				sampleMax = v
			}
		}
		dictBytes, err := t.RetrainDict(sampleMax)
		if err != nil {
			return "", fmt.Errorf("retrain: %w", err)
		}
		return fmt.Sprintf("retrained ZSTD dictionary (%d bytes) and recompressed existing ads", dictBytes), nil
	case "hot.add":
		if len(args) == 0 {
			return "", fmt.Errorf("hot.add needs at least one attribute")
		}
		hot := t.AddHotAttrs(args...)
		return "hot attributes: " + join(hot), nil
	case "hot.refresh":
		if len(args) != 2 {
			return "", fmt.Errorf("hot.refresh needs <sampleMax> <topN>")
		}
		sampleMax, e1 := strconv.Atoi(args[0])
		topN, e2 := strconv.Atoi(args[1])
		if e1 != nil || e2 != nil {
			return "", fmt.Errorf("hot.refresh arguments must be integers")
		}
		n := t.RefreshHotSet(sampleMax, topN)
		return fmt.Sprintf("refreshed hot set: %d attribute(s)", n), nil
	case "schema.fit":
		return schemaFitAction(t, args)
	case "schema.groups":
		return schemaGroupsAction(t, args)
	case "schema.groups.agree":
		return schemaGroupsAgreeAction(t, args)
	case "schema.rebuild":
		return schemaRebuildAction(t, args, s.maintainOpts.HotTopN)
	case "analyze":
		// On-demand self-tuning pass ("optimize now"), the manual counterpart to the scheduled
		// StartMaintenance. Reuses the server's configured maintenance options so it matches the
		// scheduled pass, but never does the heavy dictionary retrain (that recompacts -- it is
		// rewrite/codec.retrain's job). Refreshes the hot set from accumulated read demand and,
		// when auto-index is configured, retunes indexes; then reindexes unconditionally so value
		// histograms and any index/hot changes take effect even when auto-index is off.
		opts := s.maintainOpts
		opts.Retrain = false
		if opts.SampleMax <= 0 {
			opts.SampleMax = diagSampleMax
		}
		if opts.HotTopN <= 0 {
			opts.HotTopN = defaultAnalyzeHotTopN
		}
		if len(args) == 1 { // optional hot-set size override: analyze <topN>
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
				opts.HotTopN = v
			}
		}
		s.maintainMu.Lock()
		t.Maintain(opts)
		t.Reindex()
		s.maintainMu.Unlock()
		return fmt.Sprintf("analyzed: hot set = %d attribute(s)", len(t.HotAttrs())), nil
	default:
		return "", fmt.Errorf("unknown admin action %q", action)
	}
}

// addIndex adds an index and, when the spec changed, reindexes so the new index
// is built over the existing ads (AddIndex updates only the spec; existing
// segments' indexes are rebuilt by Reindex). Without this the index would apply
// only to future writes and would not prune the current data.
// archiveAdmin runs a management action on an append-only archive (history) table: the
// layout-tuning subset of table admin -- retrain the ZSTD dictionary, add/drop/rebuild
// per-segment indexes, rewrite, set retention, rotate -- plus truncate (empty the archive).
// Encryption/time-travel/hot-set actions do not apply to an archive. DAEMON-gated by the
// caller, like the mutable-table admin.
//
// schema.fit / schema.rebuild run the same shared implementations the mutable path uses, so the
// two table types cannot drift apart in behaviour or output.
func archiveAdmin(a *db.ArchiveTable, action string, args []string, hotTopN int) (string, error) {
	switch action {
	case "truncate":
		// Destructive reset: drop every record (a from-scratch re-sync empties the archive,
		// then re-ingests from the source). Retention/index config is preserved.
		a.Truncate()
		return "archive truncated", nil
	case "index.add.categorical":
		if len(args) == 0 {
			return "", fmt.Errorf("index.add.categorical needs at least one attribute")
		}
		return archiveIndexMsg(a, changedMsg("categorical index on "+join(args), a.AddIndex(args, nil))), nil
	case "index.add.value":
		if len(args) == 0 {
			return "", fmt.Errorf("index.add.value needs at least one attribute")
		}
		return archiveIndexMsg(a, changedMsg("value index on "+join(args), a.AddIndex(nil, args))), nil
	case "index.drop":
		if len(args) == 0 {
			return "", fmt.Errorf("index.drop needs at least one attribute")
		}
		changed := a.DropIndex(args...)
		if changed {
			a.Reindex()
		}
		return archiveIndexMsg(a, changedMsg("dropped index on "+join(args), changed)), nil
	case "index.reindex":
		a.Reindex()
		return "reindexed", nil
	case "analyze":
		// An append-only archive has no demand-driven index/hot auto-tune (its layout is fixed
		// at creation), so "redo statistics" is a reindex: it rebuilds each segment's per-value
		// histogram and other selectivity stats. Superseded-version drift does not apply.
		a.Reindex()
		return "analyzed (reindexed selectivity statistics)", nil
	case "rewrite":
		return fmt.Sprintf("rewrote %d record(s) with the current hot set", a.Rewrite()), nil
	case "codec.retrain":
		sampleMax := diagSampleMax
		if len(args) == 1 {
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
				sampleMax = v
			}
		}
		dictBytes, err := a.RetrainDict(sampleMax)
		if err != nil {
			return "", fmt.Errorf("retrain: %w", err)
		}
		return fmt.Sprintf("retrained ZSTD dictionary (%d bytes) and recompressed the archive", dictBytes), nil
	case "retention.set":
		r, err := parseRetentionArgs(args)
		if err != nil {
			return "", err
		}
		if err := a.SetRetention(r); err != nil {
			return "", fmt.Errorf("retention.set: %w", err)
		}
		return "retention: " + retentionSummary(r), nil
	case "schema.fit":
		return schemaFitAction(a, args)
	case "schema.groups":
		return schemaGroupsAction(a, args)
	case "schema.groups.agree":
		return schemaGroupsAgreeAction(a, args)
	case "schema.rebuild":
		return schemaRebuildAction(a, args, hotTopN)
	case "rotate":
		n, err := a.Rotate(float64(time.Now().Unix()))
		if err != nil {
			return "", fmt.Errorf("rotate: %w", err)
		}
		return fmt.Sprintf("rotated: dropped %d segment(s)", n), nil
	default:
		return "", fmt.Errorf("unknown archive admin action %q", action)
	}
}

// archiveIndexMsg appends the sealed-segment reach to an index-change acknowledgement. The
// change rebuilds every segment's index sidecar in place, so normally there is nothing to
// add; a non-zero stale count means some segment's rebuild did not complete (it is
// best-effort per segment) and index.reindex should be retried.
func archiveIndexMsg(a *db.ArchiveTable, msg string) string {
	stale, _ := a.StaleIndexSegments()
	if stale == 0 {
		return msg
	}
	return fmt.Sprintf("%s; %d segment(s) failed to rebuild and keep the previous index set -- retry with index.reindex", msg, stale)
}

// parseRetentionArgs parses `<maxSegments> <maxBytes> [maxAgeAttr maxAgeSeconds]` into a
// Retention. maxSegments is a count (0 = no bound); maxBytes accepts a byte count or a size
// suffix (KiB/MiB/GiB/TiB, or KB/MB/GB/TB; 0 = no bound); maxAgeAttr/maxAgeSeconds set the
// age bound (both must be given). All-zero clears retention (keep everything).
func parseRetentionArgs(args []string) (db.Retention, error) {
	var r db.Retention
	if len(args) < 2 {
		return r, fmt.Errorf("retention.set needs <maxSegments> <maxBytes> [maxAgeAttr maxAgeSeconds]")
	}
	seg, err := strconv.Atoi(args[0])
	if err != nil || seg < 0 {
		return r, fmt.Errorf("maxSegments must be a non-negative integer, got %q", args[0])
	}
	r.MaxSegments = seg
	b, err := parseByteSize(args[1])
	if err != nil {
		return r, fmt.Errorf("maxBytes: %w", err)
	}
	r.MaxBytes = b
	if len(args) >= 4 {
		r.MaxAgeAttr = args[2]
		age, err := strconv.ParseFloat(args[3], 64)
		if err != nil || age < 0 {
			return r, fmt.Errorf("maxAgeSeconds must be a non-negative number, got %q", args[3])
		}
		r.MaxAge = age
	} else if len(args) == 3 {
		return r, fmt.Errorf("maxAgeAttr given without maxAgeSeconds")
	}
	return r, nil
}

// parseByteSize parses a byte count with an optional binary (KiB/MiB/GiB/TiB) or decimal
// (KB/MB/GB/TB) suffix; a bare number is bytes.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	} {
		if len(s) > len(u.suf) && strings.EqualFold(s[len(s)-len(u.suf):], u.suf) {
			mult = u.m
			s = strings.TrimSpace(s[:len(s)-len(u.suf)])
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// retentionSummary renders a Retention for an admin acknowledgement.
func retentionSummary(r db.Retention) string {
	if (r == db.Retention{}) {
		return "unbounded (keep everything)"
	}
	parts := make([]string, 0, 3)
	if r.MaxSegments > 0 {
		parts = append(parts, fmt.Sprintf("maxSegments=%d", r.MaxSegments))
	}
	if r.MaxBytes > 0 {
		parts = append(parts, fmt.Sprintf("maxBytes=%d", r.MaxBytes))
	}
	if r.MaxAgeAttr != "" && r.MaxAge > 0 {
		parts = append(parts, fmt.Sprintf("maxAge=%gs on %s", r.MaxAge, r.MaxAgeAttr))
	}
	return join(parts)
}

func addIndex(t *db.DB, what string, categorical, value []string) string {
	changed := t.AddIndex(categorical, value)
	if !changed {
		return what + " (no change)"
	}
	t.Reindex()
	return what + " (changed; reindexed existing ads)"
}

func changedMsg(what string, changed bool) string {
	if changed {
		return what + " (changed)"
	}
	return what + " (no change)"
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// --- client ---

// Diagnostics fetches the default table's storage stats, hot set, indexes, and
// tuning suggestions.
func (c *Client) Diagnostics(ctx context.Context) (*Diagnostics, error) {
	return c.DiagnosticsTable(ctx, DefaultTable)
}

// DiagnosticsTable fetches the named table's diagnostics.
func (c *Client) DiagnosticsTable(ctx context.Context, table string) (*Diagnostics, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opDiag), table) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var d Diagnostics
	if err := json.Unmarshal([]byte(body.str()), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Explain reports how the default table would execute a constraint query.
func (c *Client) Explain(ctx context.Context, constraint string) (*db.QueryExplain, error) {
	return c.ExplainTable(ctx, DefaultTable, constraint)
}

// ExplainTable reports how the named table would execute a constraint query.
func (c *Client) ExplainTable(ctx context.Context, table, constraint string) (*db.QueryExplain, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putStr(putStr(req(id, opExplain), table), constraint)
	})
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var ex db.QueryExplain
	if err := json.Unmarshal([]byte(body.str()), &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

// MatchExplain reports how matchmaking the first request in reqTable matching
// jobSelector against resTable would execute: the job's Requirements rewritten over
// the slot (job constants baked in) and which resulting probes prune via a resource
// index. jobSelector is a constraint (e.g. `Key == "1.0"`) identifying the request.
func (c *Client) MatchExplain(ctx context.Context, reqTable, jobSelector, resTable, targetWhere string) (*db.MatchExplain, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		b := putStr(req(id, opMatchExplain), reqTable)
		b = putStr(b, jobSelector)
		b = putStr(b, resTable)
		b = putStr(b, targetWhere)
		return b
	})
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var ex db.MatchExplain
	if err := json.Unmarshal([]byte(body.str()), &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

// Admin runs a management action (index/hot-set) on the default table. Requires a
// DAEMON-authorized connection (see AdminTable).
func (c *Client) Admin(ctx context.Context, action string, args ...string) (string, error) {
	return c.AdminTable(ctx, DefaultTable, action, args...)
}

// AdminTable runs a management action on the named table; it returns the server's
// human-readable result. Every admin action retunes or restructures the store, so it
// requires DAEMON authorization -- refused on read-only and ordinary read-write
// connections alike.
func (c *Client) AdminTable(ctx context.Context, table, action string, args ...string) (string, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		b := putStr(putStr(req(id, opAdmin), table), action)
		b = putI32(b, int32(len(args)))
		for _, a := range args {
			b = putStr(b, a)
		}
		return b
	})
	if err != nil {
		return "", err
	}
	if status != stOK {
		return "", statusErr(status, body)
	}
	return body.str(), nil
}

// SetEncryptedAttrs sets the explicit attributes encrypted at rest on the named table
// (private attributes are always encrypted). It is a DAEMON-level action: the server
// refuses it unless the connection is privileged. Passing no attributes clears the
// explicit set. Returns the server's human-readable result.
func (c *Client) SetEncryptedAttrs(ctx context.Context, table string, attrs ...string) (string, error) {
	return c.AdminTable(ctx, table, "encrypt.set", attrs...)
}

// BackupKeyTable retrieves the named table's backup key -- the escrow key that decrypts
// its encrypted snapshots independently of the pool keys. DAEMON-level. Errors if
// encryption is not enabled.
func (c *Client) BackupKeyTable(ctx context.Context, table string) ([]byte, error) {
	s, err := c.AdminTable(ctx, table, "backup.key")
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(s)
}

// TruncateTable removes every ad from the named table. It is a DAEMON-level action
// (destructive, DB-wide locked): the server refuses it unless the connection is
// privileged. Returns the server's human-readable result.
func (c *Client) TruncateTable(ctx context.Context, table string) (string, error) {
	return c.AdminTable(ctx, table, "truncate")
}

// --- table catalog ---

// CreateTable creates (or no-ops if present) the named table.
func (c *Client) CreateTable(ctx context.Context, name string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opCreateTable), name) })
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// CreateTableInMemory creates (or no-ops if present) the named table as RAM-only: its data
// lives only in memory and is not recovered across a server restart. On a persistent server
// this avoids the disk I/O of persistence for high-churn, reconstructible data.
func (c *Client) CreateTableInMemory(ctx context.Context, name string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opCreateTableMem), name) })
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ConvertTableToMemory drops an existing table's on-disk backing, keeping its current
// contents in RAM only (they are gone after a server restart). Requires DAEMON
// authorization. Best run during low write activity: a write that races the conversion can
// be lost (the server takes a consistent snapshot but does not globally quiesce writers).
func (c *Client) ConvertTableToMemory(ctx context.Context, name string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opTableToMemory), name) })
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// DropTable removes the named table and its data.
func (c *Client) DropTable(ctx context.Context, name string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opDropTable), name) })
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// Tables lists the table names.
func (c *Client) Tables(ctx context.Context) ([]string, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return req(id, opListTables) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	n := int(body.i32())
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, body.str())
	}
	return names, nil
}

// schemaScanTable is the schema-accelerator surface shared by a mutable table and an archive, so
// schema.fit / schema.rebuild are ONE implementation for both. They were a mutable-table-only
// action first, which meant `.schema rebuild` on a history table failed as an unknown action --
// the tables that most want a rebuild being exactly the ones that could not have one.
type schemaScanTable interface {
	SchemaFit(sampleMax int) ([]db.SchemaFieldFit, int)
	ReschemaScan(sampleMax, hotTopN int) bool
	SchemaScanInfo() db.SchemaScanInfo
	GroupSchemas(sampleMax, k int) db.GroupSchemaInfo
	GroupSchemaDrift() db.GroupSchemaDrift
	GroupSchemaAgreement(sampleMax, k int) db.GroupSchemaAgreement
}

// schemaFitAction reports how well the derived schema still matches the data. Report only: the
// operator's input to deciding whether schema.rebuild is worth its cost.
func schemaFitAction(t schemaScanTable, args []string) (string, error) {
	sampleMax := diagSampleMax
	if len(args) == 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return "", fmt.Errorf("schema.fit <sampleMax> must be an integer")
		}
		sampleMax = n
	} else if len(args) > 1 {
		return "", fmt.Errorf("schema.fit takes at most <sampleMax>")
	}
	fit, sampled := t.SchemaFit(sampleMax)
	if len(fit) == 0 {
		return "columnar accelerator not enabled: no schema to measure", nil
	}
	b, err := json.Marshal(struct {
		Sampled int                 `json:"sampled"`
		Fields  []db.SchemaFieldFit `json:"fields"`
	}{sampled, fit})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// schemaRebuildAction re-derives the schema and rebuilds every sealed segment's columnar block.
// Routine maintenance keeps the schema pinned forever, so this is the only way to re-derive it as
// the workload drifts. hotTopN is the server's configured hot-column count, or 0 for the default.
func schemaRebuildAction(t schemaScanTable, args []string, hotTopN int) (string, error) {
	sampleMax, topN := diagSampleMax, hotTopN
	if topN <= 0 {
		topN = defaultSchemaHotTopN
	}
	switch len(args) {
	case 0:
	case 2:
		n, e1 := strconv.Atoi(args[0])
		k, e2 := strconv.Atoi(args[1])
		if e1 != nil || e2 != nil {
			return "", fmt.Errorf("schema.rebuild arguments must be integers")
		}
		sampleMax, topN = n, k
	default:
		return "", fmt.Errorf("schema.rebuild takes no arguments or <sampleMax> <topN>")
	}
	if !t.ReschemaScan(sampleMax, topN) {
		return "", fmt.Errorf("schema rebuild did not run: nothing to sample, or the " +
			"accelerator is unavailable here (encryption at rest)")
	}
	info := t.SchemaScanInfo()
	return fmt.Sprintf("schema rebuilt: %d field(s), %d hot, %d/%d sealed segments covered",
		info.SchemaFields, len(info.HotFields), info.CoveredSegments, info.SealedSegments), nil
}

// schemaGroupsAction reports the candidate group schemas: attribute sets the base schema does not
// carry which are present or absent TOGETHER, so they could be stored columnar for the ads holding
// them without spending a slot in the ads that do not.
//
// Report only, and deliberately so. A group is only worth a schema pointer if the same attributes
// keep co-occurring, which one sample cannot establish -- so this exists to be run repeatedly, and
// it prints the drift across retained derivations alongside the current one.
func schemaGroupsAction(t schemaScanTable, args []string) (string, error) {
	sampleMax, k := diagSampleMax, 0
	if len(args) >= 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return "", fmt.Errorf("schema.groups <sampleMax> must be an integer")
		}
		sampleMax = n
	}
	if len(args) >= 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return "", fmt.Errorf("schema.groups <sampleMax> <k>: k must be an integer")
		}
		k = n
	}
	if len(args) > 2 {
		return "", fmt.Errorf("schema.groups takes at most <sampleMax> <k>")
	}
	info := t.GroupSchemas(sampleMax, k)
	if info.Sampled == 0 {
		return "no ads sampled; nothing to derive", nil
	}
	var b strings.Builder
	baseFrac := 0.0
	if info.TotalCells > 0 {
		baseFrac = float64(info.BaseCells) / float64(info.TotalCells)
	}
	fmt.Fprintf(&b, "sampled %d ad(s); base schema %d field(s) covering %.1f%% of attribute occurrences\n",
		info.Sampled, info.BaseFields, baseFrac*100)
	if len(info.Groups) == 0 {
		fmt.Fprintf(&b, "no candidate groups: every attribute outside the base schema is either\n")
		fmt.Fprintf(&b, "unique to one ad or shares no presence pattern with another\n")
		return b.String(), nil
	}
	cum := baseFrac
	fmt.Fprintf(&b, "  %-4s %6s %7s %7s %8s %9s  %s\n", "grp", "attrs", "in", "none", "partial", "coverage", "members")
	for i, g := range info.Groups {
		cum += g.CellsFrac
		fmt.Fprintf(&b, "  %-4d %6d %6.1f%% %6.1f%% %7.2f%% %8.1f%%  %s\n",
			i+1, len(g.Attrs), g.InFrac*100, g.NoneFrac*100, g.PartialFrac*100,
			g.CellsFrac*100, strings.Join(g.Attrs, ", "))
	}
	fmt.Fprintf(&b, "base + %d group(s) would cover %.1f%% of attribute occurrences\n", len(info.Groups), cum*100)
	fmt.Fprintln(&b, "  in = holds every member (storable columnar); none = holds no member (its columns are")
	fmt.Fprintln(&b, "  provably undefined, so a query confined to them can skip the ad); partial = holds some,")
	fmt.Fprintln(&b, "  which needs a row decode. partial is 0 at derivation, so any growth is drift.")
	if d := t.GroupSchemaDrift(); d.Derivations > 1 {
		fmt.Fprintf(&b, "drift: %d derivations over %s; %d of the first %d group(s) still present; worst partial %.2f%%\n",
			d.Derivations, time.Duration(d.LastUnix-d.FirstUnix)*time.Second,
			d.Retained, d.OfFirst, d.MaxPartialFrac*100)
	} else {
		fmt.Fprintln(&b, "drift: only one derivation retained; run this again later to see whether the groups hold")
	}
	return b.String(), nil
}

// schemaGroupsAgreeAction re-derives per segment and reports how often each table-level group
// reappears -- the other half of whether a group is a property of the data or of the sample.
// O(segments x sample), so it is an operator action rather than part of a maintenance pass.
func schemaGroupsAgreeAction(t schemaScanTable, args []string) (string, error) {
	sampleMax, k := diagSampleMax, 0
	if len(args) >= 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return "", fmt.Errorf("schema.groups.agree <sampleMax> must be an integer")
		}
		sampleMax = n
	}
	if len(args) >= 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return "", fmt.Errorf("schema.groups.agree <sampleMax> <k>: k must be an integer")
		}
		k = n
	}
	if len(args) > 2 {
		return "", fmt.Errorf("schema.groups.agree takes at most <sampleMax> <k>")
	}
	info := t.GroupSchemas(sampleMax, k)
	ag := t.GroupSchemaAgreement(sampleMax, k)
	if ag.Segments == 0 {
		return "no sealed segments to compare against", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "re-derived over %d sealed segment(s):\n", ag.Segments)
	for i, frac := range ag.PerGroup {
		var members string
		if i < len(info.Groups) {
			members = strings.Join(info.Groups[i].Attrs, ", ")
		}
		fmt.Fprintf(&b, "  group %d: reproduced in %5.1f%% of segments  %s\n", i+1, frac*100, members)
	}
	fmt.Fprintln(&b, "  a group reproduced in few segments is a sampling artifact, not a property of the")
	fmt.Fprintln(&b, "  data, and a schema pointer spent on it would buy coverage in some segments only")
	return b.String(), nil
}
