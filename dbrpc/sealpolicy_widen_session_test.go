package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// A table's explicit sealed set widened over the wire (encrypt.set) governs what an unprivileged session
// reads of every record from that moment, including the records written before it, which still hold the
// newly sealed attribute in the clear until maintenance migrates them.

const widenSessionSecret = "widened-payroll-51e0a-not-a-private-name"

func TestWidenedSealPolicyUnprivilegedSession(t *testing.T) {
	ctx := context.Background()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir(),
		PoolKeys: []db.KEK{{ID: "POOL", Material: []byte("widen-session-pool-key-material!!!!")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	tbl, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId = %d; Owner = "u%d"; RequestMemory = %d; Salary = "%s-%d" ]`,
			i, i%3, 100+i, widenSessionSecret, i))
		if err != nil {
			t.Fatal(err)
		}
		if err := tbl.Put(fmt.Sprintf("%d.0", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServerCatalog(cat)
	defer s.Close()
	connect := func(opts ServeOptions) *Client {
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConnOpts(sconn, opts) }()
		c := NewClient(cconn)
		t.Cleanup(func() { c.Close() })
		return c
	}
	pc := connect(ServeOptions{IncludePrivate: true, Privileged: true})
	uc := connect(ServeOptions{ReadOnly: true})

	reads := []struct {
		name string
		run  func(c *Client) ([]string, error)
	}{
		{"QueryTable", func(c *Client) ([]string, error) { return c.QueryTable(ctx, "jobs", "true", 0) }},
		{"QueryTable on Salary", func(c *Client) ([]string, error) {
			return c.QueryTable(ctx, "jobs", `Salary == "`+widenSessionSecret+`-3"`, 0)
		}},
		{"QueryRawTable", func(c *Client) ([]string, error) { return c.QueryRawTable(ctx, "jobs", "true", 0) }},
		{"QueryRawProject", func(c *Client) ([]string, error) {
			return c.QueryRawProject(ctx, "jobs", "true", []string{"ClusterId", "Salary"}, 0)
		}},
		{"TopK", func(c *Client) ([]string, error) {
			return c.TopK(ctx, "jobs", "true", []string{"ClusterId", "Salary"}, "RequestMemory", true, 5)
		}},
		{"AggregateTable by Salary", func(c *Client) ([]string, error) {
			rows, err := c.AggregateTable(ctx, "jobs", "true", []string{"Salary"}, []AggSpec{{Func: db.AggCount, Arg: "*"}})
			return archAggText(rows), err
		}},
		{"WatchTable", func(c *Client) ([]string, error) {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			ch, stop, err := c.WatchTable(wctx, "jobs", nil)
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
	// Before the widen Salary is an ordinary attribute, which both sessions read.
	if rows, err := uc.QueryTable(ctx, "jobs", `Salary == "`+widenSessionSecret+`-3"`, 0); err != nil || len(rows) != 1 {
		t.Fatalf("before the widen the unprivileged session matched %d rows (%v)", len(rows), err)
	}
	if _, err := pc.SetEncryptedAttrs(ctx, "jobs", "Salary"); err != nil {
		t.Fatal(err)
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			prows, err := r.run(pc)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(prows, "\n"), widenSessionSecret) {
				t.Fatalf("privileged session did not see the value; premise fails: %.300q", prows)
			}
			urows, err := r.run(uc)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(urows, "\n"); strings.Contains(got, widenSessionSecret) {
				t.Fatalf("unprivileged session received a newly sealed value: %.300q", got)
			}
			if strings.HasSuffix(r.name, "on Salary") {
				if len(urows) != 0 {
					t.Fatalf("an unprivileged constraint on a newly sealed value matched %d ads", len(urows))
				}
			} else if len(urows) == 0 {
				t.Fatal("the unprivileged session read nothing at all")
			}
		})
	}
}
