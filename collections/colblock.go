package collections

import "errors"

var errNotNumericField = errors.New("adschema: scanInt on a non-numeric field")

// columnarBlock is the PAX layout for a row-group of adSchema records (see adschema.go): the
// schema's popular ("hot") numeric fields plus the escape and bool bitsets stay uncompressed
// in a fixed-stride region (so an attribute scan reads a column with no decode), while the
// cold numeric fields (columnar), the string regions, and the cold tails are each a separately
// Codec-compressed stream. A numeric scan on a hot field touches only the uncompressed region;
// a scan on a cold field decompresses just that column group; a full-ad reconstruct
// decompresses the string + cold streams. This is the sealed-segment form; the active segment
// stays row-based (per-record).
//
// Hot region per record (stride = hotStride): [escape bitmap][bool bitset][hot int/real,
// packed]. Cold numeric buffer is columnar: each cold field's values contiguous across the
// block. String/cold streams are per-record regions concatenated, addressed by strOff/coldOff.
type columnarBlock struct {
	schema *adSchema
	codec  Codec
	n      int

	hot         []byte // uncompressed region, hotStride bytes per record
	hotStride   int
	hotNum      []int       // hot int/real field indices (into schema.fields), layout order
	hotFieldOff map[int]int // field idx -> byte offset within a record's hot region

	coldNum        []int       // cold int/real field indices, layout order
	coldFieldStart map[int]int // field idx -> start offset in the decompressed cold-numeric buffer
	coldNumComp    []byte      // cold numeric fields, columnar, Codec-compressed

	strComp, coldComp []byte // per-record string regions / cold tails, concatenated + compressed
	strOff, coldOff   []int  // per-record cumulative offsets into the decompressed streams
}

// encodeColumnarBlock builds a columnar block from row-form records (each the output of
// adSchema.encode). hotNumFields lists the schema field indices (int/real) to keep uncompressed
// -- the popular ones, by query demand. Bools and the escape bitmap are always in the hot
// region (tiny, and bools may be scanned).
func encodeColumnarBlock(s *adSchema, recs [][]byte, hotNumFields []int, codec Codec) *columnarBlock {
	hotSet := make(map[int]bool, len(hotNumFields))
	for _, i := range hotNumFields {
		hotSet[i] = true
	}
	b := &columnarBlock{schema: s, codec: codec, n: len(recs), hotFieldOff: map[int]int{}, coldFieldStart: map[int]int{}}
	for i := range s.fields {
		if k := s.fields[i].kind; k == akInt || k == akReal {
			if hotSet[i] {
				b.hotNum = append(b.hotNum, i)
			} else {
				b.coldNum = append(b.coldNum, i)
			}
		}
	}
	// Hot region layout and cold columnar offsets.
	hp := s.escBytes + s.boolBytes
	for _, i := range b.hotNum {
		b.hotFieldOff[i] = hp
		hp += s.fields[i].width
	}
	b.hotStride = hp
	cp := 0
	for _, i := range b.coldNum {
		b.coldFieldStart[i] = cp
		cp += s.fields[i].width * len(recs)
	}

	b.hot = make([]byte, 0, b.hotStride*len(recs))
	coldRaw := make([]byte, cp)
	var strCat, coldCat []byte
	b.strOff = []int{0}
	b.coldOff = []int{0}
	for k, r := range recs {
		b.hot = append(b.hot, r[:s.escBytes]...)                       // escape
		b.hot = append(b.hot, r[s.escBytes:s.escBytes+s.boolBytes]...) // bool bitset
		for _, i := range b.hotNum {
			off := s.escBytes + s.fields[i].off
			b.hot = append(b.hot, r[off:off+s.fields[i].width]...)
		}
		for _, i := range b.coldNum {
			off := s.escBytes + s.fields[i].off
			copy(coldRaw[b.coldFieldStart[i]+k*s.fields[i].width:], r[off:off+s.fields[i].width])
		}
		_, str, cold := s.splitRecord(r)
		strCat = append(strCat, str...)
		coldCat = append(coldCat, cold...)
		b.strOff = append(b.strOff, len(strCat))
		b.coldOff = append(b.coldOff, len(coldCat))
	}
	b.coldNumComp = codec.Compress(nil, coldRaw)
	b.strComp = codec.Compress(nil, strCat)
	b.coldComp = codec.Compress(nil, coldCat)
	return b
}

