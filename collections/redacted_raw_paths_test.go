package collections

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Every raw and wire-row read that serves a REDACTED caller, over every stored form a sealed value
// reaches a reader in, must never emit the plaintext of a sealed value and must never use the key.
//
// The bytes each read emits are searched for the plaintext directly -- not a decoded value, which
// would miss a secret carried under another attribute or inside an expression -- and the sealed
// attribute must render as `undefined` wherever it is rendered at all. A private attribute must not
// appear in any form.
//
// "dropped" is the shape that leaked through the plain raw scans on interned segments: an attribute
// sealed at rest and later taken out of EncryptedAttrs. Its stored values stay sealed (migration only
// ever seals), and a read that opened every sealed value and re-sealed only what the CURRENT set names
// put that plaintext in front of the redacting renderer. A read that holds no key cannot.

const (
	redactNoteSecret  = "NOTESECRET"
	redactClaimSecret = "CLAIMSECRET"
	redactRawN        = 900
)

func redactRawAd(i int) string {
	return fmt.Sprintf(`[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=0; Owner="u%d";`+
		` Note="%s-%d"; ClaimId="%s-%d"; Pad="%0128d" ]`, i, i%7, redactNoteSecret, i, redactClaimSecret, i, i)
}

type redactRawShape struct {
	name     string
	interned bool // RetrainDict: sealed segments keyed by their own dictionary
	columnar bool // schema scan on and sealed segments columnarized
	dropped  bool // Note taken out of EncryptedAttrs after it was sealed
}

var redactRawShapes = []redactRawShape{
	{name: "inline"},
	{name: "interned", interned: true},
	{name: "columnar", columnar: true},
	{name: "interned-columnar", interned: true, columnar: true},
	{name: "interned-dropped", interned: true, dropped: true},
	{name: "interned-columnar-dropped", interned: true, columnar: true, dropped: true},
}

