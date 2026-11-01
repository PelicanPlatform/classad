package dbrpc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// An unprivileged session evaluates as if the table had no key. The name gates refuse private
// attributes; these check the rest: an attribute sealed by EncryptedAttrs is not private by name, so
// before this the constraint, the groups, the order and the match were all computed from its plaintext
// and only the returned VALUES were redacted. Whether a row matched, how many did, what the group keys
// were and which rows sorted first were all answers about the secret.
//
// Every operation runs three ways over the same rows:
//
//   - on a table that seals Salary and Note, by an unprivileged session -- which must answer exactly
//     as that session answers over the same rows with Salary and Note REMOVED;
//   - on the sealing table by a privileged session -- which must answer exactly as it does over a
//     table holding them in the clear, so the privileged path is unchanged.
//
// Each probe is also checked to discriminate: the plaintext answer differs from the redacted one, or
// the comparison proves nothing.

var keylessSealed = []string{"Salary", "Note"}

const keylessJobs, keylessSlots = 42, 12

func keylessJobAd(i int, hide bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ClusterId = %d\nProcId = 0\nOwner = \"u%d\"\nCpus = %d\nJobStatus = %d\n",
		i, i%4, 1+i%4, 1+i%3)
	if !hide {
		fmt.Fprintf(&b, "Salary = %d\nNote = \"n%d\"\n", 1000*(i%7), i%3)
	}
	b.WriteString("Requirements = TARGET.Salary >= 3000\nRank = TARGET.Salary\n")
	return b.String()
}

func keylessSlotAd(i int, hide bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Name = \"slot%02d\"\nMemory = %d\nRequirements = true\n", i, 1024*(1+i%3))
	if !hide {
		fmt.Fprintf(&b, "Salary = %d\nNote = \"s%d\"\n", 500*i, i%2)
	}
	return b.String()
}

// keylessCatalog opens a persistent catalog with tables jobs and slots. sealed names the attributes it
// seals beyond the private ones; hide stores the ads without Salary and Note at all.
func keylessCatalog(t *testing.T, sealed []string, hide bool) *db.Catalog {
	t.Helper()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{
		Dir:            t.TempDir(),
		PoolKeys:       []db.KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: sealed,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	put := func(table string, n int, ad func(int, bool) string) {
		d, err := cat.CreateTable(table)
		if err != nil {
			t.Fatal(err)
		}
		d.SetTimeTravel(time.Hour, time.Millisecond)
		tx := d.Begin()
		for i := 0; i < n; i++ {
			a, err := classad.ParseOld(ad(i, hide))
			if err != nil {
				t.Fatal(err)
			}
			tx.NewClassAd(fmt.Sprintf("%d.0", i), a)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put("jobs", keylessJobs, keylessJobAd)
	put("slots", keylessSlots, keylessSlotAd)
	return cat
}

func keylessClient(t *testing.T, cat *db.Catalog, opts ServeOptions) *Client {
	t.Helper()
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	t.Cleanup(func() { c.Close(); s.Close() })
	return c
}

// parseRow parses a returned row in either form the read ops ship: new-ClassAd text or old-ClassAd lines.
func parseRow(t *testing.T, r string) *classad.ClassAd {
	t.Helper()
	parse := classad.ParseOld
	if strings.HasPrefix(strings.TrimSpace(r), "[") {
		parse = classad.Parse
	}
	ad, err := parse(r)
	if err != nil {
		t.Fatalf("unparseable row %q: %v", r, err)
	}
	return ad
}

// keylessProbe is one operation, rendered to a canonical string so two tables' answers compare.
type keylessProbe struct {
	name string
	run  func(ctx context.Context, c *Client) (string, error)
}

func sortedJoin(rows []string) string {
	rows = slices.Clone(rows)
	slices.Sort(rows)
	return strings.Join(rows, " | ")
}

// clusterIDs reduces old-ClassAd rows to their sorted ClusterId values, which is what identifies a
// row across the three tables (their other attributes differ by construction).
func clusterIDs(t *testing.T, rows []string) string {
	t.Helper()
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ad := parseRow(t, r)
		v := ad.EvaluateAttr("ClusterId")
		ids = append(ids, v.String())
	}
	return sortedJoin(ids)
}

func aggRows(rows []AggRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r.Group, ",") + "=" + strings.Join(r.Values, ",")
	}
	return sortedJoin(out)
}

