package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// valuesFixture builds a catalog holding one mutable table and one archive with the SAME
// records, so every test below can ask both kinds the same question and compare. The shapes are
// chosen to exercise each branch of the column encoder:
//
//	ClusterId     int in every record          -> a uniform packed column
//	Owner         string in every record       -> a uniform packed column
//	RequestMemory int, but a string in a few   -> a packed column with scalar exceptions
//	MemoryUsage   an EXPRESSION over siblings  -> must EVALUATE, not come back undefined
//	Tags          a list                       -> no column form at all
//	Missing       absent from most records     -> undefined, which is an answer
const valuesN = 50

func valuesFixture(t *testing.T) (*Client, func()) {
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
			// A few records hold a string where the rest hold an int: the exceptions path.
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
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); cat.Close() }
}

var valuesTables = []string{"jobs", "history"}

// collectValues reads the stream into owned rows of values, keyed by ClusterId so the two table
// kinds (which return rows in different orders) compare.
func collectValues(t *testing.T, c *Client, table string, attrs []string, limit int) map[string][]classad.Value {
	t.Helper()
	out := map[string][]classad.Value{}
	err := c.QueryValuesStream(context.Background(), table, "true", attrs, limit, false,
		func(b *ValueBatch) bool {
			if len(b.Cols) != len(attrs) {
				t.Fatalf("%s: batch has %d columns, want %d", table, len(b.Cols), len(attrs))
			}
			for row := 0; row < b.Rows; row++ {
				vals := make([]classad.Value, len(attrs))
				for col := range attrs {
					vals[col] = b.ValueAt(col, row)
				}
				out[vals[0].String()] = vals
			}
			return true
		})
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return out
}

// sameValue compares a typed cell against the oracle's. They are equal outright for every scalar.
// A value with NO scalar form -- this fixture's Tags list -- crosses as the unparser's TEXT of it
// by design, so the comparison is against that same rendering rather than against the list, which
// is exactly the contract ValueException.NonScalar announces.
func sameValue(got, want classad.Value) (string, bool) {
	if got.String() == want.String() {
		return got.String(), true
	}
	if txt, ok := want.Unparse(); ok {
		if s, _ := got.StringValue(); s == txt {
			return got.String(), true
		}
	}
	return got.String(), false
}

