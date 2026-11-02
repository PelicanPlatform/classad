package collections

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// An archive with a data key seals what a mutable table seals: every private attribute and the
// explicit EncryptedAttrs set. These pin that for every stored form an archive's records reach -- the
// sealed inline segment, an interned one, a columnarized one, one recompressed under a trained
// dictionary, merged ones, a reopened archive -- and for an archive whose older records were written
// before it had a key and so still hold the values in the clear.
//
// Two secrets per record, both unique, so a leak is found byte-for-byte wherever it lands: Salary is
// sealed only because EncryptedAttrs names it, ClaimId because it is private. SecretNum is a NUMERIC
// sealed attribute, which is what a zone map or a column would be built from.

const (
	archEncSalary = "SALARYSECRET"
	archEncClaim  = "CLAIMSECRET"
	archEncN      = 600
)

func archEncAd(i int) string {
	return fmt.Sprintf(`[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=0; Owner="u%d"; JobStatus=%d;`+
		` RequestMemory=%d; SecretNum=%d; Salary="%s"; ClaimId="%s"; Pad="%0160d" ]`,
		i, i%7, i%5, 1024+i, 7000+i, archEncSecret(archEncSalary, i), archEncSecret(archEncClaim, i), i)
}

// archEncSecret is record i's value for a secret: the kind, then a token that differs in every byte
// from record to record. The token is what the on-disk search looks for. A secret that differed only
// in a counter would compress to a back-reference plus a digit or two, and a search for the whole
// value would find nothing whether or not it was sealed.
func archEncSecret(kind string, i int) string {
	return kind + "-" + archEncToken(kind, i)
}

func archEncToken(kind string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", kind, i)))
	return hex.EncodeToString(sum[:12])
}

func archEncOpts(dir string, key bool) ArchiveOptions {
	o := ArchiveOptions{
		Dir: dir, SegmentSize: 1 << 15,
		ZoneAttrs:        []string{"ClusterId", "SecretNum"},
		CategoricalAttrs: []string{"Owner"},
	}
	if key {
		o.DataKey = sealedRefKey()
		o.EncryptedAttrs = []string{"Salary", "SecretNum"}
	}
	return o
}

type archEncShape struct {
	name   string
	legacy bool // the first half appended before the archive had a key
	// legacyCols columnarizes the legacy half before the archive has a key, so its columns -- and
	// the schema adopted from them on reopen -- hold sealed attributes' values in the clear.
	legacyCols bool
	interned   bool // InternSealed
	dict       bool // RetrainDict (also interns)
	columnar   bool // schema scan on, sealed segments columnarized
	merged     bool // MergePass folds segments together
	reopen     bool // closed and reopened with the key
}

var archEncShapes = []archEncShape{
	{name: "inline"},
	{name: "interned", interned: true},
	{name: "dict", dict: true},
	{name: "columnar", columnar: true},
	{name: "interned-columnar", interned: true, columnar: true},
	{name: "merged", merged: true},
	{name: "reopened", interned: true, columnar: true, reopen: true},
	{name: "legacy", legacy: true},
	{name: "legacy-precolumnar", legacy: true, legacyCols: true},
	{name: "legacy-columnar", legacy: true, columnar: true},
	{name: "legacy-interned-reopened", legacy: true, interned: true, reopen: true},
}

func archEncAppend(t *testing.T, a *Archive, lo, hi int) {
	t.Helper()
	for i := lo; i < hi; i++ {
		if err := a.Append(mustAd(t, archEncAd(i))); err != nil {
			t.Fatal(err)
		}
	}
}

