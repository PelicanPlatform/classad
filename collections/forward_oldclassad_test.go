package collections

import (
	"strings"
	"testing"

	classad "github.com/PelicanPlatform/classad/classad"
)

// TestForwardOldClassAdBackslashLiteral guards the collector's forwarding path.
// A startd ad's OSIssue = "\S" (backslash-S, the agetty /etc/issue escape) is,
// in old-ClassAd, the two bytes `\` and `S`. When the collector forwards the ad
// it must re-serialize that value with old-ClassAd rules (backslash literal), not
// new-ClassAd rules (which double it to "\\S"). The doubling corrupts the value
// on every hop and, for a value ending in a backslash, produces an unterminated
// string the receiving daemon rejects -- surfacing as forwarding connection
// resets. See ast.AppendQuoteStringOld.
func TestForwardOldClassAdBackslashLiteral(t *testing.T) {
	in := "MyType = \"Machine\"\nName = \"x\"\nOSIssue = \"\\S\"\n"

	c := New(Options{Shards: 1})
	if err := c.UpdateOld([]OldAdUpdate{{Key: []byte("k"), Text: in}}); err != nil {
		t.Fatalf("UpdateOld: %v", err)
	}

	var line string
	for ra := range c.ScanRaw() {
		for _, e := range ra.Exprs {
			if strings.HasPrefix(string(e), "OSIssue ") {
				line = string(e)
			}
		}
		break
	}
	if want := `OSIssue = "\S"`; line != want {
		t.Errorf("forwarded OSIssue = %q, want %q (backslash must stay literal)", line, want)
	}
	// The forwarded ad must re-parse, or a downstream would reset the connection.
	if _, err := classad.ParseOld(line + "\n"); err != nil {
		t.Errorf("forwarded line does not re-parse: %v", err)
	}
}
