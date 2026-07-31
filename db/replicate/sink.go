package replicate

import (
	"os"
	"sync"

	"github.com/PelicanPlatform/classad/db"
)

// Sink applies Changes into a target store and tracks a durable resume cursor. It is the shared,
// transport-neutral core: an HTTP client (classad/changefeed) and a dbrpc/CEDAR replicator both
// feed Changes to it. Apply must be idempotent enough for at-least-once delivery (a reconnect
// re-delivers the tail): the mutable sink is last-write-wins by key; the archive sink is
// append-only (see NewArchiveSink).
type Sink interface {
	// Apply applies one change. For KindSynced it should flush/commit; KindReset/KindGap are
	// advisory (the sink may clear derived state / record the gap).
	Apply(c Change) error
	// Commit durably records cursor as the resume point (everything up to it is applied).
	Commit(cursor []byte) error
	// Cursor returns the last committed resume cursor (nil = from the beginning).
	Cursor() []byte
}

// CursorStore persists a sink's resume cursor across restarts.
type CursorStore interface {
	Load() ([]byte, error) // nil, nil = none stored yet
	Save(cursor []byte) error
}

// MemCursorStore is an in-memory CursorStore (tests, or an ephemeral sink).
type MemCursorStore struct {
	mu  sync.Mutex
	cur []byte
}

func (m *MemCursorStore) Load() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.cur...), nil
}

func (m *MemCursorStore) Save(c []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = append([]byte(nil), c...)
	return nil
}

// FileCursorStore persists the cursor to a file (atomic replace).
type FileCursorStore struct{ Path string }

func (f FileCursorStore) Load() ([]byte, error) {
	b, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

func (f FileCursorStore) Save(c []byte) error {
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, c, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

// SrcAttr is the attribute the sinks stamp with a change's source identity, so a fanned-in target
// can be queried/deduped by source.
const SrcAttr = "Src"

// archiveSink appends upserts into an append-only archive, stamping Src. Delivery is at-least-once:
// a reconnect resumes from the last committed cursor, so only a crash between an append and the
// cursor commit can re-append (a duplicate row) -- tolerable for history, and dedupable downstream
// by a stable job identity (GlobalJobId). Deletes are ignored (an archive is append-only); Reset is
// a no-op (append-only history is not rebuilt); Gap is recorded via OnGap if set.
type archiveSink struct {
	a     *db.ArchiveTable
	src   string
	store CursorStore
	cur   []byte
	OnGap func(src string, fromMillis, toMillis int64)
}

// NewArchiveSink builds a Sink that imports a source's changes into archive a, stamping SrcAttr=src.
func NewArchiveSink(a *db.ArchiveTable, src string, store CursorStore) (Sink, error) {
	cur, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &archiveSink{a: a, src: src, store: store, cur: cur}, nil
}

func (s *archiveSink) Apply(c Change) error {
	switch c.Kind {
	case KindUpsert:
		if c.Ad == nil {
			return nil
		}
		if _, ok := c.Ad.Lookup(SrcAttr); !ok {
			c.Ad.InsertAttrString(SrcAttr, s.src)
		}
		return s.a.Append(c.Ad)
	case KindSynced:
		if len(c.Cursor) > 0 {
			return s.Commit(c.Cursor)
		}
	case KindGap:
		if s.OnGap != nil {
			s.OnGap(s.src, c.FromMillis, c.ToMillis)
		}
	case KindDelete, KindReset:
		// append-only: nothing to remove / rebuild.
	}
	return nil
}

func (s *archiveSink) Commit(cursor []byte) error {
	if len(cursor) == 0 {
		return nil
	}
	if err := s.store.Save(cursor); err != nil {
		return err
	}
	s.cur = append([]byte(nil), cursor...)
	return nil
}

func (s *archiveSink) Cursor() []byte { return s.cur }

// tableSink upserts/deletes into a mutable table by key -- naturally idempotent (last-write-wins),
// so a re-delivered tail is a no-op. Stamps SrcAttr on upserts. Reset clears no local state here
// (the source will re-send the snapshot; upserts overwrite; a full-reconcile delete-sweep is a
// future refinement).
type tableSink struct {
	d     *db.DB
	src   string
	store CursorStore
	cur   []byte
}

// NewTableSink builds a Sink that imports a source's changes into mutable table d, stamping SrcAttr.
func NewTableSink(d *db.DB, src string, store CursorStore) (Sink, error) {
	cur, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &tableSink{d: d, src: src, store: store, cur: cur}, nil
}

func (s *tableSink) Apply(c Change) error {
	switch c.Kind {
	case KindUpsert:
		if c.Ad == nil {
			return nil
		}
		if _, ok := c.Ad.Lookup(SrcAttr); !ok {
			c.Ad.InsertAttrString(SrcAttr, s.src)
		}
		tx := s.d.Begin()
		tx.NewClassAd(c.Key, c.Ad)
		return tx.Commit()
	case KindDelete:
		tx := s.d.Begin()
		tx.DestroyClassAd(c.Key)
		return tx.Commit()
	case KindSynced:
		if len(c.Cursor) > 0 {
			return s.Commit(c.Cursor)
		}
	}
	return nil
}

func (s *tableSink) Commit(cursor []byte) error {
	if len(cursor) == 0 {
		return nil
	}
	if err := s.store.Save(cursor); err != nil {
		return err
	}
	s.cur = append([]byte(nil), cursor...)
	return nil
}

func (s *tableSink) Cursor() []byte { return s.cur }