// archEncFixture builds an archive of archEncN records in shape s and returns it with its directory.
// Its last records sit in the active segment, so every read also meets records nothing rewrote.
func archEncFixture(t *testing.T, s archEncShape) (*Archive, string) {
	t.Helper()
	dir := t.TempDir()
	start := 0
	if s.legacy {
		// What an archive written by a build that gave archives no key holds: every value in the clear.
		a, err := OpenArchive(archEncOpts(dir, false))
		if err != nil {
			t.Fatal(err)
		}
		archEncAppend(t, a, 0, archEncN/2)
		if s.legacyCols {
			if !a.BuildAndEnableSchemaScan(2000, 8) {
				t.Fatal("no columnar schema derived")
			}
			a.c.ColumnarizeSealed()
			if columnarSegmentsForTest(a.c) == 0 {
				t.Fatal("fixture: no columnarized segment")
			}
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		start = archEncN / 2
	}
	a, err := OpenArchive(archEncOpts(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	archEncAppend(t, a, start, archEncN*3/4)
	if s.dict {
		if _, err = a.RetrainDict(2000); err != nil {
			t.Fatal(err)
		}
	}
	if s.interned {
		a.c.InternSealed()
	}
	if s.columnar {
		if !a.BuildAndEnableSchemaScan(2000, 8) {
			t.Fatal("no columnar schema derived")
		}
		a.c.ColumnarizeSealed()
		if columnarSegmentsForTest(a.c) == 0 {
			t.Fatal("fixture: no columnarized segment")
		}
	}
	if s.merged {
		if a.MergePass(MergeOptions{TargetSegments: 2, TriggerSegments: 2, MinMergeBytes: 1, KeepRecent: 1}) == 0 {
			t.Fatal("fixture: nothing merged")
		}
	}
	archEncAppend(t, a, archEncN*3/4, archEncN)
	if s.reopen {
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		if a, err = OpenArchive(archEncOpts(dir, true)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { a.Close() })
	return a, dir
}

// archEncDiskSecrets returns which record numbers' secrets are stored in the clear, searched two ways.
//
// Every file under dir, as raw bytes, catches what is stored uncompressed: identity-codec records, index
// sidecars, trained dictionaries. It is not enough on its own: a compressed form (a row group, a record
// under a trained dictionary) Huffman-codes its literals, so a plaintext value there is not a byte
// substring of the file, and a search of the files alone finds nothing whether or not it was sealed.
// So every stored record is also read back DECOMPRESSED but not decoded -- reassembled from its columns
// where it has any -- which is the form a sealed value is sealed in: an nEncrypted node is ciphertext
// there, and a value stored in the clear is its plain bytes.
func archEncDiskSecrets(t *testing.T, a *Archive, dir string) map[int]bool {
	t.Helper()
	var all []byte
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		all = append(all, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			for off := 0; off < seg.used; {
				total := recTotalLen(seg.data, uint32(off))
				if total == 0 {
					break
				}
				if !recIsMarker(seg.data, uint32(off)) {
					w, err := a.c.recordWireIn(seg, seg.data, uint32(off), nil)
					if err != nil {
						t.Fatalf("segment %d record at %d: %v", seg.id, off, err)
					}
					all = append(all, w...)
					records++
				}
				off += int(total)
			}
		}
	}
	if records != archEncN {
		t.Fatalf("read back %d stored records, want %d", records, archEncN)
	}
	found := map[int]bool{}
	for i := 0; i < archEncN; i++ {
		for _, sec := range []string{archEncSalary, archEncClaim} {
			if bytes.Contains(all, []byte(archEncToken(sec, i))) {
				found[i] = true
			}
		}
	}
	return found
}

// TestArchiveSealsAtRest: no record appended with the key leaves either secret on disk, in any file the
// archive writes, in any shape. Records appended before the archive had a key keep theirs until a
// rewrite reseals them, which the second half checks: after Rewrite nothing is left in the clear.
func TestArchiveSealsAtRest(t *testing.T) {
	t.Parallel()
	for _, s := range archEncShapes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			a, dir := archEncFixture(t, s)
			if err := a.Flush(); err != nil {
				t.Fatal(err)
			}
			found := archEncDiskSecrets(t, a, dir)
			legacy := 0
			for i := range found {
				if !s.legacy || i >= archEncN/2 {
					t.Fatalf("record %d appended with the key has a secret on disk in the clear", i)
				}
				legacy++
			}
			// Interning and retraining re-encode every record of the segments they rewrite, under the key,
			// so they seal the legacy values as they go. Columnarizing does not: it moves a record's
			// values into columns and a remnant without decoding them, and a sealed attribute is never a
			// column, so its clear value lands in the remnant as it was. Those legacy values are what the
			// redacted reads must withhold by name.
			resealed := s.interned || s.dict
			switch {
			case s.legacy && !resealed && legacy == 0:
				t.Fatal("fixture: the records written before the key are not in the clear; the legacy shape tests nothing")
			case s.legacy && resealed && legacy != 0:
				t.Fatalf("%d legacy records still in the clear after their segments were re-encoded with the key", legacy)
			}
			// The entitled read still gets every value.
			n := 0
			for ad := range a.Query(mustQuery(t, `ClusterId >= 0`)) {
				id, _ := ad.EvaluateAttrInt("ClusterId")
				if v, _ := ad.EvaluateAttrString("Salary"); v != archEncSecret(archEncSalary, int(id)) {
					t.Fatalf("record %d: Salary = %q", id, v)
				}
				if v, _ := ad.EvaluateAttrString("ClaimId"); v != archEncSecret(archEncClaim, int(id)) {
					t.Fatalf("record %d: ClaimId = %q", id, v)
				}
				n++
			}
			if n != archEncN {
				t.Fatalf("entitled read returned %d records, want %d", n, archEncN)
			}
			if !s.legacy {
				return
			}
			// A rewrite decodes and re-encodes every record under the current key and policy.
			if a.Rewrite() == 0 {
				t.Fatal("Rewrite rewrote nothing")
			}
			if err := a.Flush(); err != nil {
				t.Fatal(err)
			}
			if left := archEncDiskSecrets(t, a, dir); len(left) != 0 {
				t.Fatalf("%d legacy records still hold a secret in the clear after Rewrite", len(left))
			}
		})
	}
}