// scanInt calls fn for each record's value of a numeric (int/real read as int bits) field: a
// hot field reads the uncompressed region directly; a cold field decompresses its column group
// once. present is false for a missing/exceptional record (its value is in the cold tail).
func (b *columnarBlock) scanInt(fieldIdx int, fn func(rec int, present bool, v int64)) error {
	f := b.schema.fields[fieldIdx]
	if _, hot := b.hotFieldOff[fieldIdx]; hot {
		off := b.hotFieldOff[fieldIdx]
		for k := 0; k < b.n; k++ {
			base := k * b.hotStride
			if testBit(b.hot[base:base+b.schema.escBytes], fieldIdx) {
				fn(k, false, 0)
				continue
			}
			fn(k, true, readIntLE(b.hot[base+off:], f.width, f.unsigned))
		}
		return nil
	}
	start, ok := b.coldFieldStart[fieldIdx]
	if !ok {
		return errNotNumericField
	}
	coldRaw, err := b.codec.Decompress(nil, b.coldNumComp) // one decode for the whole column group
	if err != nil {
		return err
	}
	esc := b.escapeAt
	for k := 0; k < b.n; k++ {
		if testBit(esc(k), fieldIdx) {
			fn(k, false, 0)
			continue
		}
		fn(k, true, readIntLE(coldRaw[start+k*f.width:], f.width, f.unsigned))
	}
	return nil
}

// escapeAt returns record k's escape bitmap (in the uncompressed hot region).
func (b *columnarBlock) escapeAt(k int) []byte {
	base := k * b.hotStride
	return b.hot[base : base+b.schema.escBytes]
}

// reconstruct rebuilds record k's row form (identical to the adSchema.encode input), so
// schema.forEach reconstructs the full ad. Decompresses the cold-numeric, string, and cold
// streams -- the full-ad-read cost.
func (b *columnarBlock) reconstruct(k int) ([]byte, error) {
	s := b.schema
	coldRaw, err := b.codec.Decompress(nil, b.coldNumComp)
	if err != nil {
		return nil, err
	}
	strRaw, err := b.codec.Decompress(nil, b.strComp)
	if err != nil {
		return nil, err
	}
	tailRaw, err := b.codec.Decompress(nil, b.coldComp)
	if err != nil {
		return nil, err
	}
	rec := make([]byte, s.escBytes+s.fixedLen, s.escBytes+s.fixedLen+(b.strOff[k+1]-b.strOff[k])+(b.coldOff[k+1]-b.coldOff[k]))
	base := k * b.hotStride
	copy(rec[:s.escBytes], b.hot[base:base+s.escBytes])                                              // escape
	copy(rec[s.escBytes:s.escBytes+s.boolBytes], b.hot[base+s.escBytes:base+s.escBytes+s.boolBytes]) // bools
	for _, i := range b.hotNum {
		w := s.fields[i].width
		copy(rec[s.escBytes+s.fields[i].off:], b.hot[base+b.hotFieldOff[i]:base+b.hotFieldOff[i]+w])
	}
	for _, i := range b.coldNum {
		w := s.fields[i].width
		copy(rec[s.escBytes+s.fields[i].off:], coldRaw[b.coldFieldStart[i]+k*w:b.coldFieldStart[i]+k*w+w])
	}
	rec = append(rec, strRaw[b.strOff[k]:b.strOff[k+1]]...)
	rec = append(rec, tailRaw[b.coldOff[k]:b.coldOff[k+1]]...)
	return rec, nil
}
