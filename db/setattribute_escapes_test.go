package db

import (
	"testing"
)

// TestSetAttributeUsesOldClassAdEscapes pins the parser SetAttribute uses. Its inputs are
// job_queue.log values, which the schedd itself reads with ParseClassAdRvalExpr -- a parser
// with SetOldClassAd(true) -- so string literals get NO escape interpretation and keep their
// bytes (Lexer::tokenizeStringOld).
//
// This was parsed with new-ClassAd rules instead, which has two failure modes, and the
// second is the dangerous one. Before the Go lexer was made lenient, an undefined escape
// was rejected and the attribute was skipped outright: every OSPool job carrying a host
// blocklist lost its Requirements. With a lenient lexer the write succeeds and silently
// DELETES the backslashes, turning regexp("a\.b\.c") into a pattern whose '.' matches any
// character -- a quietly wider match, stored as if it were the job's own expression.
//
// wantUnparse doubles each backslash because that is the unparser re-escaping a literal
// backslash on the way out (sink.cpp UnparseString); the stored VALUE is the single
// backslash the submitter wrote, which wantValue checks directly.
func TestSetAttributeUsesOldClassAdEscapes(t *testing.T) {
	cases := []struct {
		name        string
		expr        string
		wantUnparse string
		wantValue   string // for a bare string literal: the decoded value
	}{
		{
			name:        "OSPool host blocklist regex keeps its backslashes",
			expr:        `!regexp("warlock21\.beocat\.ksu\.edu|wsu\-lg02\.osris\.org", TARGET.Machine)`,
			wantUnparse: `(!regexp("warlock21\\.beocat\\.ksu\\.edu|wsu\\-lg02\\.osris\\.org", TARGET.Machine))`,
		},
		{
			name:        "TransferInput name keeps its escaped comma and space",
			expr:        `"osdf:///chtc/staging/testing\,\ more"`,
			wantUnparse: `"osdf:///chtc/staging/testing\\,\\ more"`,
			wantValue:   `osdf:///chtc/staging/testing\,\ more`,
		},
		{
			name:        "agetty issue string survives",
			expr:        `"\S \r (\l)"`,
			wantUnparse: `"\\S \\r (\\l)"`,
			wantValue:   `\S \r (\l)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openTxnDB(t)
			tx := d.Begin()
			defer tx.Abort()
			tx.NewClassAd("j", txnOwnerAd("alice", 8))
			if err := tx.SetAttribute("j", "Requirements", tc.expr); err != nil {
				t.Fatalf("SetAttribute rejected a real job_queue.log value: %v", err)
			}
			ad, ok := tx.LookupClassAd("j")
			if !ok {
				t.Fatal("ad not found after SetAttribute")
			}
			e, ok := ad.Lookup("Requirements")
			if !ok {
				t.Fatal("Requirements not set")
			}
			if got := e.String(); got != tc.wantUnparse {
				t.Errorf("stored expression = %s, want %s", got, tc.wantUnparse)
			}
			if tc.wantValue != "" {
				got, ok := ad.EvaluateAttrString("Requirements")
				if !ok {
					t.Fatal("Requirements did not evaluate to a string")
				}
				if got != tc.wantValue {
					t.Errorf("stored value = %q, want %q", got, tc.wantValue)
				}
			}
		})
	}
}