// archEncCountKey wraps the archive's sealer to count opens, so a redacted read can be checked for
// never using the key.
func archEncCountKey(a *Archive) *countingSealer {
	cs := &countingSealer{inner: a.c.sealer}
	a.c.sealer = cs
	return cs
}

// archEncWithheld is the oracle's view of a record for a reader not entitled to sealed values: the
// record with every attribute the policy seals read as undefined.
func archEncWithheld(i int) *classad.ClassAd {
	ad, _ := classad.Parse(archEncAd(i))
	for _, n := range []string{"Salary", "ClaimId", "SecretNum"} {
		ad.InsertExpr(n, mustExpr("undefined"))
	}
	return ad
}

func mustExpr(s string) *classad.Expr {
	e, err := classad.ParseExpr(s)
	if err != nil {
		panic(err)
	}
	return e
}

func noSecret(t *testing.T, path string, b []byte) {
	t.Helper()
	if bytes.Contains(b, []byte(archEncSalary)) || bytes.Contains(b, []byte(archEncClaim)) {
		t.Fatalf("%s: a redacted read emitted a secret: %.200q", path, b)
	}
	if bytes.Contains(b, []byte("SecretNum = 7")) || bytes.Contains(b, []byte("SecretNum=7")) {
		t.Fatalf("%s: a redacted read emitted a sealed number: %.200q", path, b)
	}
}

// archEncConstraints are constraints on sealed attributes with the number of records each matches for
// an entitled reader (priv) and for one reading with no key (keyl), to whom every sealed value is
// undefined.
var archEncConstraints = []struct {
	q          string
	priv, keyl int
}{
	{`Salary == "` + archEncSecret(archEncSalary, 3) + `"`, 1, 0},
	{`Salary is undefined`, 0, archEncN},
	{`SecretNum > 7300`, archEncN - 301, 0},
	{`!(SecretNum <= 7300)`, archEncN - 301, 0},
	{`SecretNum is undefined`, 0, archEncN},
	{`ClaimId =!= undefined`, archEncN, 0},
	{`regexp("^SALARY", Salary)`, archEncN, 0},
	{`size(Salary) > 0 || Owner == "u1"`, archEncN, (archEncN + 5) / 7},
}

