package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// An archive table seals what a mutable table in the same catalog seals, under a data key resolved
// for its own directory from the same pool keys, and every read a caller not entitled to sealed values
// makes of it answers as the same read of a mutable table would: as if each sealed value were
// undefined. These compare every archive read path against an oracle -- an in-memory table holding
// the same ads in the clear for the entitled reader, and with every sealed attribute undefined for the
// other -- over the stored forms an archive's records reach, and an archive part of whose history was
// written before it had a key.

const archTblN = 400

var archTblPool = []KEK{{ID: "POOL", Material: []byte("archive-encrypt-test-pool-key-material")}}

func archTblToken(kind string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", kind, i)))
	return kind + "-" + hex.EncodeToString(sum[:12])
}

func archTblAd(i int) string {
	return fmt.Sprintf("MyType = \"Job\"\nClusterId = %d\nProcId = 0\nOwner = \"u%d\"\nJobStatus = %d\n"+
		"RequestMemory = %d\nSecretNum = %d\nSalary = %q\nClaimId = %q",
		i, i%7, i%5, 1024+(i*37)%500, 7000+i, archTblToken("SALARY", i), archTblToken("CLAIM", i))
}

// archTblSealed is the sealing policy the catalog below configures: Salary and SecretNum explicitly,
// ClaimId because it is private.
var archTblSealed = []string{"Salary", "SecretNum", "ClaimId"}

func archTblCatalog(t *testing.T, dir string) *Catalog {
	t.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: dir, PoolKeys: archTblPool,
		EncryptedAttrs: []string{"Salary", "SecretNum"}})
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func archTblConfig() ArchiveConfig {
	return ArchiveConfig{SegmentSize: 1 << 14, ZoneAttrs: []string{"ClusterId", "SecretNum"},
		CategoricalAttrs: []string{"Owner"}}
}

// archTblOracles builds the two in-memory reference tables: the ads as written, and the ads as a
// reader with no key sees them.
func archTblOracles(t *testing.T) (plain, withheld *DB) {
	t.Helper()
	var err error
	if plain, err = Open(""); err != nil {
		t.Fatal(err)
	}
	if withheld, err = Open(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close(); withheld.Close() })
	tp, tw := plain.Begin(), withheld.Begin()
	for i := 0; i < archTblN; i++ {
		key := fmt.Sprintf("%d.0", i)
		tp.NewClassAd(key, mustAd(t, archTblAd(i)))
		w := mustAd(t, archTblAd(i))
		for _, n := range archTblSealed {
			w.InsertExpr(n, mustExpr(t, "undefined"))
		}
		tw.NewClassAd(key, w)
	}
	if err := tp.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tw.Commit(); err != nil {
		t.Fatal(err)
	}
	return plain, withheld
}

type archTblShape struct {
	name   string
	legacy bool // the first half written with no key, as an archive was before it had one
	// legacyCols builds the columnar accelerator over the legacy half before the key, so its
	// columns hold SecretNum's values in the clear, and the reopened archive adopts that schema.
	legacyCols bool
	columnar   bool // the columnar accelerator built over the sealed segments
	rewrite    bool // Rewrite re-encodes every record under the key
	reopen     bool
}

