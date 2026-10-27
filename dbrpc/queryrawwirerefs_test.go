package dbrpc

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// seedWireExprAds stores ads whose Req attribute is an expression over a sibling for some
// keys and a literal for others -- the mix that makes the closure a property of the record
// rather than of the query.
func seedWireExprAds(t *testing.T, c *Client, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad := "MyType = \"Machine\"\nName = \"slot" + itoaTest(int64(i)) + "\"\n" +
			"Memory = " + itoaTest(int64(512+512*(i%4))) + "\nDisk = " + itoaTest(int64(1000*(1+i%3))) + "\n"
		if i%3 == 0 {
			ad += "Req = true\n"
		} else {
			ad += "Req = Memory > 1024 && Disk > 1500\n"
		}
		if err := tx.NewClassAd(ctx, "slot"+itoaTest(int64(i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// wireRefRows collects a QueryRawWireRefsStream result rendered at the client edge, keyed by
// Name, as name -> value maps.
func wireRefRows(t *testing.T, c *Client, attrs []string, redact bool) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	var buf []byte
	var offs []int
	err := c.QueryRawWireRefsStream(context.Background(), DefaultTable, "true", attrs, 0, redact, func(row []byte) bool {
		var ok bool
		buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("render failed on a wire row")
		}
		m := map[string]string{}
		for i := 0; i+1 < len(offs); i++ {
			name, val, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
			if !found {
				t.Fatalf("malformed expr %q", buf[offs[i]:offs[i+1]])
			}
			m[name] = val
		}
		out[strings.Trim(m["Name"], `"`)] = m
		return true
	})
	if err != nil {
		t.Fatalf("QueryRawWireRefsStream: %v", err)
	}
	return out
}

func attrNames(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestQueryRawWireRefsEvaluates: the projected expression arrives with the siblings it
// reads, so it evaluates to what the whole ad would give -- which is the entire reason the
// opcode exists. The plain wire stream is run alongside to pin that it does NOT, so the
// difference reads as the deliberate one it is.
func TestQueryRawWireRefsEvaluates(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	seedWireExprAds(t, c, 12)

	chased := wireRefRows(t, c, []string{"Name", "Req"}, false)
	if len(chased) != 12 {
		t.Fatalf("got %d rows, want 12", len(chased))
	}
	sawTrue, sawFalse := false, false
	for name, m := range chased {
		ad, err := classad.ParseOld(exprText(m))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v := ad.EvaluateAttr("Req")
		b, err := v.BoolValue()
		if err != nil {
			t.Fatalf("%s: Req evaluated to %v, want a bool -- the referenced siblings did not come along (%s)",
				name, v, attrNames(m))
		}
		sawTrue, sawFalse = sawTrue || b, sawFalse || !b
	}
	if !sawTrue || !sawFalse {
		t.Fatalf("every row evaluated the same way (true=%v false=%v); the corpus proves nothing", sawTrue, sawFalse)
	}

	// The plain stream, same projection: an expression row loses its siblings.
	var buf []byte
	var offs []int
	undef := 0
	err := c.QueryRawWireStream(context.Background(), DefaultTable, "true", []string{"Name", "Req"}, 0, false, func(row []byte) bool {
		var ok bool
		buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("render failed")
		}
		var b strings.Builder
		for i := 0; i+1 < len(offs); i++ {
			b.Write(buf[offs[i]:offs[i+1]])
			b.WriteByte('\n')
		}
		ad, err := classad.ParseOld(b.String())
		if err != nil {
			t.Fatal(err)
		}
		if ad.EvaluateAttr("Req").IsUndefined() {
			undef++
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if undef == 0 {
		t.Fatal("the plain wire stream lost no expression; the comparison proves nothing")
	}
}

func exprText(m map[string]string) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestQueryRawWireRefsMatchesTextRefs is the cross-protocol contract: for the same
// projection, the wire-row stream with chasing returns exactly what the text stream with
// chasing returns -- same attributes, same values, every row.
//
// Rows are compared as a sorted MULTISET of canonical "name = value" sets, not keyed by an
// attribute: a projection need not include anything that identifies a row, and keying on one
// that is absent silently collapses every row into one and compares nothing.
func TestQueryRawWireRefsMatchesTextRefs(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	seedWireExprAds(t, c, 30)
	ctx := context.Background()

	canon := func(m map[string]string) string {
		// MyType rides as a type tag on a wire row and as an expression in a text row, so it
		// is compared out of both rather than against itself.
		delete(m, "MyType")
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k)
			b.WriteString(" = ")
			b.WriteString(m[k])
			b.WriteString("; ")
		}
		return b.String()
	}

	for _, proj := range [][]string{{"Name", "Req"}, {"Name"}, {"Req", "Disk"}, {"Req"}} {
		rows, err := c.QueryRawProjectRefs(ctx, DefaultTable, "true", proj, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := make([]string, 0, len(rows))
		for _, row := range rows {
			m := map[string]string{}
			for _, line := range strings.Split(strings.TrimRight(row, "\n"), "\n") {
				name, val, found := strings.Cut(line, " = ")
				if !found {
					t.Fatalf("malformed text row line %q", line)
				}
				m[name] = val
			}
			want = append(want, canon(m))
		}
		var got []string
		var buf []byte
		var offs []int
		err = c.QueryRawWireRefsStream(ctx, DefaultTable, "true", proj, 0, false, func(row []byte) bool {
			var ok bool
			buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
			if !ok {
				t.Fatal("render failed on a wire row")
			}
			m := map[string]string{}
			for i := 0; i+1 < len(offs); i++ {
				name, val, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
				if !found {
					t.Fatalf("malformed expr %q", buf[offs[i]:offs[i+1]])
				}
				m[name] = val
			}
			got = append(got, canon(m))
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(want)
		sort.Strings(got)
		if len(got) != len(want) {
			t.Fatalf("%v: wire-refs %d rows, text-refs %d", proj, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%v: row %d\n wire-refs = %s\n text-refs = %s", proj, i, got[i], want[i])
			}
		}
	}
}

// TestQueryRawWireRefsRedacts: an unprivileged connection never ships a private attribute,
// and being referenced by a projected expression is not an exception.
func TestQueryRawWireRefsRedacts(t *testing.T) {
	c, cleanup := testPairPersistent(t, false) // unprivileged
	defer cleanup()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ad := "MyType = \"Machine\"\nName = \"slot" + itoaTest(int64(i)) + "\"\nCpus = 8\n" +
			"ClaimId = \"secret\"\nStart = Cpus > 0 && ClaimId != \"\"\n"
		if err := tx.NewClassAd(ctx, "slot"+itoaTest(int64(i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for name, m := range wireRefRows(t, c, []string{"Name", "Start"}, false) {
		if _, leak := m["ClaimId"]; leak {
			t.Fatalf("%s: an unprivileged reference-chasing row carries ClaimId: %s", name, attrNames(m))
		}
		if _, ok := m["Cpus"]; !ok {
			t.Fatalf("%s: the non-private reference was dropped with the private one: %s", name, attrNames(m))
		}
	}
}

// TestQueryRawWireRefsLimit: the limit is applied to rows, as on the plain stream.
func TestQueryRawWireRefsLimit(t *testing.T) {
	c, cleanup := testPairPersistent(t, true)
	defer cleanup()
	seedWireExprAds(t, c, 10)
	n := 0
	err := c.QueryRawWireRefsStream(context.Background(), DefaultTable, "true", []string{"Name", "Req"}, 4, false,
		func([]byte) bool { n++; return true })
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("streamed %d rows, want 4 (LIMIT was not applied)", n)
	}
}

// TestQueryRawWireRefsServesBothStorageKinds: the chased wire stream is a DB operation, not
// a property of where the rows happen to live, so an in-memory table and a persistent one
// both serve it -- and both close the projection under the projected expressions'
// references. An in-memory record is keyed by interned ids rather than inline names, which
// is a storage detail the relay's consumer never sees.
//
// Asserting the ATTRIBUTES rather than a row count is the point. In-memory tables used to
// refuse this stream outright; a version that serves rows WITHOUT the closure would satisfy
// any count-based check while handing the consumer a Requirements it cannot evaluate.
func TestQueryRawWireRefsServesBothStorageKinds(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()

	const ad = "Name = \"one\"\nCpus = 4\nReq = Cpus > 0"
	for _, tbl := range []struct {
		name   string
		create func(context.Context, string) error
	}{
		{"scratch", c.CreateTableInMemory},
		{"durable", c.CreateTable},
	} {
		if err := tbl.create(ctx, tbl.name); err != nil {
			t.Fatal(err)
		}
		tx, err := c.BeginTable(ctx, tbl.name)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.NewClassAd(ctx, "k1", ad); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		var names []string
		rows := 0
		var buf []byte
		var offs []int
		err = c.QueryRawWireRefsStream(ctx, tbl.name, "true", []string{"Req"}, 0, false, func(row []byte) bool {
			rows++
			var ok bool
			buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
			if !ok {
				t.Fatal("render failed on a wire row")
			}
			names = names[:0]
			for i := 0; i+1 < len(offs); i++ {
				name, _, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
				if !found {
					t.Fatalf("malformed expr %q", buf[offs[i]:offs[i+1]])
				}
				names = append(names, name)
			}
			return true
		})
		if err != nil {
			t.Fatalf("%s: %v", tbl.name, err)
		}
		if rows != 1 {
			t.Fatalf("%s: returned %d wire rows, want 1", tbl.name, rows)
		}
		sort.Strings(names)
		// Req references Cpus, which the projection does not name; the closure must add it.
		if got := strings.Join(names, ","); got != "Cpus,Req" {
			t.Errorf("%s: row carries %s, want Cpus,Req -- a row without Cpus means the closure did not run on this storage kind", tbl.name, got)
		}
	}
}

// TestQueryRawWireUnknownFlagIsRefused pins the contract that lets wire-row options be flags
// instead of opcodes: a bit this server does not implement is refused with stBadReq BEFORE
// any row, exactly as an unknown opcode is, so a newer client falls back down the path it
// already has. If the server ever quietly ignored an unknown bit, a client asking for the
// reference closure would receive a perfectly well-formed row stream that silently lacked
// it -- the failure the refusal exists to prevent, and one no row-level assertion would
// catch.
func TestQueryRawWireUnknownFlagIsRefused(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()
	if err := c.CreateTable(ctx, "durable"); err != nil {
		t.Fatal(err)
	}
	tx, err := c.BeginTable(ctx, "durable")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, "k1", "Name = \"two\"\nCpus = 8\nReq = Cpus > 0"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	send := func(flags uint32) error {
		build := func(id uint64) []byte {
			b := putStr(req(id, opQueryRawWireOpts), "durable")
			b = putI32(b, 0)            // limit
			b = putI32(b, int32(flags)) // flags
			b = append(b, 0)            // redact
			b = putStr(b, "true")       // constraint
			b = putI32(b, 1)            // nattrs
			return putStr(b, "Req")
		}
		_, ch, strm, err := c.callStream(build)
		if err != nil {
			return err
		}
		for {
			frame, ok := <-ch
			if !ok {
				return c.streamEnd(strm)
			}
			_, status, body, ok := respHeader(frame)
			if !ok {
				return errShort
			}
			if status == stBadReq {
				return ErrRawWireUnsupported
			}
			if status != stStream {
				return statusErr(status, body)
			}
		}
	}

	// The bit this server implements is served.
	if err := send(wireFlagRefs); err != nil {
		t.Fatalf("wireFlagRefs: %v", err)
	}
	// An unimplemented bit, alone or alongside a known one, is refused.
	for _, flags := range []uint32{1 << 31, wireFlagRefs | 1<<1} {
		if err := send(flags); !errors.Is(err, ErrRawWireUnsupported) {
			t.Errorf("flags %#x: err = %v, want the stBadReq refusal", flags, err)
		}
	}
}