// TestArchiveKeylessReads: every archive read a caller not entitled to sealed values uses answers as
// if every sealed attribute were undefined -- in every record, including those written before the
// archive had a key -- emits no secret, and never opens the key. The entitled reads of the same archive
// see the plaintext, so the withholding is the redaction and not a broken store.
func TestArchiveKeylessReads(t *testing.T) {
	t.Parallel()
	for _, s := range archEncShapes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			a, _ := archEncFixture(t, s)
			cs := archEncCountKey(a)

			// Full ads, against the oracle: every attribute equal, the sealed ones undefined.
			got := 0
			for ad := range a.QueryLimitRedacted(mustQuery(t, `ClusterId >= 0`), 0) {
				noSecret(t, "QueryLimitRedacted", []byte(ad.MarshalOld()))
				id, _ := ad.EvaluateAttrInt("ClusterId")
				want := archEncWithheld(int(id))
				for _, n := range want.GetAttributes() {
					if g, w := ad.EvaluateAttr(n).String(), want.EvaluateAttr(n).String(); g != w {
						t.Fatalf("record %d: %s = %s, want %s", id, n, g, w)
					}
				}
				got++
			}
			if got != archEncN {
				t.Fatalf("QueryLimitRedacted returned %d records, want %d", got, archEncN)
			}

			// Constraints on a sealed attribute: undefined for every record, whatever its stored form.
			for _, c := range archEncConstraints {
				q := mustQuery(t, c.q)
				if n := countSeq(a.QueryLimitRedacted(q, 0)); n != c.keyl {
					t.Errorf("redacted %s matched %d, want %d", c.q, n, c.keyl)
				}
				if n, ok := a.CountConstraintRedacted(c.q); ok && n != c.keyl {
					t.Errorf("redacted count %s = %d, want %d", c.q, n, c.keyl)
				}
				if n := countSeq(a.QueryProjectStatsRedacted(q, []string{"Owner"}, nil)); n != c.keyl {
					t.Errorf("redacted projection %s matched %d, want %d", c.q, n, c.keyl)
				}
				if n := countSeq(a.QueryRawProjected(q, []string{"Owner"}, false, true)); n != c.keyl {
					t.Errorf("redacted raw projection %s matched %d, want %d", c.q, n, c.keyl)
				}
			}

			// Projected values.
			for vals := range a.QueryProjectStatsRedacted(mustQuery(t, `true`), []string{"Salary", "SecretNum", "ClaimId", "Owner"}, nil) {
				for i := 0; i < 3; i++ {
					if !vals[i].IsUndefined() {
						t.Fatalf("redacted projection read sealed value %s", vals[i].String())
					}
				}
			}
			// Raw projections, named and whole-ad, with and without reference chasing.
			for _, proj := range [][]string{{"Salary", "ClaimId", "SecretNum", "Owner"}, nil} {
				for _, refs := range []bool{false, true} {
					n := 0
					for ra := range a.QueryRawProjected(mustQuery(t, `true`), proj, refs, true) {
						noSecret(t, "QueryRawProjected", rawAdBytes(ra))
						n++
					}
					if n != archEncN {
						t.Fatalf("redacted raw projection %v returned %d rows", proj, n)
					}
				}
			}
			// Aggregates and top-K inputs over a sealed attribute: declined, or undefined throughout.
			if st, ok := a.NumStatsQueryRedacted(nil, "SecretNum"); ok && st.N != 0 {
				t.Errorf("redacted NumStats over SecretNum aggregated %d values", st.N)
			}
			if _, _, ok := a.TopKOrderThresholdRedacted(nil, "SecretNum", true, 5, nil); ok {
				t.Error("redacted top-K cutoff ordered by a sealed attribute")
			}
			if gs, ok := a.GroupStatsAllAggsRedacted([]GroupSpec{{Attr: "SecretNum"}}, nil, nil); ok && len(gs) > 1 {
				t.Fatalf("redacted GROUP BY SecretNum produced %d groups", len(gs))
			}
			if gs, ok := a.GroupStatsConstraintAggsRedacted(`SecretNum > 0`, []GroupSpec{{Attr: "Owner"}}, nil, nil); ok {
				for _, g := range gs {
					if g.Count != 0 {
						t.Fatalf("redacted grouped count over a sealed constraint counted %d", g.Count)
					}
				}
			}
			// The change stream.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			seq, err := a.WatchRedacted(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for ev := range seq {
				if ev.Kind == WatchSynced {
					break
				}
				if ev.Ad != nil {
					noSecret(t, "WatchRedacted", []byte(ev.Ad.MarshalOld()))
					n++
				}
			}
			cancel()
			if n != archEncN {
				t.Fatalf("redacted watch replayed %d records, want %d", n, archEncN)
			}

			if opens := cs.opens.Load(); opens != 0 {
				t.Fatalf("redacted reads opened the key %d times", opens)
			}
			// Premise: the entitled reads see the plaintext through the same archive.
			for _, c := range archEncConstraints {
				if n := countSeq(a.QueryLimit(mustQuery(t, c.q), 0)); n != c.priv {
					t.Errorf("entitled %s matched %d, want %d", c.q, n, c.priv)
				}
			}
			if cs.opens.Load() == 0 {
				t.Fatal("entitled reads opened no sealed value; the counter is not wired")
			}
		})
	}
}

