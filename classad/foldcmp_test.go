package classad

import (
	"math/rand"
	"strings"
	"testing"
)

// TestFoldCompareMatchesToLower pins the allocation-free comparator to the reference
// ordering it replaced: strings.Compare over both names lowercased.
func TestFoldCompareMatchesToLower(t *testing.T) {
	names := []string{
		"", "a", "A", "ab", "AB", "aB", "Ab", "abc", "ABC",
		"JobStatus", "jobstatus", "JOBSTATUS", "JobStatus2", "JobStatu",
		"_underscore", "Z", "z", "[", "{", "@", "`",
		"Ünicode", "ünicode", "İstanbul", "istanbul", "straße", "STRASSE",
		"日本語", "a日", "A日", "KelvinK", "kelvink",
	}
	for _, a := range names {
		for _, b := range names {
			want := strings.Compare(strings.ToLower(a), strings.ToLower(b))
			got := foldCompare(a, b)
			if (want < 0) != (got < 0) || (want == 0) != (got == 0) || (want > 0) != (got > 0) {
				t.Errorf("foldCompare(%q, %q) = %d, want sign of %d", a, b, got, want)
			}
		}
	}
}

func TestFoldCompareRandom(t *testing.T) {
	const alphabet = "aAbBzZ_019[]{}@`~éÉİ"
	r := rand.New(rand.NewSource(1))
	pick := func() string {
		var sb strings.Builder
		for n := r.Intn(8); n > 0; n-- {
			sb.WriteString(string(alphabet[r.Intn(len(alphabet))]))
		}
		return sb.String()
	}
	for i := 0; i < 200000; i++ {
		a, b := pick(), pick()
		want := strings.Compare(strings.ToLower(a), strings.ToLower(b))
		got := foldCompare(a, b)
		if (want < 0) != (got < 0) || (want == 0) != (got == 0) {
			t.Fatalf("foldCompare(%q, %q) = %d, want sign of %d", a, b, got, want)
		}
	}
}

func TestFoldCompareNoAlloc(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() {
		_ = foldCompare("JobStatus", "ClusterId")
		_ = foldCompare("RemoteUserCpu", "RemoteUserCpX")
	}); n != 0 {
		t.Errorf("foldCompare allocated %v times per run, want 0", n)
	}
}