// TestQueryValuesAgreesWithTheTextProjection is the oracle. The typed stream is a different
// encoder over the same scan, so the only thing that makes it trustworthy is agreeing, value for
// value, with the read everything else already uses -- the reference-chasing text projection,
// parsed and evaluated the long way round.
func TestQueryValuesAgreesWithTheTextProjection(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	ctx := context.Background()
	attrs := []string{"ClusterId", "Owner", "RequestMemory", "MemoryUsage", "Tags", "Missing"}

	for _, table := range valuesTables {
		t.Run(table, func(t *testing.T) {
			got := collectValues(t, c, table, attrs, 0)
			if len(got) != valuesN {
				t.Fatalf("typed stream returned %d rows, want %d", len(got), valuesN)
			}

			rows, err := c.QueryRawProjectRefs(ctx, table, "true", attrs, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != valuesN {
				t.Fatalf("text projection returned %d rows, want %d", len(rows), valuesN)
			}
			for _, r := range rows {
				ad, perr := classad.ParseOld(r)
				if perr != nil {
					t.Fatalf("parsing the oracle's row %q: %v", r, perr)
				}
				key := ad.EvaluateAttr("ClusterId").String()
				vals, ok := got[key]
				if !ok {
					t.Fatalf("typed stream omitted ClusterId=%s", key)
				}
				for col, name := range attrs {
					want := ad.EvaluateAttr(name)
					if got, ok := sameValue(vals[col], want); !ok {
						t.Errorf("ClusterId=%s %s: typed %q, text projection %q", key, name, got, want.String())
					}
				}
			}
		})
	}
}

// TestQueryValuesColumnShapes pins what the encoder chose, not only what it returned. Agreeing
// with the oracle would still be true of a column that packed nothing and put every cell in the
// exceptions list -- correct, and none of the point of the op.
func TestQueryValuesColumnShapes(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	attrs := []string{"ClusterId", "Owner", "RequestMemory", "MemoryUsage", "Tags", "Missing"}

	for _, table := range valuesTables {
		t.Run(table, func(t *testing.T) {
			seen := 0
			err := c.QueryValuesStream(context.Background(), table, "true", attrs, 0, false,
				func(b *ValueBatch) bool {
					seen++
					cols := map[string]*ValueColumn{}
					for i, n := range attrs {
						cols[n] = &b.Cols[i]
					}
					// Uniform: every cell packed, nothing left over. This is the shape an Arrow
					// buffer wants, and the reason the bitmap and the dense slice exist.
					for _, n := range []string{"ClusterId", "Owner", "MemoryUsage"} {
						c := cols[n]
						if len(c.Exceptions) != 0 {
							t.Errorf("%s: %d exceptions over %d rows, want a uniform column: %v",
								n, len(c.Exceptions), b.Rows, c.Exceptions[0].Value.String())
						}
						if dense := denseLen(c); dense != b.Rows {
							t.Errorf("%s: packed %d of %d rows", n, dense, b.Rows)
						}
					}
					if k := cols["ClusterId"].Kind; k != KindInt {
						t.Errorf("ClusterId column kind %v, want int", k)
					}
					if k := cols["Owner"].Kind; k != KindString {
						t.Errorf("Owner column kind %v, want string", k)
					}
					// MemoryUsage is stored as an expression in EVERY record. A column of them
					// proves the server evaluated: a relay would have had nothing to pack.
					if k := cols["MemoryUsage"].Kind; k != KindInt {
						t.Errorf("MemoryUsage column kind %v, want int -- the expression was not "+
							"evaluated server-side", k)
					}
					// Mixed: the majority kind packs and the odd cells become exceptions.
					rm := cols["RequestMemory"]
					if rm.Kind != KindInt {
						t.Errorf("RequestMemory column kind %v, want int (the majority)", rm.Kind)
					}
					if len(rm.Exceptions) == 0 {
						t.Error("RequestMemory has no exceptions; the fixture no longer stores a " +
							"string where the rest store an int, so this case is untested")
					}
					for _, x := range rm.Exceptions {
						if !x.Value.IsString() {
							t.Errorf("RequestMemory exception at row %d is %v, want the string cell",
								x.Row, x.Value.String())
						}
					}
					if denseLen(rm)+len(rm.Exceptions) != b.Rows {
						t.Errorf("RequestMemory: %d packed + %d exceptions != %d rows",
							denseLen(rm), len(rm.Exceptions), b.Rows)
					}
					// A list has no column form, so the column declines entirely rather than
					// reporting nulls -- and the value survives as the text that is its only
					// useful form.
					tags := cols["Tags"]
					if tags.Kind != KindNone {
						t.Errorf("Tags column kind %v, want none", tags.Kind)
					}
					if len(tags.Exceptions) != b.Rows {
						t.Errorf("Tags: %d exceptions over %d rows, want every cell",
							len(tags.Exceptions), b.Rows)
					}
					for _, x := range tags.Exceptions {
						if !x.NonScalar {
							t.Errorf("Tags exception at row %d is not flagged non-scalar, so a "+
								"consumer cannot tell the list from a string", x.Row)
						}
						if s, _ := x.Value.StringValue(); !strings.Contains(s, "\"a\"") {
							t.Errorf("Tags exception at row %d lost the list text: %q", x.Row, s)
						}
						break
					}
					// Absent is UNDEFINED, which is an answer and not a failure, and it stays
					// distinguishable from a packed value.
					miss := cols["Missing"]
					undef := 0
					for _, x := range miss.Exceptions {
						if x.Value.IsUndefined() {
							undef++
						}
					}
					if undef == 0 {
						t.Error("no cell of Missing came back undefined; the fixture no longer " +
							"leaves it out of most records")
					}
					return true
				})
			if err != nil {
				t.Fatal(err)
			}
			if seen == 0 {
				t.Fatal("no batch was delivered")
			}
		})
	}
}

func denseLen(c *ValueColumn) int {
	switch c.Kind {
	case KindInt:
		return len(c.Ints)
	case KindReal:
		return len(c.Reals)
	case KindBool:
		return len(c.Bools)
	case KindString:
		return len(c.Strs)
	}
	return 0
}

// TestQueryValuesBatches checks the stream actually batches, and that the batch boundary does not
// change the answer. A single-batch stream would let every column-kind decision above be made
// over the whole result, which is not what a real scan does.
func TestQueryValuesBatches(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	old := ValueBatchRows
	ValueBatchRows = 7
	defer func() { ValueBatchRows = old }()

	attrs := []string{"ClusterId", "Owner"}
	batches, rows := 0, 0
	keys := map[string]bool{}
	err := c.QueryValuesStream(context.Background(), "history", "true", attrs, 0, false,
		func(b *ValueBatch) bool {
			batches++
			if b.Rows > ValueBatchRows {
				t.Fatalf("batch carried %d rows, over the %d cap", b.Rows, ValueBatchRows)
			}
			for row := 0; row < b.Rows; row++ {
				keys[b.ValueAt(0, row).String()] = true
				rows++
			}
			return true
		})
	if err != nil {
		t.Fatal(err)
	}
	if batches < 2 {
		t.Fatalf("%d batch(es) for %d rows at a cap of %d; the stream is not batching",
			batches, rows, ValueBatchRows)
	}
	if rows != valuesN || len(keys) != valuesN {
		t.Fatalf("batched stream delivered %d rows (%d distinct), want %d", rows, len(keys), valuesN)
	}
}

// TestQueryValuesLimit checks the limit is applied server-side and the stream ends there.
func TestQueryValuesLimit(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	for _, table := range valuesTables {
		t.Run(table, func(t *testing.T) {
			got := collectValues(t, c, table, []string{"ClusterId"}, 5)
			if len(got) != 5 {
				t.Fatalf("limit 5 returned %d rows", len(got))
			}
		})
	}
}

// TestQueryValuesStopsOnYieldFalse checks a consumer can abandon the stream mid-batch without
// the connection being left unusable -- the caller's next read must still work.
func TestQueryValuesStopsOnYieldFalse(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	n := 0
	err := c.QueryValuesStream(context.Background(), "jobs", "true", []string{"ClusterId"}, 0, false,
		func(b *ValueBatch) bool { n++; return false })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("yield returned false after batch 1, but %d batches were delivered", n)
	}
	got := collectValues(t, c, "jobs", []string{"ClusterId"}, 0)
	if len(got) != valuesN {
		t.Fatalf("the connection returned %d rows after an abandoned stream, want %d", len(got), valuesN)
	}
}

