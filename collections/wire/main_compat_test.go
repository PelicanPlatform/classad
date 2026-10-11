package wire

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// TestMainEncodingsDecode guards the durable encoding across the lexer changes
// that made the parser follow libclassad (signed number literals, raw string
// bytes, ...). testdata/main_encodings.txt holds inline encodings that the code
// before those changes (origin/main at 42dd2bf) wrote for a set of ads, with
// the String() it decoded them to. They must decode to the same text, and
// parsing the same source now must produce the same bytes.
func TestMainEncodingsDecode(t *testing.T) {
	f, err := os.Open("testdata/main_encodings.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			t.Fatalf("malformed fixture line %q", line)
		}
		src, err1 := strconv.Unquote(fields[1])
		enc, err2 := hex.DecodeString(fields[2])
		want, err3 := strconv.Unquote(fields[3])
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("malformed fixture line %q", line)
		}
		n++

		ad, err := DecodeInline(enc)
		if err != nil {
			t.Errorf("%q: decoding the stored bytes: %v", src, err)
			continue
		}
		if got := ad.String(); got != want {
			t.Errorf("%q: stored bytes decode to\n  %s\nwant\n  %s", src, got, want)
		}

		var reenc []byte
		switch fields[0] {
		case "old":
			cad, perr := classad.ParseOld(src)
			if perr != nil {
				t.Errorf("%q: %v", src, perr)
				continue
			}
			reenc = EncodeInline(nil, cad.AST())
		case "new":
			nad, perr := parser.ParseClassAd(src)
			if perr != nil {
				t.Errorf("%q: %v", src, perr)
				continue
			}
			reenc = EncodeInline(nil, nad)
		}
		if !bytes.Equal(reenc, enc) {
			t.Errorf("%q: encodes to\n  %x\nnow, was\n  %x", src, reenc, enc)
		}
	}
	if n == 0 {
		t.Fatal("no fixtures")
	}
}
