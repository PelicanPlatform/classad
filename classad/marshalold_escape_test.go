package classad

import (
	"strings"
	"testing"
)

// TestMarshalOldStringEscaping verifies MarshalOld uses old-ClassAd string escaping
// (only the delimiter is escaped; backslashes and control characters are emitted verbatim,
// matching C++ ClassAdUnParser with SetOldClassAd and the lenient old-format lexer). The
// bug was strict new-format escaping, which grew a backslash on every parse->marshal hop
// (a Windows path or regex value gaining `\` each time it was served).
func TestMarshalOldStringEscaping(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// wantContains is a substring the MarshalOld output must contain verbatim.
		wantContains string
	}{
		{"windows path", `Path = "C:\bin\condor"`, `Path = "C:\bin\condor"`},
		{"regex backslash", `Pat = "host\d+\.example"`, `Pat = "host\d+\.example"`},
		{"escaped quote", `Q = "say \"hi\""`, `Q = "say \"hi\""`},
		{"nested in expr", `Req = Machine == "C:\slot"`, `Machine == "C:\slot"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad, err := ParseOld(tc.src)
			if err != nil {
				t.Fatalf("ParseOld(%q): %v", tc.src, err)
			}
			out := ad.MarshalOld()
			if !strings.Contains(out, tc.wantContains) {
				t.Fatalf("MarshalOld = %q, want it to contain %q (old-format escaping)", out, tc.wantContains)
			}
		})
	}
}

// TestMarshalOldRoundTripIdempotent checks ParseOld -> MarshalOld is idempotent for values
// with backslashes and nested string literals -- the escape asymmetry the fuzzer found.
func TestMarshalOldRoundTripIdempotent(t *testing.T) {
	srcs := []string{
		`X = "C:\foo"`,
		`X = "a\\b"`,
		`X = "re\d+"`,
		`Y = "plain"`,
		`Req = (OpSys == "LINUX") && (Path == "C:\bin")`,
		`Name = "slot1@h"` + "\n" + `Regexp = "^user\d+$"`,
	}
	for _, src := range srcs {
		ad, err := ParseOld(src)
		if err != nil {
			t.Fatalf("ParseOld(%q): %v", src, err)
		}
		m1 := ad.MarshalOld()
		ad2, err := ParseOld(m1)
		if err != nil {
			t.Fatalf("re-parse of %q failed: %v", m1, err)
		}
		if m2 := ad2.MarshalOld(); m1 != m2 {
			t.Fatalf("MarshalOld not idempotent:\n  src: %q\n  m1:  %q\n  m2:  %q", src, m1, m2)
		}
	}
}