// TestQueryValuesRefusesUnknownFlags pins the contract that makes the reserved flags word safe:
// a server that does not understand a bit REFUSES, rather than serving the request without
// whatever the bit asked for. A stream that silently lacks an option is the dangerous shape.
func TestQueryValuesRefusesUnknownFlags(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	build := func(id uint64) []byte {
		b := putStr(req(id, opQueryValues), "jobs")
		b = putI32(b, 0)
		unknown := ^valueFlagsKnown   // every bit this server does not know
		b = putI32(b, int32(unknown)) //nolint:gosec // a deliberate out-of-contract request
		b = putU8(b, 0)
		b = putStr(b, "true")
		b = putI32(b, 1)
		return putStr(b, "ClusterId")
	}
	_, ch, _, err := c.callStream(build)
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := <-ch
	if !ok {
		t.Fatal("the server closed the stream without answering")
	}
	_, status, _, ok := respHeader(frame)
	if !ok {
		t.Fatal("short response frame")
	}
	if status != stBadReq {
		t.Fatalf("an unknown flag bit was answered with status %v, want stBadReq -- the server "+
			"served a request it did not understand", status)
	}
	drain(ch)
}

// TestQueryValuesNeedsAProjection: this op packs one column per REQUESTED attribute, so "every
// attribute" has no shape here -- a record's attribute set varies row to row. Refused rather
// than answered with an empty or guessed column set.
func TestQueryValuesNeedsAProjection(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	err := c.QueryValuesStream(context.Background(), "jobs", "true", nil, 0, false,
		func(b *ValueBatch) bool { return true })
	if err == nil {
		t.Fatal("an empty projection was accepted")
	}
}

// TestQueryValuesMissingTable reports the name, not an empty stream: an empty result and an
// unknown table are different answers and a caller must be able to tell them apart.
func TestQueryValuesMissingTable(t *testing.T) {
	c, cleanup := valuesFixture(t)
	defer cleanup()
	err := c.QueryValuesStream(context.Background(), "nope", "true", []string{"ClusterId"}, 0, false,
		func(b *ValueBatch) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("reading an unknown table returned %v", err)
	}
}
