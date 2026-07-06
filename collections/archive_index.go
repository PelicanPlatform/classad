package collections

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"

	"github.com/RoaringBitmap/roaring/v2"
)

// Sidecar index file format (per sealed segment, "seg-<id>.idx"). It serializes the
// immutable segIndex so recovery is O(segments), not O(records). All integers are
// little-endian.
//
//	magic   uint32  = 'A','R','C','X'
//	version uint16  = 1
//	upto    uint32
//	specGen uint64
//	all     bitmap  (uint32 len + roaring bytes)
//	catN    uint32; catN × { id uint32; exc bitmap; postN uint32; postN × { str key; bitmap } }
//	valN    uint32; valN × { id uint32; exc bitmap; postN uint32; postN × { f64 key; bitmap } }
const (
	sidecarMagic   = 0x41524358 // "ARCX"
	sidecarVersion = 1
)

// writeSidecarIndex serializes a sealed segment's index to its sidecar file and
// fsyncs it.
func (a *Archive) writeSidecarIndex(as *archiveSeg) error {
	si := as.seg.idx.Load()
	if si == nil {
		return fmt.Errorf("archive: sealing segment %d with no index", as.seg.id)
	}
	var b []byte
	b = appendU32(b, sidecarMagic)
	b = appendU16(b, sidecarVersion)
	b = appendU32(b, si.upto)
	b = appendU64(b, si.specGen)
	b, err := appendBitmap(b, si.all)
	if err != nil {
		return err
	}

	b = appendU32(b, uint32(len(si.cat)))
	for id, cp := range si.cat {
		b = appendU32(b, id)
		if b, err = appendBitmap(b, cp.exc); err != nil {
			return err
		}
		b = appendU32(b, uint32(len(cp.post)))
		for k, bm := range cp.post {
			b = appendBytes(b, []byte(k))
			if b, err = appendBitmap(b, bm); err != nil {
				return err
			}
		}
	}

	b = appendU32(b, uint32(len(si.val)))
	for id, vp := range si.val {
		b = appendU32(b, id)
		if b, err = appendBitmap(b, vp.exc); err != nil {
			return err
		}
		b = appendU32(b, uint32(len(vp.post)))
		for k, bm := range vp.post {
			b = appendU64(b, math.Float64bits(k))
			if b, err = appendBitmap(b, bm); err != nil {
				return err
			}
		}
	}
	return writeFileSync(a.idxPath(as.seg.id), b)
}

// readSidecarIndex loads a segment's sidecar index file back into a *segIndex.
func readSidecarIndex(path string) (*segIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &cursor{b: data}
	if c.u32() != sidecarMagic {
		return nil, fmt.Errorf("archive: bad sidecar magic in %s", path)
	}
	if v := c.u16(); v != sidecarVersion {
		return nil, fmt.Errorf("archive: unsupported sidecar version %d in %s", v, path)
	}
	si := &segIndex{
		cat: map[uint32]*catPostings{},
		val: map[uint32]*valPostings{},
	}
	si.upto = c.u32()
	si.specGen = c.u64()
	if si.all, err = c.bitmap(); err != nil {
		return nil, err
	}

	catN := c.u32()
	for i := uint32(0); i < catN; i++ {
		id := c.u32()
		cp := &catPostings{post: map[string]*roaring.Bitmap{}}
		if cp.exc, err = c.bitmap(); err != nil {
			return nil, err
		}
		postN := c.u32()
		for j := uint32(0); j < postN; j++ {
			k := string(c.bytes())
			bm, err := c.bitmap()
			if err != nil {
				return nil, err
			}
			cp.post[k] = bm
		}
		si.cat[id] = cp
	}

	valN := c.u32()
	for i := uint32(0); i < valN; i++ {
		id := c.u32()
		vp := &valPostings{post: map[float64]*roaring.Bitmap{}}
		if vp.exc, err = c.bitmap(); err != nil {
			return nil, err
		}
		postN := c.u32()
		for j := uint32(0); j < postN; j++ {
			k := math.Float64frombits(c.u64())
			bm, err := c.bitmap()
			if err != nil {
				return nil, err
			}
			vp.post[k] = bm
		}
		si.val[id] = vp
	}
	if c.err != nil {
		return nil, fmt.Errorf("archive: truncated sidecar %s: %w", path, c.err)
	}
	return si, nil
}

// --- little-endian append helpers ---

func appendU16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func appendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func appendU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }

func appendBytes(b, p []byte) []byte {
	b = appendU32(b, uint32(len(p)))
	return append(b, p...)
}

func appendBitmap(b []byte, bm *roaring.Bitmap) ([]byte, error) {
	if bm == nil {
		bm = roaring.New()
	}
	p, err := bm.MarshalBinary()
	if err != nil {
		return b, err
	}
	return appendBytes(b, p), nil
}

// cursor reads little-endian fields from a byte slice, latching the first error so
// callers can check once at the end (an out-of-range read yields zero and sets err).
type cursor struct {
	b   []byte
	i   int
	err error
}

func (c *cursor) need(n int) bool {
	if c.err != nil {
		return false
	}
	if c.i+n > len(c.b) {
		c.err = fmt.Errorf("unexpected end of data")
		return false
	}
	return true
}

func (c *cursor) u16() uint16 {
	if !c.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(c.b[c.i:])
	c.i += 2
	return v
}

func (c *cursor) u32() uint32 {
	if !c.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(c.b[c.i:])
	c.i += 4
	return v
}

func (c *cursor) u64() uint64 {
	if !c.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(c.b[c.i:])
	c.i += 8
	return v
}

func (c *cursor) bytes() []byte {
	n := int(c.u32())
	if !c.need(n) {
		return nil
	}
	p := c.b[c.i : c.i+n]
	c.i += n
	return p
}

func (c *cursor) bitmap() (*roaring.Bitmap, error) {
	p := c.bytes()
	if c.err != nil {
		return nil, c.err
	}
	bm := roaring.New()
	if err := bm.UnmarshalBinary(p); err != nil {
		return nil, err
	}
	return bm, nil
}

// writeFileSync writes data to path (truncating) and fsyncs it before returning.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
