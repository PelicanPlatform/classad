package db

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// viewArchiveSegmentSize overrides the sealed-segment size of a continuous aggregate's
// archive; 0 uses the archive default (8 MiB). Tests set it small to exercise segment
// rotation with little data.
var viewArchiveSegmentSize int

// Continuous aggregates: a materialized view whose GROUP BY includes a time_bucket. Recent
// buckets live in the in-memory backing (updated in place, so late data lands correctly);
// once a bucket's window has closed (wall-clock past start+width+grace) it is SEALED --
// appended once to a per-view append-only archive and evicted from memory -- so history is
// unbounded and cheap to read while memory stays bounded to recent buckets. The watermark
// (highest sealed bucket start) makes sealing idempotent across late data and reload: a row
// whose bucket start is <= watermark is dropped rather than resurrecting a sealed bucket.

const viewArchiveSubdir = "archive"

// initContinuous prepares a continuous-aggregate view before its build goroutine starts: it
// loads the persisted watermark and opens the per-view archive, so the build's replay drops
// already-sealed buckets (start <= watermark) instead of re-appending them. stateDir is the
// view's catalog directory (<catalog>/views/<name>); "" for an in-memory catalog, in which
// case the view still buckets but never seals (bounded by the cardinality cap). No-op for a
// gauge view.
func (v *View) initContinuous(stateDir string) error {
	if v.bucketWidth == 0 || stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	st, err := loadViewState(stateDir)
	if err != nil {
		return err
	}
	v.watermark = st.Watermark
	v.stateDir = stateDir

	timeAlias := v.spec.Groups[v.bucketIdx].Alias
	cfg := ArchiveConfig{ZoneAttrs: []string{timeAlias}, SegmentSize: viewArchiveSegmentSize}
	if v.spec.Retention > 0 {
		cfg.Retention = collections.Retention{MaxAgeAttr: timeAlias, MaxAge: float64(v.spec.Retention)}
	}
	arch, err := openArchiveTable(filepath.Join(stateDir, viewArchiveSubdir), cfg)
	if err != nil {
		return err
	}
	v.archive = arch
	return nil
}

// sealAged appends and evicts every live bucket whose window has closed (now past
// start+width+grace), advancing the watermark, forgetting the base-key contributions that
// fed the sealed buckets, applying archive retention, and persisting the watermark. The
// caller holds v.mu. No-op for a gauge view or an in-memory catalog (no archive).
func (v *View) sealAged(now int64) {
	if v.archive == nil {
		return
	}
	sealAtOrBelow := now - v.bucketWidth - v.grace // seal buckets whose start <= this
	// Collect the aged buckets and seal them in bucket-start order, so the archive stays
	// time-ordered: each segment then carries a contiguous time range, which keeps zone-map
	// pruning and age-based retention (drop a segment whose max time is old) precise.
	type agedBucket struct {
		key   string
		start int64
	}
	var toSeal []agedBucket
	for gk, g := range v.groups {
		start, err := strconv.ParseInt(g.labels[v.bucketIdx], 10, 64)
		if err != nil || start > sealAtOrBelow {
			continue // unparsable, or the bucket window is still open
		}
		toSeal = append(toSeal, agedBucket{gk, start})
	}
	sort.Slice(toSeal, func(i, j int) bool { return toSeal[i].start < toSeal[j].start })

	advanced := false
	for _, a := range toSeal {
		if err := v.archive.Append(v.renderGroupAd(v.groups[a.key])); err != nil {
			continue // keep the bucket live and retry on the next tick
		}
		delete(v.groups, a.key)
		_, _ = v.backing.Delete(a.key)
		if a.start > v.watermark {
			v.watermark = a.start
			advanced = true
		}
	}
	if !advanced {
		return
	}
	// Base keys whose bucket is now sealed will never change it again; forget them so
	// contrib stays bounded to the live window.
	for k, c := range v.contrib {
		if c.bucketStart <= v.watermark {
			delete(v.contrib, k)
		}
	}
	if v.spec.Retention > 0 {
		_, _ = v.archive.Rotate(float64(now))
	}
	if v.stateDir != "" {
		_ = saveViewState(v.stateDir, viewState{Watermark: v.watermark})
	}
}

// Seal appends and evicts every bucket whose window has closed as of now (unix seconds)
// into the archive, advancing the watermark. It is what the periodic tick invokes; exported
// so an operator (or a test) can force sealing at a chosen instant.
func (v *View) Seal(now int64) {
	v.mu.Lock()
	v.sealAged(now)
	v.mu.Unlock()
}

// tickLoop periodically seals aged buckets until ctx is cancelled. Only continuous
// aggregates with an archive start one.
func (v *View) tickLoop(ctx context.Context) {
	if v.tickEvery <= 0 {
		return
	}
	t := time.NewTicker(v.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.mu.Lock()
			v.sealAged(v.nowFn())
			v.mu.Unlock()
		}
	}
}

// SealedQuery streams this continuous aggregate's sealed (archived) rows matching the
// constraint. It returns an empty sequence for a gauge view or an in-memory continuous
// aggregate (no archive). Reads union this with the live backing so a view read returns the
// full series, not just the unsealed buckets.
func (v *View) SealedQuery(constraint string) (iter.Seq[*classad.ClassAd], error) {
	v.mu.Lock()
	arch := v.archive // the archive is internally synchronized; don't hold v.mu during the scan
	v.mu.Unlock()
	if arch == nil {
		return func(yield func(*classad.ClassAd) bool) {}, nil
	}
	return arch.Query(constraint)
}

// LateDrops reports how many base rows were dropped because their time bucket was already
// sealed (out-of-window late data).
func (v *View) LateDrops() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lateDrops
}

// viewState is a continuous aggregate's mutable persisted state (the immutable ViewSpec is
// saved separately). Only the watermark is durable; sealed history lives in the archive and
// the live window is rebuilt from the base table on reload.
type viewState struct {
	Watermark int64 `json:"watermark"`
}

func saveViewState(stateDir string, st viewState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(stateDir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, "state.json"))
}

func loadViewState(stateDir string) (viewState, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if os.IsNotExist(err) {
		return viewState{Watermark: -1}, nil // nothing sealed yet
	}
	if err != nil {
		return viewState{}, err
	}
	var st viewState
	if err := json.Unmarshal(b, &st); err != nil {
		return viewState{}, err
	}
	return st, nil
}