// archEncLegacyColumnar is an archive written entirely before it had a key and columnarized then, so
// every record, and every column built from them, holds the sealed attributes' values in the clear --
// SecretNum a numeric column the column tiers answer from -- reopened with the key.
func archEncLegacyColumnar(t *testing.T) *Archive {
	t.Helper()
	dir := t.TempDir()
	a, err := OpenArchive(archEncOpts(dir, false))
	if err != nil {
		t.Fatal(err)
	}
	archEncAppend(t, a, 0, archEncN)
	if !a.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	a.c.ColumnarizeSealed()
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if a, err = OpenArchive(archEncOpts(dir, true)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	// Premise: the column tiers do answer from the clear SecretNum column for an entitled reader.
	if st, ok := a.c.NumStatsQuery(nil, "SecretNum"); !ok || st.N != archEncN {
		t.Fatalf("fixture: the columns do not serve SecretNum (N=%d ok=%v); the column checks test nothing", st.N, ok)
	}
	return a
}

// TestArchiveKeylessColumnTiers: the column tiers never answer a reader not entitled to sealed values
// from a column of a sealed attribute's clear values. Each either declines -- the keyless scan then
// reads the attribute as undefined -- or answers as if every value were undefined.
func TestArchiveKeylessColumnTiers(t *testing.T) {
	t.Parallel()
	a := archEncLegacyColumnar(t)
	if st, ok := a.NumStatsQueryRedacted(nil, "SecretNum"); ok && st.N != 0 {
		t.Errorf("redacted MIN/MAX/SUM over SecretNum read %d values from the columns", st.N)
	}
	if st, ok := a.NumStatsQueryRedacted(mustQuery(t, `SecretNum > 7300`), "RequestMemory"); ok && st.N != 0 {
		t.Errorf("redacted aggregate under a sealed constraint read %d values", st.N)
	}
	if _, seen, ok := a.TopKOrderThresholdRedacted(nil, "SecretNum", true, 5, nil); ok {
		t.Errorf("redacted top-K cutoff ordered %d records by a sealed attribute", seen)
	}
	if _, seen, ok := a.TopKOrderThresholdRedacted(mustQuery(t, `SecretNum > 7300`), "RequestMemory", true, 5, nil); ok && seen != 0 {
		t.Errorf("redacted top-K cutoff under a sealed constraint saw %d records", seen)
	}
	if n, ok := a.CountConstraintRedacted(`SecretNum > 7300`); ok && n != 0 {
		t.Errorf("redacted count on a sealed attribute = %d", n)
	}
	groupBys := []struct {
		name    string
		groups  []GroupSpec
		aggs    []GroupAggCol
		filters []string
	}{
		{"group by sealed", []GroupSpec{{Attr: "SecretNum"}}, nil, nil},
		{"aggregate sealed", []GroupSpec{{Attr: "Owner"}}, []GroupAggCol{{Attr: "SecretNum"}}, nil},
		{"filter on sealed", []GroupSpec{{Attr: "Owner"}}, []GroupAggCol{{Attr: "RequestMemory", Filter: `SecretNum > 7300`}}, nil},
		{"count filter on sealed", []GroupSpec{{Attr: "Owner"}}, nil, []string{`SecretNum > 7300`}},
	}
	for _, g := range groupBys {
		if gs, ok := a.GroupStatsAllAggsRedacted(g.groups, g.aggs, g.filters); ok {
			t.Errorf("%s: redacted match-all GROUP BY answered from the columns (%d groups)", g.name, len(gs))
		}
		if gs, ok := a.GroupStatsConstraintAggsRedacted(`RequestMemory > 0`, g.groups, g.aggs, g.filters); ok {
			t.Errorf("%s: redacted GROUP BY answered from the columns (%d groups)", g.name, len(gs))
		}
		// Premise: the same request is served from the columns for an entitled reader.
		if _, ok := a.GroupStatsAllAggs(g.groups, g.aggs, g.filters); !ok && g.filters == nil && g.aggs != nil && g.aggs[0].Filter == "" {
			t.Errorf("%s: fixture: not served from the columns even for an entitled reader", g.name)
		}
	}
	if gs, ok := a.GroupStatsConstraintAggsRedacted(`SecretNum > 7300`, []GroupSpec{{Attr: "Owner"}}, nil, nil); ok {
		for _, g := range gs {
			if g.Count != 0 {
				t.Errorf("redacted grouped count under a sealed constraint counted %d", g.Count)
			}
		}
	}
	// The scans the declines fall back to read the attribute as undefined.
	if n := countSeq(a.QueryLimitRedacted(mustQuery(t, `SecretNum > 7300`), 0)); n != 0 {
		t.Errorf("redacted scan matched %d records on a sealed attribute", n)
	}
	for vals := range a.QueryProjectStatsRedacted(mustQuery(t, `true`), []string{"SecretNum"}, nil) {
		if !vals[0].IsUndefined() {
			t.Fatalf("redacted projection read SecretNum = %s", vals[0].String())
		}
	}
}

// TestArchiveDerivedStructuresExcludeSealed: neither a zone map nor a column is built for an attribute
// the policy seals, even from records that hold its values in the clear -- those are exactly what such
// a structure would be built from.
func TestArchiveDerivedStructuresExcludeSealed(t *testing.T) {
	t.Parallel()
	a := archEncLegacyColumnar(t)
	id, ok := a.c.intern.LookupID("SecretNum")
	if !ok {
		t.Fatal("SecretNum never interned")
	}
	cid, _ := a.c.intern.LookupID("ClusterId")
	zoned := 0
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		act := sh.act
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act {
				continue
			}
			a.c.rezoneSealed(sh, seg)
			if _, has := seg.zones[id]; has {
				t.Fatalf("segment %d keeps a zone for a sealed attribute", seg.id)
			}
			if _, has := seg.zones[cid]; has {
				zoned++
			}
		}
	}
	if zoned == 0 {
		t.Fatal("fixture: no segment zoned at all; the check tests nothing")
	}
	// Every sample the derivation reads holds SecretNum as a plain number in every record.
	if !a.ReschemaScan(2000, 8) {
		t.Fatal("no schema re-derived")
	}
	st := a.c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema")
	}
	if st.schema.hasField(id) {
		t.Fatal("the columnar schema made a sealed attribute a field")
	}
	if rid, _ := a.c.intern.LookupID("RequestMemory"); !st.schema.hasField(rid) {
		t.Fatal("fixture: RequestMemory is not a field either; the schema check tests nothing")
	}
	// Nor a secondary (group) schema, which takes what the base schema leaves out: here every sealed
	// attribute is present in every sampled record, the shape a group is made of.
	info := a.GroupSchemas(2000, 8)
	for _, g := range info.Groups {
		for _, n := range g.Attrs {
			if a.c.shouldEncrypt(n) {
				t.Fatalf("a group schema holds the sealed attribute %s: %v", n, g.Attrs)
			}
		}
	}
}

