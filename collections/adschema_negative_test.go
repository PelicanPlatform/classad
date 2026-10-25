package collections

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// negAdSchemaOpts is the schema shape these tests build: a presence floor well under full
// coverage and a fit percentile that one outlier cannot move.
var negAdSchemaOpts = adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: true}

// TestNegativeFromTextIsColumnar is the regression this whole change exists for.
//
// Job and history ads arrive as old-ClassAd TEXT, and ExitCode is the obvious attribute that
// goes negative. Before the encoder folded `-1` into a signed literal, an attribute built by
// parsing text and the same attribute built by inserting the same int64 produced DIFFERENT
// columns -- and when every value was negative, the text-built one produced no column at all,
// because an attribute with no storable scalar never clears the presence floor. The failure was
// silent: queries returned the right rows, at full scan cost, with no column to show for it.
//
// Both construction paths are exercised against each other on purpose. "They agree" is the
// property that matters; either one alone can be wrong in the same direction.
func TestNegativeFromTextIsColumnar(t *testing.T) {
	cases := []struct {
		name string
		val  func(i int) int64
		// unsigned is what the column should be: a sample with any negative in it must be
		// signed, or every negative value escapes the column it was supposed to live in.
		unsigned bool
	}{
		{"all non-negative", func(i int) int64 { return int64(i % 5) }, true},
		{"all negative", func(i int) int64 { return -int64(1 + i%5) }, false},
		{"one in ten negative", func(i int) int64 {
			if i%10 == 0 {
				return -1
			}
			return int64(i % 5)
		}, false},
	}
	for _, tc := range cases {
		var fromText, fromInsert adField
		for _, viaText := range []bool{true, false} {
			c := New(Options{Shards: 1})
			var wires [][]byte
			for i := 0; i < 40; i++ {
				ad := classad.New()
				if viaText {
					var err error
					if ad, err = classad.ParseOld(fmt.Sprintf("ExitCode = %d\nOwner = \"user%02d\"", tc.val(i), i)); err != nil {
						t.Fatal(err)
					}
				} else {
					ad.InsertAttr("ExitCode", tc.val(i))
					ad.InsertAttrString("Owner", fmt.Sprintf("user%02d", i))
				}
				wires = append(wires, c.encodeAd(ad.AST()))
			}
			s := buildAdSchema(wires, negAdSchemaOpts)
			id, _ := c.intern.LookupID("ExitCode")
			idx, ok := s.byID[id]
			how := "InsertAttr"
			if viaText {
				how = "ParseOld"
			}
			if !ok {
				t.Errorf("%s/%s: ExitCode got no columnar field", tc.name, how)
				continue
			}
			f := s.fields[idx]
			if f.kind != akInt || f.unsigned != tc.unsigned {
				t.Errorf("%s/%s: ExitCode column kind=%d unsigned=%v, want akInt unsigned=%v",
					tc.name, how, f.kind, f.unsigned, tc.unsigned)
			}
			if viaText {
				fromText = f
			} else {
				fromInsert = f
			}
			// The values have to survive the column, not merely earn one.
			for i, w := range wires {
				assertRoundTrip(t, s, w, fmt.Sprintf("%s/%s ad[%d]", tc.name, how, i))
			}
		}
		if fromText.kind != fromInsert.kind || fromText.width != fromInsert.width || fromText.unsigned != fromInsert.unsigned {
			t.Errorf("%s: parsed text and inserted int64 disagree about the column: %+v vs %+v",
				tc.name, fromText, fromInsert)
		}
	}
}

// TestNegativeStaysOutOfUnsignedColumn guards the other direction. Signedness is chosen from the
// SAMPLE, so a value more negative than the sample suggested still has to escape rather than be
// written into a slot that cannot represent it -- which, read back through an unsigned column,
// would come back as a large positive number instead of an error.
func TestNegativeStaysOutOfUnsignedColumn(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 40; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("ExitCode = %d\nOwner = \"user%02d\"", i%5, i))
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	s := buildAdSchema(wires, negAdSchemaOpts)
	assertIntField(t, s, c, "ExitCode", true)

	odd, err := classad.ParseOld(`ExitCode = -7` + "\n" + `Owner = "late"`)
	if err != nil {
		t.Fatal(err)
	}
	w := c.encodeAd(odd.AST())
	assertRoundTrip(t, s, w, "negative into an unsigned column")
}