// archTblFixture builds the archive in shape s inside a catalog with pool keys and returns the table.
func archTblFixture(t *testing.T, s archTblShape) *ArchiveTable {
	t.Helper()
	root := t.TempDir()
	adir := filepath.Join(root, archivesSubdir, "history")
	start := 0
	if s.legacy {
		a, err := openArchiveTable(adir, archTblConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < archTblN/2; i++ {
			if err := a.AppendOld(archTblAd(i)); err != nil {
				t.Fatal(err)
			}
		}
		if s.legacyCols && !a.BuildAndEnableSchemaScan(2000, 8) {
			t.Fatal("no columnar schema derived")
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		start = archTblN / 2
	}
	cat := archTblCatalog(t, root)
	a, err := cat.CreateArchiveTable("history", archTblConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := start; i < archTblN; i++ {
		if err := a.AppendOld(archTblAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	if s.columnar && !a.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	if s.rewrite && a.Rewrite() == 0 {
		t.Fatal("Rewrite rewrote nothing")
	}
	if s.reopen {
		cat.Close()
		cat = archTblCatalog(t, root)
		var ok bool
		if a, ok = cat.ArchiveTable("history"); !ok {
			t.Fatal("archive not recovered")
		}
	}
	t.Cleanup(func() { cat.Close() })
	return a
}

var archTblShapes = []archTblShape{
	{name: "sealed"},
	{name: "columnar", columnar: true},
	{name: "reopened", columnar: true, reopen: true},
	{name: "legacy", legacy: true},
	{name: "legacy-columnar", legacy: true, columnar: true},
	{name: "legacy-precolumnar", legacy: true, legacyCols: true},
	{name: "legacy-rewritten", legacy: true, rewrite: true, reopen: true},
}

// adFields renders an ad as name -> value text, for comparison regardless of attribute order.
func adFields(ad *classad.ClassAd) map[string]string {
	out := map[string]string{}
	for _, n := range ad.GetAttributes() {
		out[strings.ToLower(n)] = ad.EvaluateAttr(n).String()
	}
	return out
}

func adsByCluster(t *testing.T, seq func(func(*classad.ClassAd) bool)) map[int64]map[string]string {
	t.Helper()
	out := map[int64]map[string]string{}
	for ad := range seq {
		id, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			t.Fatalf("ad without ClusterId: %s", ad.MarshalOld())
		}
		out[id] = adFields(ad)
	}
	return out
}

func sortedRows(rows []AggRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r.Group, "|") + " => " + strings.Join(r.Values, "|")
	}
	sort.Strings(out)
	return out
}

var archTblConstraints = []string{
	`true`,
	`Salary == "` + archTblToken("SALARY", 3) + `"`,
	`Salary is undefined`,
	`SecretNum > 7300`,
	`!(SecretNum <= 7300)`,
	`SecretNum is undefined && Owner == "u2"`,
	`ClusterId < 50 || Salary =!= undefined`,
	`regexp("^SALARY", Salary)`,
	`JobStatus == 2`,
}

var archTblAggs = []AggRequest{
	{Constraint: `true`, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	{Constraint: `Salary == "` + archTblToken("SALARY", 3) + `"`, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	{Constraint: `SecretNum > 7300`, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	{Constraint: `true`, GroupCols: []GroupCol{{Attr: "Owner"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	{Constraint: `true`, GroupCols: []GroupCol{{Attr: "SecretNum", BucketWidth: 100}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	{Constraint: `ClusterId >= 0`, GroupCols: []GroupCol{{Attr: "Salary"}}, Aggs: []AggSpec{{Func: AggCount, Arg: "*"}}},
	{Constraint: `SecretNum > 7300`, GroupCols: []GroupCol{{Attr: "Owner"}}, Aggs: []AggSpec{{Func: AggSum, Arg: "RequestMemory"}}},
	{Constraint: `true`, Aggs: []AggSpec{{Func: AggMax, Arg: "SecretNum"}}},
	{Constraint: `ClusterId > 10`, Aggs: []AggSpec{{Func: AggMin, Arg: "SecretNum"}}},
	{Constraint: `true`, GroupCols: []GroupCol{{Attr: "Owner"}}, Aggs: []AggSpec{{Func: AggMax, Arg: "SecretNum"}}},
	{Constraint: `true`, Aggs: []AggSpec{{Func: AggCount, Arg: "SecretNum"}}},
	{Constraint: `true`, GroupCols: []GroupCol{{Attr: "JobStatus"}},
		Aggs: []AggSpec{{Func: AggCount, Arg: "*", Filter: `SecretNum > 7300`}, {Func: AggSum, Arg: "RequestMemory", Filter: `Salary =!= undefined`}}},
	{Constraint: `Salary is undefined`, GroupCols: []GroupCol{{Attr: "JobStatus"}}, Aggs: []AggSpec{{Func: AggMax, Arg: "RequestMemory"}}},
}

type archTblTopK struct {
	constraint string
	attrs      []string
	order      string
	desc       bool
	k          int
}

var archTblTopKs = []archTblTopK{
	{`true`, []string{"ClusterId", "Salary"}, "SecretNum", true, 5},
	{`true`, []string{"ClusterId", "Salary"}, "RequestMemory", true, 5},
	{`Salary =!= undefined`, []string{"ClusterId"}, "RequestMemory", false, 7},
	{`SecretNum > 7300`, []string{"ClusterId", "SecretNum"}, "ClusterId", true, 4},
}

func topKText(rows [][]classad.Value) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = v.String()
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

// TestArchiveTableReadsMatchOracle: for every archive read path, the entitled read equals the plaintext
// oracle and the redacted read equals the withheld one.
func TestArchiveTableReadsMatchOracle(t *testing.T) {
	t.Parallel()
	plain, withheld := archTblOracles(t)
	for _, s := range archTblShapes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			a := archTblFixture(t, s)
			for _, c := range archTblConstraints {
				for _, redact := range []bool{false, true} {
					oracle := plain
					query := a.QueryLimit
					if redact {
						oracle, query = withheld, a.QueryLimitRedacted
					}
					want, err := oracle.Query(c)
					if err != nil {
						t.Fatal(err)
					}
					got, err := query(c, 0)
					if err != nil {
						t.Fatal(err)
					}
					w, g := adsByCluster(t, want), adsByCluster(t, got)
					if len(w) != len(g) {
						t.Errorf("redact=%v %s: %d ads, oracle %d", redact, c, len(g), len(w))
						continue
					}
					for id, wf := range w {
						if gf := g[id]; fmt.Sprint(gf) != fmt.Sprint(wf) {
							t.Errorf("redact=%v %s: ad %d = %v, oracle %v", redact, c, id, gf, wf)
							break
						}
					}
				}
			}
			for _, req := range archTblAggs {
				for _, redact := range []bool{false, true} {
					oracle, aggregate := plain, a.AggregateColsStats
					if redact {
						oracle, aggregate = withheld, a.AggregateColsStatsRedacted
					}
					want, err := oracle.AggregateColsStats(req.Constraint, req.GroupCols, req.Aggs, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					got, err := aggregate(req.Constraint, req.GroupCols, req.Aggs, nil)
					if err != nil {
						t.Fatal(err)
					}
					if g, w := sortedRows(got), sortedRows(want); !slices.Equal(g, w) {
						t.Errorf("redact=%v aggregate %+v:\n got %v\nwant %v", redact, req, g, w)
					}
				}
			}
			for _, k := range archTblTopKs {
				for _, redact := range []bool{false, true} {
					oracle, topK := plain, a.TopK
					if redact {
						oracle, topK = withheld, a.TopKRedacted
					}
					want, err := oracle.TopK(k.constraint, k.attrs, k.order, k.desc, k.k)
					if err != nil {
						t.Fatal(err)
					}
					got, err := topK(k.constraint, k.attrs, k.order, k.desc, k.k)
					if err != nil {
						t.Fatal(err)
					}
					if g, w := topKText(got), topKText(want); !slices.Equal(g, w) {
						t.Errorf("redact=%v top-K %+v:\n got %v\nwant %v", redact, k, g, w)
					}
				}
			}
			// The raw projection a server relays: the sealed attributes undefined, nothing else changed.
			for ra := range mustSeq(a.QueryRawProjected(`true`, []string{"ClusterId", "Salary", "ClaimId", "SecretNum"}, true)) {
				text := string(rawJoin(ra))
				if strings.Contains(text, "SALARY-") || strings.Contains(text, "CLAIM-") || strings.Contains(text, "SecretNum = 7") {
					t.Fatalf("redacted raw projection emitted a sealed value: %q", text)
				}
			}
		})
	}
}

func mustSeq[T any](seq func(func(T) bool), err error) func(func(T) bool) {
	if err != nil {
		panic(err)
	}
	return seq
}

func rawJoin(ra collections.RawAd) []byte {
	var b []byte
	for _, e := range ra.Exprs {
		b = append(b, e...)
		b = append(b, '\n')
	}
	return b
}

// TestArchiveTableSealsWithCatalogKey: an archive in a catalog with pool keys is encrypted at rest --
// its master key wrapped under them beside its data -- and its sealed values cannot be read from its
// files without that key. One in a catalog without pool keys still seals, as a mutable table does,
// and says it is not encrypted at rest.
func TestArchiveTableSealsWithCatalogKey(t *testing.T) {
	t.Parallel()
	for _, keyed := range []bool{true, false} {
		t.Run(fmt.Sprintf("poolkeys=%v", keyed), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			cfg := CatalogConfig{Dir: root, EncryptedAttrs: []string{"Salary", "SecretNum"}}
			if keyed {
				cfg.PoolKeys = archTblPool
			}
			cat, err := OpenCatalogConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			a, err := cat.CreateArchiveTable("history", archTblConfig())
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < archTblN; i++ {
				if err = a.AppendOld(archTblAd(i)); err != nil {
					t.Fatal(err)
				}
			}
			if a.EncryptionEnabled() != keyed {
				t.Errorf("EncryptionEnabled = %v with pool keys %v", a.EncryptionEnabled(), keyed)
			}
			if !a.SealsValues() {
				t.Error("an archive in a persistent catalog seals nothing")
			}
			if got := a.EncryptedAttrNames(); !slices.Equal(got, []string{"salary", "secretnum"}) {
				t.Errorf("EncryptedAttrNames = %v", got)
			}
			keyFile := unprotectedMasterFile
			if keyed {
				keyFile = masterKeysFile
			}
			adir := filepath.Join(root, archivesSubdir, "history")
			if _, err = os.Stat(filepath.Join(adir, keyFile)); err != nil {
				t.Errorf("no %s beside the archive: %v", keyFile, err)
			}
			if err = cat.Close(); err != nil {
				t.Fatal(err)
			}

			// The files read with no key: every sealed value is unreadable, every other one is there.
			codec, err := collections.NewZSTDCodec(nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := collections.OpenArchive(collections.ArchiveOptions{Dir: adir, SegmentSize: 1 << 14, Codec: codec})
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			for _, n := range archTblSealed {
				for vals := range raw.QueryProject(mustVMQuery(t, `true`), []string{n}) {
					if vals[0].IsString() || vals[0].IsInteger() {
						t.Fatalf("%s read from the files with no key: %s", n, vals[0].String())
					}
				}
			}
			rows := 0
			for vals := range raw.QueryProject(mustVMQuery(t, `true`), []string{"Owner"}) {
				if s, err := vals[0].StringValue(); err != nil || !strings.HasPrefix(s, "u") {
					t.Fatalf("Owner = %s with no key; the files do not hold what was appended", vals[0].String())
				}
				rows++
			}
			if rows != archTblN {
				t.Fatalf("read %d records from the files, want %d", rows, archTblN)
			}
		})
	}
}

// TestArchiveTableEncryptedAttrsPersist: a runtime change to the explicit set survives a restart, as a
// mutable table's does, and an archive that never changed it takes the catalog's.
func TestArchiveTableEncryptedAttrsPersist(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cat := archTblCatalog(t, root)
	a, err := cat.CreateArchiveTable("history", archTblConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, err := cat.CreateArchiveTable("other", archTblConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetEncryptedAttrs([]string{"Owner"}); err == nil {
		t.Fatal("sealing an indexed attribute was accepted")
	}
	if err := a.SetEncryptedAttrs([]string{"Salary", "JobStatus"}); err != nil {
		t.Fatal(err)
	}
	_ = b
	cat.Close()
	cat = archTblCatalog(t, root)
	defer cat.Close()
	a, _ = cat.ArchiveTable("history")
	b, _ = cat.ArchiveTable("other")
	if got := a.EncryptedAttrNames(); !slices.Equal(got, []string{"jobstatus", "salary"}) {
		t.Errorf("after reopen the archive seals %v, want its runtime set", got)
	}
	if got := b.EncryptedAttrNames(); !slices.Equal(got, []string{"salary", "secretnum"}) {
		t.Errorf("an archive with no runtime set seals %v, want the catalog's", got)
	}
}

// TestArchiveTableSealedIndexDropped: an archive that indexed an attribute before it was sealed opens
// once the catalog seals it -- the sealing wins, as it does for a mutable table's persisted indexes --
// rather than refusing the open, which would take the catalog down with it.
func TestArchiveTableSealedIndexDropped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	adir := filepath.Join(root, archivesSubdir, "history")
	cfg := archTblConfig()
	cfg.CategoricalAttrs = []string{"Owner", "Salary"}
	cfg.ValueAttrs = []string{"SecretNum"}
	a, err := openArchiveTable(adir, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := a.AppendOld(archTblAd(i)); err != nil {
			t.Fatal(err)
		}
	}
	a.Close()
	cat := archTblCatalog(t, root)
	defer cat.Close()
	a, ok := cat.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered")
	}
	if got := a.SealedIndexes(); !slices.Equal(got, []string{"Salary", "SecretNum"}) {
		t.Errorf("SealedIndexes = %v", got)
	}
	catIdx, valIdx := a.IndexedAttrs()
	for _, n := range append(catIdx, valIdx...) {
		if strings.EqualFold(n, "Salary") || strings.EqualFold(n, "SecretNum") {
			t.Fatalf("a sealed attribute is still indexed: %v %v", catIdx, valIdx)
		}
	}
	if err := a.AppendOld(archTblAd(51)); err != nil {
		t.Fatal(err)
	}
}

func mustVMQuery(t *testing.T, s string) *vm.Query {
	t.Helper()
	q, err := vm.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func mustExpr(t *testing.T, s string) *classad.Expr {
	t.Helper()
	e, err := classad.ParseExpr(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