// TestArchiveSealedRefusesIndex: an attribute cannot be both sealed and indexed on an archive, as on a
// mutable table: sealing an indexed one is refused, and indexing a sealed one does not take.
func TestArchiveSealedRefusesIndex(t *testing.T) {
	t.Parallel()
	a, err := OpenArchive(archEncOpts(t.TempDir(), true))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.SetEncryptedAttrs([]string{"Owner"}); err == nil {
		t.Fatal("sealing an indexed attribute was accepted")
	}
	a.AddIndex([]string{"Salary"}, []string{"SecretNum"})
	cat, val := a.IndexedAttrs()
	for _, n := range append(cat, val...) {
		if strings.EqualFold(n, "Salary") || strings.EqualFold(n, "SecretNum") {
			t.Fatalf("a sealed attribute was indexed: %v %v", cat, val)
		}
	}
}

// TestArchiveSetEncryptedAttrsAppliesToReads: a runtime change to the explicit set governs the next
// appends and, at once, what a reader not entitled to sealed values sees of every record.
func TestArchiveSetEncryptedAttrsAppliesToReads(t *testing.T) {
	t.Parallel()
	a, dir := archEncFixture(t, archEncShape{})
	if err := a.SetEncryptedAttrs([]string{"Salary", "SecretNum", "Owner"}); err == nil {
		t.Fatal("Owner is indexed; sealing it must be refused")
	}
	a.DropIndex("Owner")
	if err := a.SetEncryptedAttrs([]string{"Salary", "SecretNum", "Owner"}); err != nil {
		t.Fatal(err)
	}
	if n := countSeq(a.QueryLimitRedacted(mustQuery(t, `Owner == "u1"`), 0)); n != 0 {
		t.Fatalf("a newly sealed attribute still matched %d records for a keyless reader", n)
	}
	if n := countSeq(a.QueryLimit(mustQuery(t, `Owner == "u1"`), 0)); n != archEncN/7+1 && n != archEncN/7 {
		t.Fatalf("entitled read matched %d", n)
	}
	for ad := range a.QueryLimitRedacted(mustQuery(t, `true`), 0) {
		if v := ad.EvaluateAttr("Owner"); !v.IsUndefined() {
			t.Fatalf("Owner = %s for a keyless reader after it was sealed", v.String())
		}
	}
	const later = "OWNERSECRET"
	if err := a.Append(mustAd(t, `[ ClusterId = 99999; Owner = "`+later+`" ]`)); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	var all []byte
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			all = append(all, b...)
		}
		return nil
	})
	if bytes.Contains(all, []byte(later)) {
		t.Fatal("an attribute sealed at runtime was appended in the clear")
	}
}