// TestOldFormNegativeRoundTrips pins the read side of the fold at the schema layer.
//
// Segments sealed before the fold hold `-1` as a unary-minus node over the literal 1, and no
// record is ever rewritten. Such a record meets a column that is now a SIGNED int field and must
// still take the escape path and come back byte-identical, exactly as it did when there was no
// signed column to tempt it.
//
// The old-form ad is assembled here by hand rather than encoded, because the encoder no longer
// produces these bytes: building the fixture with the encoder would quietly turn this into a
// second test of the new form.
func TestOldFormNegativeRoundTrips(t *testing.T) {
	c := New(Options{Shards: 1})

	// A corpus that earns ExitCode a signed int column.
	var wires [][]byte
	for i := 0; i < 40; i++ {
		ad, err := classad.ParseOld(fmt.Sprintf("ExitCode = %d\nOwner = \"user%02d\"", -1+i%5, i))
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, c.encodeAd(ad.AST()))
	}
	s := buildAdSchema(wires, negAdSchemaOpts)
	assertIntField(t, s, c, "ExitCode", false)

	// nUnOp(0x09), unOps[0] == "-", nInt(0x04), zigzag varint 2 == 1: the pre-fold encoding
	// of -1, written out literally.
	oldNode := []byte{0x09, 0x00, 0x04, 0x02}
	w := oldFormInternedAd(c, map[string][]byte{
		"ExitCode": oldNode,
		"Owner":    wire.AppendStringNode(nil, "legacy"),
	})

	// The value still reads as -1 ...
	node, ok := wire.Ad(w).Lookup(mustIntern(t, c, "ExitCode"))
	if !ok {
		t.Fatal("hand-built ad: ExitCode not found")
	}
	if lit, ok := wire.NumberValue(node); !ok || lit.Int != -1 {
		t.Fatalf("hand-built old-form ExitCode reads as (%+v, %v), want -1", lit, ok)
	}
	// ... and survives the schema record unchanged, through the cold tail.
	assertRoundTrip(t, s, w, "old-form negative")

	rec := s.encode(wire.Ad(w))
	var got []byte
	s.forEachKeyed(rec, func(id uint32, n []byte, fromCold bool) bool {
		if id == mustIntern(t, c, "ExitCode") {
			got = append([]byte(nil), n...)
			if !fromCold {
				t.Error("an old-form negative must escape to the cold tail, not occupy the int slot")
			}
		}
		return true
	})
	if !bytes.Equal(got, oldNode) {
		t.Errorf("old-form node came back as % x, want % x -- the decode side must not 'correct' it", got, oldNode)
	}
}

func mustIntern(t *testing.T, c *Collection, name string) uint32 {
	t.Helper()
	id, ok := c.intern.LookupID(name)
	if !ok {
		t.Fatalf("%s was never interned", name)
	}
	return id
}

// oldFormInternedAd assembles an interned wire ad from raw attribute nodes, so a test can put
// bytes on the wire that the current encoder would not write. Layout per wire/format.go:
// [magic][version][flags=0][hotCount][attrCount] then (uvarint internID + node)*.
func oldFormInternedAd(c *Collection, attrs map[string][]byte) []byte {
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	b := []byte{0xCA, 0x01, 0x00}
	b = binary.AppendUvarint(b, 0) // hotCount
	b = binary.AppendUvarint(b, uint64(len(names)))
	for _, n := range names {
		b = binary.AppendUvarint(b, uint64(c.intern.Intern(n)))
		b = append(b, attrs[n]...)
	}
	return b
}

// TestNegativeRealFromTextIsColumnar is the real-valued half of the same bug.
//
// The mechanism differs in one way worth keeping straight: a real column has no width or
// signedness to choose, so a negative real never mis-sized a slot. What it did instead was fail
// to be a stored scalar at all, which cost it the presence floor -- an attribute whose values
// are mostly negative reals was dominated by no storable kind and got no column, and the ones
// that did have a column escaped every negative record into the cold tail.
func TestNegativeRealFromTextIsColumnar(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  func(i int) float64
	}{
		{"all negative", func(i int) float64 { return -1.5 - float64(i%7) }},
		{"mixed sign", func(i int) float64 { return -3.5 + float64(i%7) }},
	} {
		c := New(Options{Shards: 1})
		var wires [][]byte
		for i := 0; i < 40; i++ {
			ad, err := classad.ParseOld(fmt.Sprintf("Rate = %s\nOwner = \"user%02d\"",
				strconv.FormatFloat(tc.val(i), 'g', -1, 64), i))
			if err != nil {
				t.Fatal(err)
			}
			wires = append(wires, c.encodeAd(ad.AST()))
		}
		s := buildAdSchema(wires, negAdSchemaOpts)
		id, _ := c.intern.LookupID("Rate")
		idx, ok := s.byID[id]
		if !ok {
			t.Errorf("%s: Rate got no columnar field", tc.name)
			continue
		}
		if f := s.fields[idx]; f.kind != akReal {
			t.Errorf("%s: Rate column kind=%d, want akReal", tc.name, f.kind)
		}
		for i, w := range wires {
			assertRoundTrip(t, s, w, fmt.Sprintf("%s real ad[%d]", tc.name, i))
		}
	}
}
