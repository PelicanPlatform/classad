package collections

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Codec compresses and decompresses encoded ad bytes. It is a seam so the store
// can run with no compression (identityCodec, the default) in tests and
// benchmarks, and with ZSTD — optionally with a pre-trained shared dictionary —
// in production.
//
// A codec's dictionary is fixed for the life of a collection: stored records are
// opaque compressed bytes that compaction copies verbatim, so every record must
// be decodable by the same codec. Re-training the dictionary between compactions
// (which requires versioning dictionaries per record and recompressing) is a
// future extension; a dictionary trained once from a representative sample (see
// TrainDict) already captures the cross-ad redundancy that dominates a pool of
// similar ClassAds.
type Codec interface {
	// Compress appends the compressed form of src to dst and returns it.
	Compress(dst, src []byte) []byte
	// Decompress appends the decompressed form of src to dst and returns it.
	Decompress(dst, src []byte) ([]byte, error)
	// Name identifies the codec (for diagnostics).
	Name() string
}

// identityCodec stores ad bytes verbatim (the default).
type identityCodec struct{}

func (identityCodec) Compress(dst, src []byte) []byte { return append(dst, src...) }

func (identityCodec) Decompress(dst, src []byte) ([]byte, error) { return append(dst, src...), nil }

func (identityCodec) Name() string { return "identity" }

// zstdCodec compresses with ZSTD, optionally against a fixed shared dictionary.
// EncodeAll/DecodeAll are safe for concurrent use, so a single encoder/decoder
// pair serves all shards.
type zstdCodec struct {
	enc     *zstd.Encoder
	dec     *zstd.Decoder
	hasDict bool
	dopts   []zstd.DOption // decoder options, for pooled streaming (prefix) decoders
	spool   sync.Pool      // *streamDec, one per concurrent prefix read
}

// maxEncoderConcurrency is the CEILING on how many encoder states (each a resident
// multi-MB match window) a shared zstd codec preallocates. The effective value is
// min(GOMAXPROCS, this): the library default is GOMAXPROCS, so this only ever LOWERS it
// on a many-core host (where GOMAXPROCS slots hold hundreds of MB per trained dictionary)
// and never RAISES it on a small host (<=4 cores keep their GOMAXPROCS default). 4 keeps
// ample parallelism for concurrent same-table commits while bounding memory.
const maxEncoderConcurrency = 4

// encoderConcurrency returns the capped concurrency for a new codec's encoder.
func encoderConcurrency() int {
	if n := runtime.GOMAXPROCS(0); n < maxEncoderConcurrency {
		return n
	}
	return maxEncoderConcurrency
}

// NewZSTDCodec returns a ZSTD codec. If dict is non-empty it is used as a shared
// compression dictionary (see TrainDict). Pass nil for dictionary-less ZSTD.
func NewZSTDCodec(dict []byte) (Codec, error) {
	// Cap encoder concurrency to bound the encoder's resident memory. A zstd.Encoder
	// preallocates one match-finder state -- including a large history buffer
	// (fastBase.ensureHist) -- PER concurrency slot, and the default is GOMAXPROCS. This
	// codec is shared; a commit compresses each ad with it, and concurrent same-table
	// commits (collector fan-out + many advertisers) do run Compress in parallel, so a
	// couple of slots earn their keep -- but GOMAXPROCS of them, each a multi-MB window
	// that stays resident once a slot is ever used, is pure waste on a many-core host. A
	// production heap profile showed the encoder path holding 310+ MB (>80% of live heap),
	// still creeping upward as slots warmed. Capping at min(GOMAXPROCS, 4) keeps enough
	// parallelism for the write path (compresses are microseconds, so a short burst drains
	// immediately) while hard-bounding memory at N*window per codec regardless of load or
	// core count. Decoders are left at the default: DecodeAll parallelism helps the
	// read/query path and did not show up as resident in the profile.
	eopts := []zstd.EOption{zstd.WithEncoderConcurrency(encoderConcurrency())}
	var dopts []zstd.DOption
	if len(dict) > 0 {
		eopts = append(eopts, zstd.WithEncoderDict(dict))
		dopts = append(dopts, zstd.WithDecoderDicts(dict))
	}
	enc, err := zstd.NewWriter(nil, eopts...)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, dopts...)
	if err != nil {
		return nil, err
	}
	return &zstdCodec{enc: enc, dec: dec, hasDict: len(dict) > 0, dopts: dopts}, nil
}

func (z *zstdCodec) Compress(dst, src []byte) []byte { return z.enc.EncodeAll(src, dst) }

func (z *zstdCodec) Decompress(dst, src []byte) ([]byte, error) { return z.dec.DecodeAll(src, dst) }

