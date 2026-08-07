package wire_test

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

const wideAd = `[ ClusterId = 1234; ProcId = 7; Owner = "alice"; JobStatus = 2; QDate = 1700000000;
RequestMemory = 2048; RequestCpus = 4; RemoteHost = "slot1@node42.example.edu";
Cmd = "/home/alice/run.sh"; Iwd = "/home/alice/work"; JobUniverse = 5;
RemoteWallClockTime = 12345.0; NumJobStarts = 1; ExitCode = 0 ]`

// narrowAd is what a projected text fetch returns: the three attributes a grouped query reads.
const narrowAd = `[ Owner = "alice"; JobStatus = 2; RequestMemory = 2048 ]`

func mustWire(tb testing.TB, text string) []byte {
	tb.Helper()
	ad, err := classad.Parse(text)
	if err != nil {
		tb.Fatal(err)
	}
	return wire.EncodeInline(nil, ad.AST())
}

func oldText(tb testing.TB, text string) string {
	tb.Helper()
	ad, err := classad.Parse(text)
	if err != nil {
		tb.Fatal(err)
	}
	return strings.TrimSpace(ad.MarshalOld())
}

// benchDecode measures turning one relayed row into the *classad.ClassAd a consumer evaluates.
func benchDecode(b *testing.B, w []byte) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ad, err := wire.DecodeInline(w)
		if err != nil {
			b.Fatal(err)
		}
		if classad.FromAST(ad) == nil {
			b.Fatal("nil")
		}
	}
}

func benchParse(b *testing.B, text string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := classad.ParseOld(text); err != nil {
			b.Fatal(err)
		}
	}
}

// The question these four answer: is decoding a WHOLE wire ad cheaper than parsing a
// NARROW projected text ad? If it is, a consumer can drop the text projection entirely --
// which also restores whole-ad evaluation semantics, since every sibling is present.
func BenchmarkTextParseWide(b *testing.B)   { benchParse(b, oldText(b, wideAd)) }
func BenchmarkTextParseNarrow(b *testing.B) { benchParse(b, oldText(b, narrowAd)) }
func BenchmarkWireDecodeWide(b *testing.B)  { benchDecode(b, mustWire(b, wideAd)) }
func BenchmarkWireDecodeNarrow(b *testing.B) {
	benchDecode(b, mustWire(b, narrowAd))
}