func keylessProbes(t *testing.T) []keylessProbe {
	const where = "Salary >= 3000"
	agg := func(constraint string, groups []GroupCol, aggs ...AggSpec) func(context.Context, *Client) (string, error) {
		return func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.AggregateBucketedTable(ctx, "jobs", constraint, groups, aggs)
			return aggRows(rows), err
		}
	}
	return []keylessProbe{
		{"Query", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.QueryTable(ctx, "jobs", where, 0)
			return clusterIDs(t, rows), err
		}},
		{"QueryRawProject", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.QueryRawProject(ctx, "jobs", `Note == "n1"`, []string{"ClusterId"}, 0)
			return clusterIDs(t, rows), err
		}},
		{"QueryRawProjectRefs", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.QueryRawProjectRefs(ctx, "jobs", where, []string{"ClusterId"}, 0)
			return clusterIDs(t, rows), err
		}},
		{"QueryRawProjectedFromSeq", func(ctx context.Context, c *Client) (string, error) {
			rows, _, err := c.QueryRawProjectedFromSeq(ctx, "jobs", where, []string{"ClusterId"}, SeqCursor{}, 0)
			return clusterIDs(t, rows), err
		}},
		{"QueryKeys", func(ctx context.Context, c *Client) (string, error) {
			keys, err := c.QueryKeysTable(ctx, "jobs", where)
			return sortedJoin(keys), err
		}},
		{"QueryAsOf", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.QueryAsOfTable(ctx, "jobs", where, 0, time.Now())
			return clusterIDs(t, rows), err
		}},
		{"TxnQuery", func(ctx context.Context, c *Client) (string, error) {
			tx, err := c.BeginTable(ctx, "jobs")
			if err != nil {
				return "", err
			}
			defer tx.Abort(ctx)
			rows, err := tx.Query(ctx, where, 0)
			if err != nil {
				return "", err
			}
			keys, err := tx.KeysWhere(ctx, where)
			return clusterIDs(t, rows) + " / " + sortedJoin(keys), err
		}},
		{"CountWhere", agg(where, nil, AggSpec{Func: AggCount, Arg: "*"})},
		{"CountWhereString", agg(`Note == "n1"`, nil, AggSpec{Func: AggCount, Arg: "*"})},
		{"GroupBySealed", agg("true", []GroupCol{{Attr: "Salary"}}, AggSpec{Func: AggCount, Arg: "*"})},
		{"GroupBySealedBucketed", agg("true", []GroupCol{{Attr: "Salary", BucketWidth: 2000}}, AggSpec{Func: AggCount, Arg: "*"})},
		{"AggregateSealed", agg("true", nil, AggSpec{Func: AggSum, Arg: "Salary"}, AggSpec{Func: AggMin, Arg: "Salary"},
			AggSpec{Func: AggMax, Arg: "Salary"}, AggSpec{Func: AggCount, Arg: "Salary"})},
		{"GroupFilterSealed", agg("true", []GroupCol{{Attr: "Owner"}}, AggSpec{Func: AggCount, Arg: "*", Filter: where})},
		{"TopKWhere", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.TopK(ctx, "jobs", where, []string{"ClusterId"}, "Cpus", true, 50)
			return clusterIDs(t, rows), err
		}},
		{"TopKOrderSealed", func(ctx context.Context, c *Client) (string, error) {
			// Ties in the order value leave which tied rows make the cut unspecified, so compare the
			// order values the cut returned, not the rows.
			rows, err := c.TopK(ctx, "jobs", "true", []string{"Salary"}, "Salary", true, 5)
			return strings.Join(rows, " | "), err
		}},
		{"MatchSorted", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.MatchSortedTable(ctx, "slots", "Requirements = TARGET.Salary >= 3000\nRank = TARGET.Salary\n", 0)
			names := make([]string, 0, len(rows))
			for _, r := range rows {
				names = append(names, parseRow(t, r).EvaluateAttr("Name").String())
			}
			return strings.Join(names, " "), err // rank order is the answer here
		}},
		{"MatchExplain", func(ctx context.Context, c *Client) (string, error) {
			// The selector picks the request, so whether one matches is the answer; a plan that is
			// found bakes the request's own values into the slot predicate it shows.
			ex, err := c.MatchExplain(ctx, "jobs", `Note == "n1"`, "slots", "")
			if err != nil {
				return "error: " + err.Error(), nil
			}
			return ex.SlotPredicate, nil
		}},
		{"MatchTables", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.MatchTables(ctx, "jobs", "slots", "Name", "ClusterId < 10", "", 0, nil)
			out := make([]string, len(rows))
			for i, r := range rows {
				out[i] = r.Request + "->" + r.Resource + "@" + r.Rank
			}
			return sortedJoin(out), err
		}},
		{"MatchTablesFiltered", func(ctx context.Context, c *Client) (string, error) {
			rows, err := c.MatchTables(ctx, "jobs", "slots", "Name", "Note == \"n1\"", `Note == "s1"`, 0, nil)
			out := make([]string, len(rows))
			for i, r := range rows {
				out[i] = r.Request + "->" + r.Resource
			}
			return sortedJoin(out), err
		}},
	}
}

