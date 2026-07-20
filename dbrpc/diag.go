package dbrpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
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
}

// diagSampleMax bounds the ad sample the server takes for index suggestions.
const diagSampleMax = 2000

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
	}
	return json.Marshal(d)
}

// admin runs a management action. Actions:
//
//	index.add.categorical <attr>...   add categorical (string eq/membership) indexes
//	index.add.value <attr>...         add value (numeric + range) indexes
//	index.drop <attr>...              drop indexes on the given attributes
//	index.reindex                     rebuild all indexes from live ads
//	hot.add <attr>...                 pin attributes into the hot set
//	hot.refresh <sampleMax> <topN>    recompute the hot set from sampled frequency
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
	default:
		return "", fmt.Errorf("unknown admin action %q", action)
	}
}

// addIndex adds an index and, when the spec changed, reindexes so the new index
// is built over the existing ads (AddIndex updates only the spec; existing
// segments' indexes are rebuilt by Reindex). Without this the index would apply
// only to future writes and would not prune the current data.
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
