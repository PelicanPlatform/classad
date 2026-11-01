package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// Every row-returning raw read op, served to an UNPRIVILEGED session over an encrypted table, must
// carry neither the plaintext of a sealed value nor any private attribute -- whatever the request
// asks for. The paged projection (opQueryRawProjSeq) was the one that did not: it ignored the
// session's privilege and served the entitled read, private attributes and opened sealed values
// included, to any peer.
//
// The rows are searched for the plaintext as bytes; a decoded-value check would miss a secret
// carried under another name.

const (
	rawOpsSalary = "payroll-77c1e-sealed-not-private"
	rawOpsClaim  = "claim-41d0b-private-by-name"
	rawOpsN      = 40
)

func rawOpsPair(t *testing.T, includePrivate bool) (*Client, func()) {
	t.Helper()
	d, err := db.OpenConfig(db.Config{
		Dir:            t.TempDir(),
		PoolKeys:       []db.KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"Salary"},
	})
	if err != nil {
		t.Skipf("encrypted table unavailable here: %v", err)
	}
	tx := d.Begin()
	for i := 0; i < rawOpsN; i++ {
		// NewClassAd, not NewClassAdOld: the streaming old-text encoder does not seal, and on an
		// encrypted table it stores nothing.
		ad, err := classad.ParseOld(fmt.Sprintf("MyType = \"Job\"\nClusterId = %d\nCpus = 4\n"+
			"Salary = \"%s-%d\"\nClaimId = \"%s-%d\"", i, rawOpsSalary, i, rawOpsClaim, i))
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	opts := ServeOptions{ReadOnly: true}
	if includePrivate {
		opts = ServeOptions{IncludePrivate: true}
	}
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

type rawOpRead struct {
	name string
	run  func(t *testing.T, c *Client) []string
	// wire rows are inline wire, rendered to old-ClassAd text before the check.
	wire bool
	// values rows come from the typed value stream, rendered one entry per line. Like a wire
	// row, a cell with no value is absent rather than present-and-undefined.
	values bool
}

// omitsAbsent reports whether this op leaves an attribute OUT rather than serving it as
// undefined -- true of the wire rows and of the typed value columns, and the reason the
// sealed-attribute check below accepts either shape.
func (r rawOpRead) omitsAbsent() bool { return r.wire || r.values }

func rawOpReads() []rawOpRead {
	ctx := context.Background()
	const where = "Cpus == 4"
	attrs := []string{"ClusterId", "Salary", "ClaimId"}
	must := func(t *testing.T, rows []string, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	collect := func(t *testing.T, f func(yield func(string) bool) error) []string {
		t.Helper()
		var rows []string
		if err := f(func(r string) bool { rows = append(rows, r); return true }); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	wireRows := func(t *testing.T, f func(yield func([]byte) bool) error) []string {
		t.Helper()
		var rows []string
		if err := f(func(r []byte) bool { rows = append(rows, string(r)); return true }); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	reads := []rawOpRead{
		{name: "QueryRawTable", run: func(t *testing.T, c *Client) []string {
			rows, err := c.QueryRawTable(ctx, DefaultTable, where, 0)
			return must(t, rows, err)
		}},
		{name: "QueryRawProjectedFromSeq", run: func(t *testing.T, c *Client) []string {
			// Small pages, so the cursor is handed back and resumed rather than one page read.
			var out []string
			var after SeqCursor
			for {
				rows, page, err := c.QueryRawProjectedFromSeq(ctx, DefaultTable, where, attrs, after, 7)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, rows...)
				if !page.More {
					return out
				}
				after = page.Next
			}
		}},
		{name: "QueryRawProjectedFromSeq/whole", run: func(t *testing.T, c *Client) []string {
			rows, _, err := c.QueryRawProjectedFromSeq(ctx, DefaultTable, where, nil, SeqCursor{}, 0)
			return must(t, rows, err)
		}},
	}
	for _, p := range []struct {
		name  string
		attrs []string
	}{{"whole", nil}, {"projected", attrs}} {
		pa := p.attrs
		reads = append(reads,
			rawOpRead{name: "QueryRawProject/" + p.name, run: func(t *testing.T, c *Client) []string {
				rows, err := c.QueryRawProject(ctx, DefaultTable, where, pa, 0)
				return must(t, rows, err)
			}},
			rawOpRead{name: "QueryRawProjectRefs/" + p.name, run: func(t *testing.T, c *Client) []string {
				rows, err := c.QueryRawProjectRefs(ctx, DefaultTable, where, pa, 0)
				return must(t, rows, err)
			}},
			rawOpRead{name: "QueryRawProjectRefsStats/" + p.name, run: func(t *testing.T, c *Client) []string {
				var st collections.ScanStats
				return collect(t, func(y func(string) bool) error {
					return c.QueryRawProjectRefsStreamStats(ctx, DefaultTable, where, pa, 0, y, &st)
				})
			}},
			// redact=false: the request asks for everything, and the session's privilege must win.
			rawOpRead{name: "QueryRawWire/" + p.name, wire: true, run: func(t *testing.T, c *Client) []string {
				return wireRows(t, func(y func([]byte) bool) error {
					return c.QueryRawWireStream(ctx, DefaultTable, where, pa, 0, false, y)
				})
			}},
			rawOpRead{name: "QueryRawWireRefs/" + p.name, wire: true, run: func(t *testing.T, c *Client) []string {
				return wireRows(t, func(y func([]byte) bool) error {
					return c.QueryRawWireRefsStream(ctx, DefaultTable, where, pa, 0, false, y)
				})
			}})
	}
	// The typed value stream needs an explicit projection: it packs one column per REQUESTED
	// attribute, so there is no "whole ad" shape to join the loop above with.
	//
	// redact=false here too: the request asks for everything, and the session's privilege wins.
	reads = append(reads,
		rawOpRead{name: "QueryValues/projected", values: true, run: func(t *testing.T, c *Client) []string {
			var rows []string
			err := c.QueryValuesStream(ctx, DefaultTable, where, attrs, 0, false, func(b *ValueBatch) bool {
				for row := 0; row < b.Rows; row++ {
					var line []string
					for col, name := range attrs {
						v := b.ValueAt(col, row)
						if v.IsUndefined() {
							continue
						}
						txt, ok := v.Unparse()
						if !ok {
							txt = v.String()
						}
						line = append(line, name+" = "+txt)
					}
					rows = append(rows, strings.Join(line, "\n"))
				}
				return true
			})
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}})
	return reads
}

func rawOpExprs(t *testing.T, row string, wire bool) []string {
	t.Helper()
	if !wire {
		return strings.Split(row, "\n")
	}
	buf, offs, _, _, ok := collections.RenderRawAdInline([]byte(row), nil, nil)
	if !ok {
		t.Fatal("RenderRawAdInline failed on a served wire row")
	}
	var out []string
	for i := 0; i+1 < len(offs); i++ {
		out = append(out, string(buf[offs[i]:offs[i+1]]))
	}
	return out
}

func TestUnprivilegedRawOpsNeverServePlaintext(t *testing.T) {
	pc, pclean := rawOpsPair(t, true)
	defer pclean()
	c, clean := rawOpsPair(t, false)
	defer clean()

	for _, rd := range rawOpReads() {
		t.Run(rd.name, func(t *testing.T) {
			// Premise: the privileged session reads the sealed value through this op.
			prows := rd.run(t, pc)
			if len(prows) != rawOpsN {
				t.Fatalf("privileged session read %d rows, want %d", len(prows), rawOpsN)
			}
			saw := 0
			for _, r := range prows {
				if strings.Contains(r, rawOpsSalary) {
					saw++
				}
			}
			if saw != rawOpsN {
				t.Fatalf("privileged session saw the sealed value in %d of %d rows; the premise fails", saw, rawOpsN)
			}

			rows := rd.run(t, c)
			if len(rows) != rawOpsN {
				t.Errorf("unprivileged session read %d rows, want %d: redaction withholds values, not rows",
					len(rows), rawOpsN)
			}
			for _, r := range rows {
				if strings.Contains(r, rawOpsSalary) || strings.Contains(r, rawOpsClaim) {
					t.Fatalf("an unprivileged session was served plaintext: %q", r)
				}
				salary := 0
				for _, e := range rawOpExprs(t, r, rd.wire) {
					switch {
					case strings.HasPrefix(e, "ClaimId"):
						t.Fatalf("an unprivileged session was served a private attribute: %q", e)
					case strings.HasPrefix(e, "Salary"):
						salary++
						if e != "Salary = undefined" {
							t.Fatalf("sealed attribute served as %q, want undefined", e)
						}
					}
				}
				// Text rows render the sealed attribute as undefined; wire rows and value columns
				// leave it out, which reads the same at the far end.
				if salary != 1 && (salary != 0 || !rd.omitsAbsent()) {
					t.Fatalf("row carries Salary %d times, want once as undefined: %q", salary, r)
				}
			}
		})
	}
}