// TestUnprivilegedEvaluationWithoutKey runs every probe against the three tables (see the file comment).
func TestUnprivilegedEvaluationWithoutKey(t *testing.T) {
	enc := keylessCatalog(t, keylessSealed, false)
	plain := keylessCatalog(t, nil, false)
	hidden := keylessCatalog(t, nil, true)
	unpriv := ServeOptions{ReadOnly: true}
	priv := ServeOptions{IncludePrivate: true, Privileged: true}
	encU, hidU := keylessClient(t, enc, unpriv), keylessClient(t, hidden, unpriv)
	encP, plainP := keylessClient(t, enc, priv), keylessClient(t, plain, priv)
	ctx := context.Background()
	for _, p := range keylessProbes(t) {
		t.Run(p.name, func(t *testing.T) {
			got := func(c *Client) string {
				s, err := p.run(ctx, c)
				if err != nil {
					t.Fatalf("%s: %v", p.name, err)
				}
				return s
			}
			eu, hu, ep, pp := got(encU), got(hidU), got(encP), got(plainP)
			if pp == hu {
				t.Fatalf("the probe does not discriminate: plaintext and absent both give %q", pp)
			}
			if eu != hu {
				t.Errorf("unprivileged session evaluated a sealed value:\n got  %q\n want %q (the same rows without it)\n plaintext answer %q", eu, hu, pp)
			}
			if ep != pp {
				t.Errorf("privileged session changed:\n got  %q\n want %q (the same rows in the clear)", ep, pp)
			}
		})
	}
}

// TestUnprivilegedDeleteWhereWithoutKey: a WRITE session that is not privileged deletes by a constraint
// evaluated with no key -- how many rows it removes is an answer about a sealed value.
func TestUnprivilegedDeleteWhereWithoutKey(t *testing.T) {
	ctx := context.Background()
	writer := ServeOptions{}
	for _, tc := range []struct {
		name, constraint string
	}{
		{"numeric", "Salary >= 3000"},
		{"string", `Note == "n1" || Note == "n2"`},
		// Matches every row once Salary reads as undefined: the deleting transaction's re-check has to
		// read with no key as the scan did, or it spares every row the scan chose.
		{"absent", `Salary =?= undefined`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			del := func(cat *db.Catalog, opts ServeOptions) int {
				n, err := keylessClient(t, cat, opts).DeleteWhereTable(ctx, "jobs", tc.constraint)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			plain := del(keylessCatalog(t, nil, false), ServeOptions{IncludePrivate: true, Privileged: true})
			hidden := del(keylessCatalog(t, nil, true), writer)
			if plain == hidden {
				t.Fatalf("the constraint does not discriminate: %d rows either way", plain)
			}
			if got := del(keylessCatalog(t, keylessSealed, false), writer); got != hidden {
				t.Errorf("unprivileged DeleteWhere removed %d rows, want %d (the plaintext would remove %d)", got, hidden, plain)
			}
			if got := del(keylessCatalog(t, keylessSealed, false), ServeOptions{IncludePrivate: true, Privileged: true}); got != plain {
				t.Errorf("privileged DeleteWhere removed %d rows, want %d", got, plain)
			}
		})
	}
}

