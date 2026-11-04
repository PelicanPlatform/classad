package dbrpc

import (
	"context"
	"fmt"
	"testing"
	"unsafe"

	"github.com/PelicanPlatform/classad/db"

	"github.com/PelicanPlatform/classad/classad"
)

// collectDictValues runs the value stream with the dictionary form requested, returning the rows
// keyed by ClusterId and the kinds each column arrived as.
func collectDictValues(t *testing.T, c *Client, table string, attrs []string, dict bool) (map[string][]classad.Value, map[int]ValueKind) {
	t.Helper()
	out := map[string][]classad.Value{}
	kinds := map[int]ValueKind{}
	err := c.QueryValuesStreamOpts(context.Background(), table, "true", attrs, 0, false,
		ValueStreamOptions{Dictionary: dict}, func(b *ValueBatch) bool {
			for col := range b.Cols {
				kinds[col] = b.Cols[col].Kind
			}
			for row := range b.Rows {
				vals := make([]classad.Value, len(b.Cols))
				for col := range b.Cols {
					vals[col] = b.ValueAt(col, row)
				}
				out[vals[0].String()] = vals
			}
			return true
		})
	if err != nil {
		t.Fatalf("QueryValuesStreamOpts(dict=%v): %v", dict, err)
	}
	return out, kinds
}

// dictFixture is valuesFixture with the client told what the server advertises.
//
// The fixture's own client learns no features over a bare pipe, and FeatValueDict postdates
// advertisement -- so silence is a no and the client declines to ask for the form under test. A
// real deployment has them: the in-process loopback builds its peer attributes from
// ServerSessionAttrs and refuses to start if one is missing.
//
// A SECOND client on the fixture's existing connection is not the way to do this. Both would read
// the same frames and steal each other's replies, which hangs rather than fails.
func dictFixture(t *testing.T) (*Client, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mut, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < valuesN; i++ {
		text := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nResidentSetSize = %d\n"+
			"MemoryUsage = (ResidentSetSize + 1023) / 1024\nTags = { \"a\", \"b\" }\n",
			i, i%4, 1024*(1+i%5))
		if i%17 == 0 {
			text += fmt.Sprintf("RequestMemory = \"%dM\"\n", 512*(1+i%3))
		} else {
			text += fmt.Sprintf("RequestMemory = %d\n", 512*(1+i%3))
		}
		if i%11 == 0 {
			text += "Missing = 7\n"
		}
		ad, perr := classad.ParseOld(text)
		if perr != nil {
			t.Fatal(perr)
		}
		if err := mut.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
			t.Fatal(err)
		}
		if err := arch.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(s.SessionAttrs()))
	return c, func() { c.Close(); s.Close(); cat.Close() }
}

// TestStringDictionaryCarriesTheSameValues: the dictionary form is an encoding of the same
// column, so it must decode to exactly what the plain form decodes to -- value for value,
// including the cells that are not strings at all and ship as exceptions either way.
//
// The kinds are asserted too, because agreeing with the plain form is also true of a server that
// quietly ignored the flag, which is the way this test would stop testing anything.
func TestStringDictionaryCarriesTheSameValues(t *testing.T) {
	c, cleanup := dictFixture(t)
	defer cleanup()
	attrs := []string{"ClusterId", "Owner", "RequestMemory", "MemoryUsage", "Tags", "Missing"}

	for _, table := range valuesTables {
		t.Run(table, func(t *testing.T) {
			plain, plainKinds := collectDictValues(t, c, table, attrs, false)
			dict, dictKinds := collectDictValues(t, c, table, attrs, true)

			if plainKinds[1] != KindString {
				t.Fatalf("Owner arrived as %v without the flag, want string", plainKinds[1])
			}
			if dictKinds[1] != KindStringDict {
				t.Fatalf("Owner arrived as %v WITH the flag, want string-dict -- the server "+
					"ignored it and this compares one form with itself", dictKinds[1])
			}
			if len(plain) == 0 || len(plain) != len(dict) {
				t.Fatalf("plain returned %d rows, dictionary %d", len(plain), len(dict))
			}
			for key, want := range plain {
				got, ok := dict[key]
				if !ok {
					t.Fatalf("the dictionary form omitted ClusterId=%s", key)
				}
				for col, name := range attrs {
					if g, same := sameValue(got[col], want[col]); !same {
						t.Errorf("ClusterId=%s %s: dictionary %q, plain %q",
							key, name, g, want[col].String())
					}
				}
			}
		})
	}
}

