package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// Persistent watch epoch for a persistent append-only collection (an Archive).
//
// A watch cursor is {epoch, perShardSeq[]}, and it may only be resumed against a store in
// which every sequence number it covers still names the record it named when the cursor was
// issued. Within one process that holds by construction. Across a restart it holds only if
// the reopened store (1) contains every record the old process ever handed a watcher, and
// (2) never assigns a sequence the old process already assigned to a different record.
// Neither holds after a crash: a record can be published, and its seq end up in a client's
// cursor, before that record's bytes are durable (WatchCursor and catch-up read commitSeq,
// which advances before the commit's msync), so recovery can come back with a shorter log,
// rebuild commitSeq from it, and reissue the lost seqs to new records -- which a resumed
// cursor would then silently skip.
//
// So the epoch is carried across a restart only when the previous process proved, at Close,
// that both conditions hold, and is otherwise rotated (every cursor then gets a Reset):
//
//   - Open CONSUMES <dir>/watch.epoch before mapping any segment: it reads the epoch and the
//     per-shard high-water commitSeq, then removes the file and fsyncs the directory. From
//     that point until a successful Close, a crash leaves no marker, and the next Open picks
//     a fresh random epoch.
//   - Close, after every segment has been flushed without error (and no write or sync error
//     is pending), fsyncs each shard directory (so segment files created this session are
//     durable) and only then durably writes the marker with the epoch and each shard's
//     commitSeq. The marker's existence therefore implies every record issued under that
//     epoch reached disk.
//   - On reopen the shard's commitSeq becomes max(recovered, marker high-water), so a seq
//     issued before the restart is never reissued even when its record is no longer on disk
//     (Truncate, or Rotate emptying a shard).
//
// Mutable collections keep a per-process random epoch: their delete journal is in memory, so
// a resume across a restart could not replay deletes precisely anyway.
//
// Copying a cleanly closed store directory and later reopening an older copy is outside this
// guarantee (the epoch matches but the log is shorter); Watch's "cursor ahead of head" check
// turns that into a Reset only while the restored log is still behind the cursor. Delete
// watch.epoch when restoring a store from a copy.

const (
	watchEpochFile    = "watch.epoch"
	watchEpochMagic   = 0x48435745 // "EWCH"
	watchEpochVersion = 1
)

// watchEpochMark is the content of a clean-shutdown watch.epoch marker.
type watchEpochMark struct {
	epoch uint64
	seqs  []uint64 // per-shard commitSeq at Close (the high-water mark)
}

func encodeWatchEpoch(m watchEpochMark) []byte {
	b := make([]byte, 0, 24+8*len(m.seqs)+4)
	b = binary.LittleEndian.AppendUint32(b, watchEpochMagic)
	b = binary.LittleEndian.AppendUint32(b, watchEpochVersion)
	b = binary.LittleEndian.AppendUint64(b, m.epoch)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(m.seqs)))
	for _, s := range m.seqs {
		b = binary.LittleEndian.AppendUint64(b, s)
	}
	return binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

func decodeWatchEpoch(b []byte) (watchEpochMark, bool) {
	if len(b) < 28 {
		return watchEpochMark{}, false
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum ||
		binary.LittleEndian.Uint32(body[0:]) != watchEpochMagic ||
		binary.LittleEndian.Uint32(body[4:]) != watchEpochVersion {
		return watchEpochMark{}, false
	}
	m := watchEpochMark{epoch: binary.LittleEndian.Uint64(body[8:])}
	n := binary.LittleEndian.Uint64(body[16:])
	if m.epoch == 0 || uint64(len(body)) != 24+8*n {
		return watchEpochMark{}, false
	}
	m.seqs = make([]uint64, n)
	for i := range m.seqs {
		m.seqs[i] = binary.LittleEndian.Uint64(body[24+8*i:])
	}
	return m, true
}

// consumeWatchEpoch reads and removes dir's clean-shutdown marker, making the removal durable
// before returning. ok reports whether a valid marker was present. An error means the marker
// could not be removed durably, and the caller must not open the store: a later crash would
// otherwise leave the old marker vouching for seqs this process issues.
func consumeWatchEpoch(dir string) (m watchEpochMark, ok bool, err error) {
	path := filepath.Join(dir, watchEpochFile)
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return watchEpochMark{}, false, nil
		}
		return watchEpochMark{}, false, rerr
	}
	m, ok = decodeWatchEpoch(b)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return watchEpochMark{}, false, err
	}
	if err := syncDir(dir); err != nil {
		return watchEpochMark{}, false, err
	}
	return m, ok, nil
}

// syncDir fsyncs a directory so entries created, renamed, or removed in it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// claimWatchEpoch is Open's half of the protocol for a persistent append-only collection with
// Watch enabled: consume the marker and adopt its epoch if it is valid for this shard count
// (otherwise keep the random one New chose). Returns the marker's high-water seqs to apply once
// the shards are loaded, or nil.
func (c *Collection) claimWatchEpoch(dir string) ([]uint64, error) {
	m, ok, err := consumeWatchEpoch(dir)
	if err != nil {
		return nil, fmt.Errorf("watch epoch: %w", err)
	}
	c.watchEpochDir = dir
	if !ok || len(m.seqs) != len(c.shards) {
		return nil, nil
	}
	c.hub.epoch = m.epoch
	return m.seqs, nil
}

// persistWatchEpoch is Close's half: called only after every segment flushed cleanly, it makes
// the segment files' directory entries durable and then writes the marker. seqs is each shard's
// commitSeq, captured under the shard lock during Close. A failure is deliberately not a Close
// error: Open already consumed the previous marker, so the only consequence of writing none is
// that the next Open rotates the epoch (watchers Reset) -- the safe direction.
func (c *Collection) persistWatchEpoch(seqs []uint64) {
	if c.watchEpochDir == "" {
		return
	}
	for i := range c.shards {
		if syncDir(filepath.Join(c.dir, fmt.Sprintf("%d", i))) != nil {
			return
		}
	}
	if writeFileDurable(filepath.Join(c.watchEpochDir, watchEpochFile),
		encodeWatchEpoch(watchEpochMark{epoch: c.hub.epoch, seqs: seqs})) != nil {
		// A rename that landed without its directory fsync may or may not survive; either
		// outcome is safe, since the segments were made durable first.
		return
	}
}
