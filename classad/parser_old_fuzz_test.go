package classad

import (
	"sort"
	"strings"
	"testing"
)

// oldFormatFuzzSeeds are old-ClassAd sources (newline-separated `Name = Expr`, no
// brackets) exercising the ParseOld -> MarshalOld path -- including attribute names that
// are NOT bare identifiers, which real ads carry (per-user schedd stats like
// "user:condor_tail_X") and which the new-format lexer would reject unless quoted. This
// path was previously unfuzzed, so the colon-name rejection slipped through.
var oldFormatFuzzSeeds = []string{
	"",
	"Name = \"login06\"\nCpus = 8",
	"MyType = \"Machine\"\nRequirements = Cpus > 4 && Memory > 1024",
	// Non-bare attribute names taken verbatim by the old format:
	"acwarden:condor_tail_FileTransferDownloadBytes = 984.0\nName = \"login06\"",
	"acwarden:condor_tail_FileTransferDownloadBytesPerSecond_1h = 0.002624417388097289",
	"a.b.c = 1\nName = \"x\"",
	"Weird-Name = 2\nOk = 3",
	"'already quoted' = 1",
}

// FuzzParseOldRoundTrip checks the old-format ATTRIBUTE-NAME round trip -- the dimension
// that regressed in the field. On any input ParseOld must not panic/hang; when it parses,
// its MarshalOld rendering must (a) re-parse via ParseOld and (b) preserve the exact set of
// attribute names. That catches both a name the parser wrongly rejects and a name MarshalOld
// wrongly quotes (which the C++ old-format never does, and which would come back changed).
//
// It deliberately does NOT assert byte-exact value round-tripping: string-VALUE escaping
// (control bytes, backslashes) round-trips asymmetrically between MarshalOld's strict
// escaping and the lexer's lenient old-format escaping -- a separate, pre-existing issue
// tracked on its own, out of scope for the attribute-name path.
func FuzzParseOldRoundTrip(f *testing.F) {
	for _, s := range oldFormatFuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		ad1, err := ParseOld(src)
		if err != nil {
			return // rejecting malformed input is fine; the point is it must not panic
		}
		if ad1 == nil {
			t.Fatalf("ParseOld returned nil ad and nil error for %q", src)
		}
		m1 := ad1.MarshalOld()
		ad2, err := ParseOld(m1)
		if err != nil {
			t.Fatalf("MarshalOld output does not re-parse via ParseOld:\n  src: %q\n  m1:  %q\n  err: %v", src, m1, err)
		}
		n1, n2 := sortedAttrNames(ad1), sortedAttrNames(ad2)
		if strings.Join(n1, "\x00") != strings.Join(n2, "\x00") {
			t.Fatalf("attribute names not preserved through MarshalOld:\n  src: %q\n  m1:  %q\n  before: %q\n  after:  %q", src, m1, n1, n2)
		}
	})
}

func sortedAttrNames(ad *ClassAd) []string {
	names := ad.GetAttributes()
	sort.Strings(names)
	return names
}

// TestParseOldColonAttrName is the direct regression for the field report: a Schedd ad
// with per-user stat attributes whose names contain ':' must parse (not be dropped), keep
// their values, and re-serialize verbatim so C++ clients read the same attribute.
func TestParseOldColonAttrName(t *testing.T) {
	const attr = "acwarden:condor_tail_FileTransferDownloadBytes"
	ad, err := ParseOld("Name = \"login06.hep.wisc.edu\"\n" + attr + " = 984.0\nOther = 3")
	if err != nil {
		t.Fatalf("ParseOld rejected a colon attribute name (the reported failure): %v", err)
	}
	if v, ok := ad.EvaluateAttrReal(attr); !ok || v != 984.0 {
		t.Fatalf("%s = %v (ok=%v), want 984.0", attr, v, ok)
	}
	out := ad.MarshalOld()
	if !strings.Contains(out, attr+" = ") {
		t.Fatalf("MarshalOld did not emit the colon name verbatim:\n%s", out)
	}
	if strings.Contains(out, "'"+attr) {
		t.Fatalf("MarshalOld quoted the colon name, which breaks C++ old-format wire clients:\n%s", out)
	}
}