func (z *zstdCodec) Name() string {
	if z.hasDict {
		return "zstd+dict"
	}
	return "zstd"
}

// DefaultDictSize is the target size in bytes for a trained dictionary's content.
const DefaultDictSize = 112 * 1024

// TrainDict builds a ZSTD compression dictionary from sample records (the
// wire-encoded bytes of a representative set of ads; see CollectSamples). The
// resulting dictionary can be handed to NewZSTDCodec. It uses DefaultDictSize.
func TrainDict(samples [][]byte) ([]byte, error) {
	return TrainDictSize(samples, DefaultDictSize)
}

// TrainDictSize is TrainDict with an explicit dictionary content size.
//
// The pure-Go zstd.BuildDict does not perform ZDICT-style content *selection*
// (the cover algorithm); the dictionary content is whatever we supply as the
// builder's History. We therefore assemble the content ourselves by
// concatenating distinct samples up to dictSize — for a pool of similar ClassAds,
// this captures the shared attribute names and values that later ads back-
// reference. BuildDict then trains the entropy tables from the full sample set.
func TrainDictSize(samples [][]byte, dictSize int) (dict []byte, err error) {
	if len(samples) == 0 {
		return nil, errors.New("no samples")
	}
	// zstd.BuildDict can panic (integer divide by zero in its Huffman-table training)
	// on some degenerate sample distributions. Contain it so a public API
	// (RetrainDict) surfaces an error and keeps the current dictionary rather than
	// crashing the process.
	defer func() {
		if r := recover(); r != nil {
			dict, err = nil, fmt.Errorf("zstd BuildDict failed: %v", r)
		}
	}()
	// Assemble dictionary content from distinct samples, most-recent-first order
	// not being meaningful here; dedup exact duplicates to avoid wasting content
	// space on tiled/identical ads.
	seen := make(map[string]struct{}, len(samples))
	content := make([]byte, 0, dictSize)
	for _, s := range samples {
		if len(content) >= dictSize {
			break
		}
		if len(s) == 0 {
			continue
		}
		key := string(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		content = append(content, s...)
	}
	if len(content) > dictSize {
		content = content[:dictSize]
	}
	if len(content) < 8 {
		return nil, fmt.Errorf("insufficient distinct sample data (%d bytes)", len(content))
	}
	return zstd.BuildDict(zstd.BuildDictOptions{
		ID:       1, // a non-zero dictionary id
		Contents: samples,
		History:  content,
		// Standard zstd default recent-offset repcodes; {0,0,0} yields an invalid
		// dictionary ("invalid offset in dictionary").
		Offsets: [3]int{1, 4, 8},
	})
}

// PrefixDecompressor is an optional Codec capability: decompress only
// (approximately) the first want bytes of a record. With the hot region encoded
// as a physical prefix (see wire.EncodeInlineWithHotEnc), a hot-covered
// projected read decompresses a couple of KB instead of the whole record. The
// result may be shorter than want (the record ends first -- then it is the
// complete record) and is otherwise a TRUNCATED record: the caller must be
// prepared for parses to run off the end and fall back to a full Decompress.
type PrefixDecompressor interface {
	DecompressPrefix(dst, src []byte, want int) ([]byte, error)
}

// streamDec is one pooled streaming decoder (Reset-per-record) plus its reader.
// zstd's DecodeAll is concurrency-safe but streaming decode is not, so each
// concurrent prefix read takes its own decoder from the pool.
type streamDec struct {
	dec *zstd.Decoder
	br  *bytes.Reader
}

func (z *zstdCodec) DecompressPrefix(dst, src []byte, want int) ([]byte, error) {
	v := z.spool.Get()
	var sd *streamDec
	if v == nil {
		dopts := append([]zstd.DOption{zstd.WithDecoderConcurrency(1)}, z.dopts...)
		dec, err := zstd.NewReader(nil, dopts...)
		if err != nil {
			return dst, err
		}
		sd = &streamDec{dec: dec, br: new(bytes.Reader)}
	} else {
		sd = v.(*streamDec)
	}
	sd.br.Reset(src)
	if err := sd.dec.Reset(sd.br); err != nil {
		z.spool.Put(sd)
		return dst, err
	}
	off := len(dst)
	if cap(dst)-off < want {
		dst = append(dst, make([]byte, want)...)
	} else {
		dst = dst[:off+want]
	}
	n, err := io.ReadFull(sd.dec, dst[off:])
	dst = dst[:off+n]
	z.spool.Put(sd)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil // the whole record was shorter than want: dst holds all of it
	}
	return dst, err
}