func redactRawFixture(t *testing.T, s redactRawShape) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 15,
		DataKey: sealedRefKey(), EncryptedAttrs: []string{"Note"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, redactRawAd(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(0, redactRawN*3/4)
	if s.interned {
		if _, err := c.RetrainDict(2000); err != nil {
			t.Fatal(err)
		}
	}
	if s.columnar {
		if !c.BuildAndEnableSchemaScan(2000, 8) {
			t.Fatal("no columnar schema derived")
		}
		c.ColumnarizeSealed()
		if columnarSegmentsForTest(c) == 0 {
			t.Fatal("fixture: no columnarized segment")
		}
	}
	// An active tail too, so every read also meets records that were never rewritten.
	put(redactRawN*3/4, redactRawN)
	if s.dropped {
		if err := c.SetEncryptedAttrs(nil); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// redactRawRead is one read path: run it and return every row it emitted, as the bytes it emitted.
type redactRawRead struct {
	name string
	run  func(c *Collection, redact bool) [][]byte
	// wire rows are inline wire; the text check renders them first.
	wire bool
}

func rawAdBytes(ra RawAd) []byte {
	var b []byte
	for _, e := range ra.Exprs {
		b = append(b, e...)
		b = append(b, '\n')
	}
	b = append(b, ra.MyType...)
	b = append(b, '\n')
	return append(b, ra.TargetType...)
}

func redactRawReads(t *testing.T) []redactRawRead {
	q, err := vm.Parse(`ClusterId >= 0`)
	if err != nil {
		t.Fatal(err)
	}
	proj := []string{"ClusterId", "Owner", "Note", "ClaimId"}
	text := func(seq func(c *Collection, redact bool) func(func(RawAd) bool)) func(*Collection, bool) [][]byte {
		return func(c *Collection, redact bool) [][]byte {
			var out [][]byte
			for ra := range seq(c, redact) {
				out = append(out, rawAdBytes(ra))
			}
			return out
		}
	}
	rows := func(seq func(c *Collection, redact bool) func(func([]byte) bool)) func(*Collection, bool) [][]byte {
		return func(c *Collection, redact bool) [][]byte {
			var out [][]byte
			for w := range seq(c, redact) {
				out = append(out, append([]byte(nil), w...))
			}
			return out
		}
	}
	reads := []redactRawRead{
		{name: "ScanRaw", run: text(func(c *Collection, redact bool) func(func(RawAd) bool) {
			if redact {
				return c.ScanRawRedacted()
			}
			return c.ScanRaw()
		})},
		{name: "QueryRaw", run: text(func(c *Collection, redact bool) func(func(RawAd) bool) {
			if redact {
				return c.QueryRawRedacted(q)
			}
			return c.QueryRaw(q)
		})},
		{name: "QueryRawFromSeq", run: func(c *Collection, redact bool) [][]byte {
			// Paged in small pages, so the cursor walk is exercised and not just one page.
			var out [][]byte
			var after SeqCursor
			for {
				var seq func(func(RawAd) bool)
				var page *SeqPage
				if redact {
					seq, page = c.QueryRawFromSeqRedacted(q, proj, after, 97)
				} else {
					seq, page = c.QueryRawFromSeq(q, proj, after, 97)
				}
				for ra := range seq {
					out = append(out, rawAdBytes(ra))
				}
				if page.Err != nil {
					t.Fatal(page.Err)
				}
				if !page.More {
					return out
				}
				after = page.Next
			}
		}},
	}
	for _, p := range []struct {
		name string
		proj []string
	}{{"whole", nil}, {"projected", proj}} {
		for _, chase := range []bool{false, true} {
			sfx := fmt.Sprintf("/%s/chase=%v", p.name, chase)
			reads = append(reads,
				redactRawRead{name: "ScanRawProjected" + sfx, run: text(func(c *Collection, redact bool) func(func(RawAd) bool) {
					return c.ScanRawProjected(p.proj, chase, redact)
				})},
				redactRawRead{name: "QueryRawProjected" + sfx, run: text(func(c *Collection, redact bool) func(func(RawAd) bool) {
					return c.QueryRawProjected(q, p.proj, chase, redact)
				})})
		}
		for _, parallel := range []bool{false, true} {
			sfx := fmt.Sprintf("/%s/parallel=%v", p.name, parallel)
			// The parallel and serial emitters are separate code; pick each rather than hope for it.
			set := func(c *Collection) {
				if parallel {
					c.parallelMinBytes = 0
				} else {
					c.parallelMinBytes = 1 << 40
				}
			}
			reads = append(reads,
				redactRawRead{name: "ScanRawWire" + sfx, wire: true, run: rows(func(c *Collection, redact bool) func(func([]byte) bool) {
					set(c)
					return c.ScanRawWire(p.proj, redact)
				})},
				redactRawRead{name: "QueryRawWire" + sfx, wire: true, run: rows(func(c *Collection, redact bool) func(func([]byte) bool) {
					set(c)
					return c.QueryRawWire(q, p.proj, redact)
				})},
				redactRawRead{name: "ScanRawWireRefs" + sfx, wire: true, run: rows(func(c *Collection, redact bool) func(func([]byte) bool) {
					set(c)
					return c.ScanRawWireRefs(p.proj, redact)
				})},
				redactRawRead{name: "QueryRawWireRefs" + sfx, wire: true, run: rows(func(c *Collection, redact bool) func(func([]byte) bool) {
					set(c)
					return c.QueryRawWireRefs(q, p.proj, redact)
				})})
		}
	}
	return reads
}

// rowExprs renders one emitted row to its "Name = Value" expressions.
func rowExprs(t *testing.T, row []byte, wire bool) []string {
	t.Helper()
	if !wire {
		return strings.Split(string(row), "\n")
	}
	buf, offs, mt, tt, ok := RenderRawAdInline(row, nil, nil)
	if !ok {
		t.Fatal("RenderRawAdInline failed on an emitted wire row")
	}
	out := []string{mt, tt}
	for i := 0; i+1 < len(offs); i++ {
		out = append(out, string(buf[offs[i]:offs[i+1]]))
	}
	return out
}

func TestRedactedRawReadsNeverEmitPlaintext(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	for _, shape := range redactRawShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			c := redactRawFixture(t, shape)
			ctr := &countingSealer{inner: c.sealer}
			c.sealer = ctr
			for _, rd := range redactRawReads(t) {
				t.Run(rd.name, func(t *testing.T) {
					// Premise: the entitled read of the same path does carry the Note plaintext, so a
					// redacted read without it is withholding something rather than reading nothing.
					priv := rd.run(c, false)
					if len(priv) != redactRawN {
						t.Fatalf("entitled read returned %d rows, want %d", len(priv), redactRawN)
					}
					sawNote := 0
					for _, row := range priv {
						if bytes.Contains(row, []byte(redactNoteSecret)) {
							sawNote++
						}
					}
					if sawNote != redactRawN {
						t.Fatalf("entitled read carried the Note plaintext in %d of %d rows; the premise fails",
							sawNote, redactRawN)
					}

					ctr.opens.Store(0)
					ctr.seals.Store(0)
					red := rd.run(c, true)
					if n := ctr.opens.Load(); n != 0 {
						t.Errorf("redacted read opened %d sealed values; it must hold no key", n)
					}
					if n := ctr.seals.Load(); n != 0 {
						t.Errorf("redacted read sealed %d values; a read seals nothing", n)
					}
					if len(red) != redactRawN {
						t.Errorf("redacted read returned %d rows, want %d: redaction withholds values, not ads",
							len(red), redactRawN)
					}
					leaks := 0
					for _, row := range red {
						if bytes.Contains(row, []byte(redactNoteSecret)) || bytes.Contains(row, []byte(redactClaimSecret)) {
							leaks++
							if leaks == 1 {
								t.Errorf("redacted read emitted plaintext: %q", row)
							}
							continue
						}
						notes := 0
						for _, e := range rowExprs(t, row, rd.wire) {
							switch {
							case strings.HasPrefix(e, "ClaimId"):
								t.Fatalf("redacted read emitted a private attribute: %q", e)
							case strings.HasPrefix(e, "Note"):
								notes++
								if e != "Note = undefined" {
									t.Fatalf("redacted read rendered the sealed attribute as %q, want undefined", e)
								}
							}
						}
						// A text row renders the sealed attribute as undefined. A wire row drops it instead
						// (the assembler has no value to ship), which reads the same at the far end.
						if notes != 1 && (notes != 0 || !rd.wire) {
							t.Fatalf("redacted row rendered Note %d times, want once as undefined: %q", notes, row)
						}
					}
					if leaks != 0 {
						t.Errorf("%d of %d redacted rows carried plaintext", leaks, len(red))
					}
				})
			}
		})
	}
}