// TestArchiveRollbackWithoutKey is what a build that gives archives no key does with one that has
// sealed values: it opens, never shows a sealed value as plaintext, does not crash, and loses nothing
// -- its maintenance cannot decode a sealed record and so leaves the segment as it was -- and the next
// open with the key reads every record back.
func TestArchiveRollbackWithoutKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := OpenArchive(archEncOpts(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	archEncAppend(t, a, 0, archEncN)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	old, err := OpenArchive(archEncOpts(dir, false))
	if err != nil {
		t.Fatalf("an archive with sealed values does not open without the key: %v", err)
	}
	for ad := range old.Query(mustQuery(t, `true`)) {
		noSecret(t, "keyless Query", []byte(ad.MarshalOld()))
	}
	for ra := range old.QueryRawProjected(mustQuery(t, `true`), nil, false, false) {
		noSecret(t, "keyless QueryRawProjected", rawAdBytes(ra))
	}
	for vals := range old.QueryProject(mustQuery(t, `true`), []string{"Salary", "ClaimId"}) {
		for _, v := range vals {
			if s, ok := v.StringValue(); ok == nil && strings.Contains(s, "SECRET") {
				t.Fatalf("keyless projection read %q", s)
			}
		}
	}
	// Every maintenance pass an old build runs on its own.
	old.Rewrite()
	if _, err = old.RetrainDict(2000); err != nil {
		t.Logf("RetrainDict without the key: %v", err)
	}
	old.c.InternSealed()
	old.MergePass(MergeOptions{TargetSegments: 2, TriggerSegments: 2, MinMergeBytes: 1, KeepRecent: 1})
	if old.BuildAndEnableSchemaScan(2000, 8) {
		old.c.ColumnarizeSealed()
	}
	if got := old.Count(); got != archEncN {
		t.Fatalf("the keyless open counts %d records, want %d", got, archEncN)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = OpenArchive(archEncOpts(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	seen := map[int64]bool{}
	for ad := range a.Query(mustQuery(t, `true`)) {
		id, _ := ad.EvaluateAttrInt("ClusterId")
		if v, _ := ad.EvaluateAttrString("Salary"); v != archEncSecret(archEncSalary, int(id)) {
			t.Fatalf("record %d after rollback and return: Salary = %q", id, v)
		}
		seen[id] = true
	}
	if len(seen) != archEncN {
		t.Fatalf("after rollback and return %d records read back, want %d", len(seen), archEncN)
	}
}
