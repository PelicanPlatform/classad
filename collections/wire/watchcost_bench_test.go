package wire_test

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// The watch feed hands the RPC server a *classad.ClassAd and it renders text the consumer
// parses back. These measure the two halves of that against the wire alternative:
// server-side render vs encode, and consumer-side parse vs decode.

// watchAdText is a job ad of the width a change feed actually carries.
const watchAdText = `[ ClusterId = 1234; ProcId = 7; Owner = "alice"; JobStatus = 2; QDate = 1700000000;
RequestMemory = 2048; RequestCpus = 4; RemoteHost = "slot1@node42.example.edu";
Cmd = "/home/alice/run.sh"; Iwd = "/home/alice/work"; JobUniverse = 5;
RemoteWallClockTime = 12345.0; NumJobStarts = 1; ExitCode = 0 ]`

func watchAd(tb testing.TB) *classad.ClassAd {
	tb.Helper()
	ad, err := classad.Parse(watchAdText)
	if err != nil {
		tb.Fatal(err)
	}
	return ad
}

// BenchmarkWatchRenderText is the server side today: ClassAd -> new-ClassAd text.
func BenchmarkWatchRenderText(b *testing.B) {
	ad := watchAd(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if ad.String() == "" {
			b.Fatal("empty")
		}
	}
}

// BenchmarkWatchEncodeWire is the server side with a wire-form watch: ClassAd -> wire.
func BenchmarkWatchEncodeWire(b *testing.B) {
	ad := watchAd(b)
	var buf []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = wire.EncodeInline(buf[:0], ad.AST())
		if len(buf) == 0 {
			b.Fatal("empty")
		}
	}
}

// BenchmarkWatchConsumerParse is what every exporter pays today.
func BenchmarkWatchConsumerParse(b *testing.B) {
	text := watchAd(b).String()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := classad.Parse(text); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWatchConsumerDecode is what it would pay on a wire-form watch.
func BenchmarkWatchConsumerDecode(b *testing.B) {
	w := wire.EncodeInline(nil, watchAd(b).AST())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		node, err := wire.DecodeInline(w)
		if err != nil {
			b.Fatal(err)
		}
		if classad.FromAST(node) == nil {
			b.Fatal("nil")
		}
	}
}