// TestViewBuiltWithoutKey: a materialized view is shared by every session that can name it, so it is
// built as an unentitled reader sees the base table. Grouped by a sealed attribute it has one group,
// undefined, exactly as over rows without it -- for every session that reads it.
func TestViewBuiltWithoutKey(t *testing.T) {
	ctx := context.Background()
	spec := db.ViewSpec{
		BaseTable:   "jobs",
		Groups:      []db.ViewGroupCol{{Attr: "Salary", Alias: "label_salary"}},
		Metrics:     []db.ViewMetric{{Func: db.ViewCount, Arg: "*", Alias: "metric_jobs"}},
		Cardinality: 100,
	}
	read := func(cat *db.Catalog, opts ServeOptions) string {
		c := keylessClient(t, cat, opts)
		if err := c.CreateView(ctx, "by_salary", spec); err != nil {
			t.Fatal(err)
		}
		var rows []string
		for i := 0; i < 50; i++ {
			var err error
			if rows, err = c.QueryTable(ctx, "by_salary", "true", 0); err != nil {
				t.Fatal(err)
			}
			if len(rows) > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond) // the view builds from the change stream
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			ad := parseRow(t, r)
			out[i] = ad.EvaluateAttr("label_salary").String() + "=" + ad.EvaluateAttr("metric_jobs").String()
		}
		return sortedJoin(out)
	}
	plain := read(keylessCatalog(t, nil, false), ServeOptions{})
	hidden := read(keylessCatalog(t, nil, true), ServeOptions{})
	if plain == hidden {
		t.Fatalf("the view does not discriminate: %q either way", plain)
	}
	for _, opts := range []ServeOptions{{}, {IncludePrivate: true, Privileged: true}} {
		if got := read(keylessCatalog(t, keylessSealed, false), opts); got != hidden {
			t.Errorf("view over a sealed attribute read by %+v = %q, want %q (the plaintext gives %q)", opts, got, hidden, plain)
		}
	}
}

// TestOrderedRefusedWithoutKey: an ordered index is maintained with the key -- membership, partition,
// order and cluster signature all come from the plaintext -- so an unprivileged session may not read one
// on a table that seals values: probing a partition by value or reading the order would answer for a
// sealed value the spec reads. A privileged session, and any session on a table that seals nothing, read
// it as before.
func TestOrderedRefusedWithoutKey(t *testing.T) {
	ctx := context.Background()
	spec := []db.OrderSpec{{Partition: "Owner", Keys: []db.SortKey{{Expr: "Salary", Desc: true}}}}
	open := func(dir string) *db.DB {
		cfg := db.Config{Dir: dir, Ordered: spec}
		if dir != "" {
			cfg.EncryptedAttrs = keylessSealed
		}
		d, err := db.OpenConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		tx := d.Begin()
		for i := 0; i < 10; i++ {
			a, err := classad.ParseOld(keylessJobAd(i, false))
			if err != nil {
				t.Fatal(err)
			}
			tx.NewClassAd(fmt.Sprintf("%d.0", i), a)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return d
	}
	read := func(d *db.DB, opts ServeOptions) ([]OrderedRow, error) {
		s := NewServer(d)
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConnOpts(sconn, opts) }()
		c := NewClient(cconn)
		defer func() { c.Close(); s.Close() }()
		return c.Ordered(ctx, 0, "u3")
	}
	sealing := open(t.TempDir())
	if _, err := read(sealing, ServeOptions{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "DAEMON") {
		t.Errorf("unprivileged ordered read on a sealing table: err = %v, want a refusal", err)
	}
	if rows, err := read(sealing, ServeOptions{IncludePrivate: true, Privileged: true}); err != nil || len(rows) == 0 {
		t.Errorf("privileged ordered read: %d rows, err %v", len(rows), err)
	}
	if rows, err := read(open(""), ServeOptions{ReadOnly: true}); err != nil || len(rows) == 0 {
		t.Errorf("unprivileged ordered read on a table that seals nothing: %d rows, err %v", len(rows), err)
	}
}
