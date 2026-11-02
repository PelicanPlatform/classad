package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// An archive in a catalog with pool keys seals Salary (explicitly) and ClaimId (private), and every
// archive read an unprivileged session makes runs with no key, exactly as the same session's reads of a
// mutable table do: the sealed values arrive undefined, decide nothing, and never reach the wire.

const archSessionSecret = "archive-payroll-7d21c-not-a-private-name"

func archiveSealedPair(t *testing.T, privileged bool) (*Client, func()) {
	t.Helper()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir(),
		PoolKeys:       []db.KEK{{ID: "POOL", Material: []byte("archive-session-pool-key-material!!")}},
		EncryptedAttrs: []string{"Salary"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := cat.CreateArchiveTable("history", db.ArchiveConfig{SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nRequestMemory = %d\nSalary = \"%s-%d\"\nClaimId = \"claim-%s-%d\"",
			i, i%3, 100+i, archSessionSecret, i, archSessionSecret, i)
		if err := a.AppendOld(ad); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	opts := ServeOptions{ReadOnly: true}
	if privileged {
		opts = ServeOptions{IncludePrivate: true}
	}
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); cat.Close() }
}

func TestArchiveUnprivilegedSessionReadsWithNoKey(t *testing.T) {
	ctx := context.Background()
	reads := []struct {
		name string
		run  func(c *Client) ([]string, error)
	}{
		{"ArchiveQuery", func(c *Client) ([]string, error) { return c.ArchiveQuery(ctx, "history", "true", 0) }},
		{"ArchiveQuery on Salary", func(c *Client) ([]string, error) {
			return c.ArchiveQuery(ctx, "history", `Salary == "`+archSessionSecret+`-3"`, 0)
		}},
		{"QueryRawProject", func(c *Client) ([]string, error) {
			return c.QueryRawProject(ctx, "history", "true", []string{"ClusterId", "Salary"}, 0)
		}},
		{"TopK", func(c *Client) ([]string, error) {
			return c.TopK(ctx, "history", "true", []string{"ClusterId", "Salary"}, "RequestMemory", true, 5)
		}},
		{"ArchiveAggregate by Salary", func(c *Client) ([]string, error) {
			rows, err := c.ArchiveAggregate(ctx, "history", "true", []string{"Salary"}, []AggSpec{{Func: db.AggCount, Arg: "*"}})
			return archAggText(rows), err
		}},
		{"AggregateTable by Salary", func(c *Client) ([]string, error) {
			rows, err := c.AggregateTable(ctx, "history", "true", []string{"Salary"}, []AggSpec{{Func: db.AggCount, Arg: "*"}})
			return archAggText(rows), err
		}},
		{"WatchTable", func(c *Client) ([]string, error) {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			ch, stop, err := c.WatchTable(wctx, "history", nil)
			if err != nil {
				return nil, err
			}
			defer stop()
			var out []string
			for ev := range ch {
				if ev.AdText != "" {
					out = append(out, ev.AdText)
				}
				if len(out) == 40 {
					break
				}
			}
			return out, nil
		}},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			// Premise: the privileged session sees the value, so withholding it means something.
			pc, pclean := archiveSealedPair(t, true)
			defer pclean()
			prows, err := r.run(pc)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(prows, "\n"), archSessionSecret) {
				t.Fatalf("privileged session did not see the sealed value; premise fails: %.300q", prows)
			}
			uc, uclean := archiveSealedPair(t, false)
			defer uclean()
			urows, err := r.run(uc)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(urows, "\n"); strings.Contains(got, archSessionSecret) {
				t.Fatalf("unprivileged session received a sealed value: %.300q", got)
			}
			if strings.HasSuffix(r.name, "on Salary") {
				if len(urows) != 0 {
					t.Fatalf("an unprivileged constraint on a sealed value matched %d ads", len(urows))
				}
			} else if len(urows) == 0 {
				t.Fatal("the unprivileged session read nothing at all")
			}
		})
	}
}

func archAggText(rows []AggRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r.Group, "|") + "=" + strings.Join(r.Values, "|")
	}
	return out
}