// TestStringDictionaryDeclinesWhereItWouldNotPay: a code is four bytes per cell whatever the
// strings cost, so a column of long, nearly-distinct values would ship a dictionary AND a code
// for every row -- more bytes than the plain form, to save allocations the receiver was going to
// make anyway.
func TestStringDictionaryDeclinesWhereItWouldNotPay(t *testing.T) {
	vals := make([]classad.Value, 0, 500)
	for i := range 500 {
		vals = append(vals, classad.NewStringValue(fmt.Sprintf("a-very-long-and-distinct-value-%d", i)))
	}
	if codes, dict := newValueEncoder(1).buildStringDict(vals); dict != nil {
		t.Errorf("a column of %d distinct values took the dictionary form (%d entries, %d codes)",
			len(vals), len(dict), len(codes))
	}

	// And it DOES take it where it pays, or the test above passes for a builder that never
	// returns one.
	repeat := make([]classad.Value, 0, 500)
	for i := range 500 {
		repeat = append(repeat, classad.NewStringValue(fmt.Sprintf("user%d", i%8)))
	}
	codes, dict := newValueEncoder(1).buildStringDict(repeat)
	if dict == nil {
		t.Fatal("a column of 8 distinct values over 500 rows did not take the dictionary form")
	}
	if len(dict) != 8 {
		t.Errorf("dictionary has %d entries, want 8", len(dict))
	}
	if len(codes) != 500 {
		t.Errorf("got %d codes, want one per cell (500)", len(codes))
	}
}

// TestStringDictionaryIsNotSentUnlessAsked: an older decoder refuses a kind it does not know, so
// the server must never send this form to a client that did not ask. That is the whole
// compatibility story and it is one line of server code, which is exactly the kind of line that
// gets inverted.
func TestStringDictionaryIsNotSentUnlessAsked(t *testing.T) {
	c, cleanup := dictFixture(t)
	defer cleanup()
	_, kinds := collectDictValues(t, c, "jobs", []string{"ClusterId", "Owner"}, false)
	for col, k := range kinds {
		if k == KindStringDict {
			t.Errorf("column %d arrived as string-dict without the flag being set", col)
		}
	}
}

// TestInternCopiesOncePerDistinctValue pins the other half of the saving, which is on the SENDING
// side and invisible to any wire assertion: a repeated value is copied once per scan rather than
// once per cell.
//
// It also pins that the intern table is keyed by the OWNED copy. Keying it by the borrowed string
// would retain that string -- and with it the whole decompressed block the copy exists to release
// -- which is the bug this map could most easily have been.
func TestInternCopiesOncePerDistinctValue(t *testing.T) {
	e := newValueEncoder(1)
	borrowed := []string{"user0", "user1", "user0", "user1", "user0"}
	first := map[string]string{}
	for _, s := range borrowed {
		got := e.intern(s)
		if got != s {
			t.Fatalf("intern(%q) = %q", s, got)
		}
		if prev, seen := first[s]; seen {
			if !sameStringData(prev, got) {
				t.Errorf("intern(%q) made a second copy; it should reuse the first", s)
			}
		} else {
			first[s] = got
		}
	}
	if len(e.owned) != 2 {
		t.Errorf("intern table holds %d entries for 2 distinct values", len(e.owned))
	}
	for k, v := range e.owned {
		if !sameStringData(k, v) {
			t.Errorf("the intern table is keyed by something other than the copy it returns; "+
				"a borrowed key would pin the block it was read from (key %q)", k)
		}
	}
}

// sameStringData reports whether two strings share a backing array, which is how "the same copy"
// is checked without comparing contents that are equal by construction.
func sameStringData(a, b string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return a == b
	}
	return unsafe.StringData(a) == unsafe.StringData(b)
}

func TestStringDictionaryRejectsACodeWithNoEntry(t *testing.T) {
	// A frame whose code points past its dictionary is corrupt or truncated. Substituting a
	// blank would put a value in the result that no record ever held, so the batch is refused.
	var b []byte
	b = putI32(b, 1) // rows
	b = putI32(b, 1) // cols
	b = append(b, byte(KindStringDict))
	b = putBytes(b, []byte{0x01}) // valid: row 0 packed
	b = putI32(b, 1)              // one code
	b = putI32(b, 1)              // dictionary of one
	b = putStr(b, "only")
	b = putI32(b, 7) // code 7, with one entry
	b = putI32(b, 0) // no exceptions
	r := &reader{b: b}
	var out ValueBatch
	if decodeValueBatch(r, &out) {
		t.Error("a code with no dictionary entry was accepted")
	}
}
