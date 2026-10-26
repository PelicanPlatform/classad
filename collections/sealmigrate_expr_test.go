package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The migration's candidate test has to recognise a SEALED value, not a non-literal one. The encoder
// seals whatever an encrypted attribute holds -- an expression as much as a literal -- so a store
// written before sealing was on holds plaintext expressions too, and an expression is not a literal.
// Treating "not a literal" as "sealed" reported those records clean and left them in the clear.

// sealShape is one way an encrypted attribute's plaintext can be stored. needle is a byte string that
// appears on disk exactly when the value is stored in the clear; "" for a shape with no distinctive
// bytes (a number), which the redacted-read check covers instead.
type sealShape struct {
	name    string
	expr    func(t *testing.T) ast.Expr
	needle  string
	literal bool // stored as a literal node, which even the old detection recognised
}

func parsedExpr(src string) func(t *testing.T) ast.Expr {
	return func(t *testing.T) ast.Expr {
		t.Helper()
		for _, a := range mustAd(t, "[X = "+src+"]").AST().Attributes {
			if a.Name == "X" {
				return a.Value
			}
		}
		t.Fatalf("parse %q: no X", src)
		return nil
	}
}

var sealShapes = []sealShape{
	{"string literal", parsedExpr(`"SealLeakLiteral-a1"`), "SealLeakLiteral-a1", true},
	{"string with quotes", parsedExpr(`"SealLeak \"Quoted\" b2"`), `SealLeak "Quoted" b2`, true},
	{"arithmetic", parsedExpr(`strcat("SealLeakArith-c3", Owner) + 7`), "SealLeakArith-c3", false},
	{"conditional", parsedExpr(`Owner == "SealLeakCond-d4" ? 1 : 2`), "SealLeakCond-d4", false},
	{"list", parsedExpr(`{ "SealLeakList-e5", 7, Cpus }`), "SealLeakList-e5", false},
	{"nested ad", parsedExpr(`[ Inner = "SealLeakNested-f6"; N = 3 ]`), "SealLeakNested-f6", false},
	// No needle: the only distinctive bytes are the referenced NAME, and an interned segment keeps every
	// name its records use -- including those inside a sealed value -- in its dictionary in the clear.
	{"reference", parsedExpr(`MY.SealLeakRefG7`), "", false},
	// Unary minus over a literal, which is how older builds stored every negative number. The encoder
	// now folds `-N` into a negative literal, but not the outer minus of `-(-N)`, whose stored form is
	// therefore that same node: nUnOp over an nInt.
	{"unary minus", func(*testing.T) ast.Expr {
		return &ast.UnaryOp{Op: "-", Expr: &ast.UnaryOp{Op: "-", Expr: &ast.IntegerLiteral{Value: 987654321}}}
	}, "", false},
	{"real", parsedExpr(`3.25`), "", true},
}

// sealFixtureAd builds record i. SecretLit is always a string literal, so it is the attribute the
// columnar schema picks up. With secrets, Secret rotates through shapes, and ClaimId -- a private
// attribute, sealed whatever the configuration -- holds an expression.
func sealFixtureAd(t *testing.T, i, cpus int, secrets bool, shapes []sealShape) *classad.ClassAd {
	t.Helper()
	ad := mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; JobStatus=%d; SecretLit="SealLeakColumn-h8-%d"]`,
		cpus, 1+i%5, i%3))
	if secrets {
		ad.Insert("ClaimId", parsedExpr(`strcat("SealLeakClaim-i9", "-", Owner)`)(t))
		ad.Insert("Secret", shapes[i%len(shapes)].expr(t))
	}
	return ad
}

// TestMigrateSealedAttrsSealsLegacyValues writes a store with no key -- every value in the clear, with
// what a production job table accumulates: updated keys, delta records, a trained dictionary, interned
// and columnar-native segments -- then reopens it with encryption on for those attributes and migrates.
//
// The oracle is two-sided. Every byte the store persists is searched for each value's plaintext, and
// every record is read back twice: with the key it must return what was written, and without it the
// encrypted attributes must come back undefined. The second catches a value stored in the clear with
// no distinctive bytes to search for (a number, a reference).
//
// Each case isolates one place plaintext can hide, since a pass that rewrites a segment for one reason
// seals everything in it: a literal anywhere in a segment would hide a detection that misses
// expressions, and a trained dictionary makes every segment a candidate.
func TestMigrateSealedAttrsSealsLegacyValues(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	var exprShapes []sealShape
	for _, s := range sealShapes {
		if !s.literal {
			exprShapes = append(exprShapes, s)
		}
	}
	needlesOf := func(shapes []sealShape, extra ...string) []string {
		out := append([]string{"SealLeakClaim-i9"}, extra...)
		for _, s := range shapes {
			if s.needle != "" {
				out = append(out, s.needle)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		encrypted []string
		shapes    []sealShape
		deltaMax  int
		// steps run in order over the keyless store: "put+" writes a version of every key with
		// Secret and ClaimId, "put-" one without; "patch" changes JobStatus by a patch (a delta
		// record when deltaMax is set); "retrain" trains a dictionary; "columnar" columnarizes
		// the sealed segments; "rewrite" re-encodes every live ad and force-compacts; "touch" writes
		// every third key again unchanged, so the segments before it hold superseded and current
		// versions side by side.
		steps   []string
		needles []string
	}{{
		// Only expression-valued encrypted attributes, and no delta records, so superseded whole
		// versions sit in the segments rewritten.
		name:      "expressions in records",
		encrypted: []string{"Secret"},
		shapes:    exprShapes,
		steps:     []string{"put+", "put+", "patch", "touch"},
		needles:   needlesOf(exprShapes),
	}, {
		// The only encrypted value lives in the columnar payload, not in any record.
		name:      "columnar payload",
		encrypted: []string{"SecretLit"},
		deltaMax:  4,
		steps:     []string{"put-", "put-", "columnar", "patch"},
		needles:   []string{"SealLeakColumn-h8"},
	}, {
		name:      "dictionary trained on plaintext",
		encrypted: []string{"Secret", "SecretLit"},
		shapes:    sealShapes,
		deltaMax:  4,
		steps:     []string{"put+", "put+", "retrain", "columnar", "put+", "patch"},
		needles:   needlesOf(sealShapes, "SealLeakColumn-h8"),
	}, {
		// No record holds a secret any more; the dictionary trained while they did is the only copy.
		name:      "dictionary is the only copy",
		encrypted: []string{"Secret"},
		shapes:    sealShapes,
		deltaMax:  4,
		steps:     []string{"put+", "put+", "retrain", "put-", "rewrite", "patch"},
		needles:   needlesOf(sealShapes),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const n = 600
			// The identity base codec, so a byte search can see a record's plaintext: ZSTD
			// entropy-codes its literals. A trained dictionary is searchable either way -- its
			// content is raw sample bytes.
			base := Options{
				Shards: 2, Dir: dir, SegmentSize: 1 << 15, DeltaMax: tc.deltaMax,
				SchemaRequireAttrs: []string{"SecretLit"},
			}
			c, err := Open(base) // no DataKey: nothing seals
			if err != nil {
				t.Fatal(err)
			}
			round := -1
			for _, step := range tc.steps {
				switch step {
				case "put+", "put-":
					round++
					for i := 0; i < n; i++ {
						ad := sealFixtureAd(t, i, 100*round+i, step == "put+", tc.shapes)
						if err = c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
							t.Fatal(err)
						}
					}
				case "patch":
					for i := 0; i < n; i++ {
						p := classad.New()
						p.InsertAttr("JobStatus", 7)
						tx := c.Begin()
						tx.PatchAttrs([]byte(fmt.Sprintf("k%d", i)), p, nil)
						if r := tx.Commit(); r.Conflicted() {
							t.Fatal("patch conflicted")
						}
					}
					if d, _ := c.DeltaStats(); tc.deltaMax > 0 && d == 0 {
						t.Fatal("no delta records were written, so the store is not the shape under test")
					}
				case "retrain":
					if _, err = c.RetrainDict(512); err != nil {
						t.Fatal(err)
					}
				case "columnar":
					c.BuildAndEnableSchemaScan(512, 4) // columnarizes the sealed segments too
					c.ColumnarizeSealed()
					if countColumnarized(c) == 0 {
						t.Fatal("no segment was columnarized, so the store is not the shape under test")
					}
				case "rewrite":
					c.Rewrite()
				case "touch":
					for i := 0; i < n; i += 3 {
						k := []byte(fmt.Sprintf("k%d", i))
						ad, ok := c.Get(k)
						if !ok {
							t.Fatalf("touch: %s missing", k)
						}
						if err = c.Put(k, ad); err != nil {
							t.Fatal(err)
						}
					}
				default:
					t.Fatalf("unknown step %q", step)
				}
			}
			want := make(map[string]*classad.ClassAd, n)
			for i := 0; i < n; i++ {
				k := fmt.Sprintf("k%d", i)
				ad, ok := c.Get([]byte(k))
				if !ok {
					t.Fatalf("legacy Get(%s): missing", k)
				}
				if cpus, _ := ad.EvaluateAttrInt("Cpus"); cpus != int64(100*round+i) {
					t.Fatalf("legacy Get(%s).Cpus = %d, want %d", k, cpus, 100*round+i)
				}
				if st, _ := ad.EvaluateAttrInt("JobStatus"); st != 7 {
					t.Fatalf("legacy Get(%s).JobStatus = %d, want the patched 7", k, st)
				}
				want[k] = ad
			}
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			for _, needle := range tc.needles {
				if len(diskBytesContaining(t, dir, needle)) == 0 {
					t.Fatalf("the legacy fixture has no plaintext %q on disk, so this test proves nothing about it", needle)
				}
			}

			_, dataKey := deriveDataKey(t)
			enc := base
			enc.DataKey = dataKey
			enc.EncryptedAttrs = tc.encrypted
			c, err = Open(enc)
			if err != nil {
				t.Fatal(err)
			}
			if m := c.MigrateSealedAttrs(0); m == 0 {
				t.Error("migrated no segments, but the store holds plaintext encrypted attributes")
			}
			if !c.sealMigrationDone() {
				t.Error("the pass did not complete on a store with no writer racing it")
			}
			checkSealedReads(t, c, want, append([]string{"ClaimId"}, tc.encrypted...))
			// The maintenance a running table does after the migration must not bring plaintext back.
			c.BuildAndEnableSchemaScan(512, 4)
			c.ColumnarizeSealed()
			// Training can refuse samples this small that are mostly ciphertext ("0 literals found"),
			// which leaves the store as it was; that is not what is under test.
			if _, err = c.RetrainDict(512); err != nil {
				t.Logf("retrain after the migration: %v", err)
			}
			checkSealedReads(t, c, want, append([]string{"ClaimId"}, tc.encrypted...))
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			for _, needle := range tc.needles {
				if hits := diskBytesContaining(t, dir, needle); len(hits) != 0 {
					t.Errorf("plaintext %q survived the migration, in: %v", needle, hits)
				}
			}

			// And after a restart, which reads everything back from what the migration left on disk.
			c, err = Open(enc)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if again := c.MigrateSealedAttrs(0); again != 0 {
				t.Errorf("a second open migrated %d more segments; the first pass left unsealed values", again)
			}
			checkSealedReads(t, c, want, append([]string{"ClaimId"}, tc.encrypted...))
		})
	}
}

// checkSealedReads asserts every key reads back as written with the key, that a scan returns each key
// once, and that every attribute in sealed is undefined to a read without the key.
func checkSealedReads(t *testing.T, c *Collection, want map[string]*classad.ClassAd, sealed []string) {
	t.Helper()
	bad, inClear := 0, map[string]int{}
	for k, w := range want {
		got, ok := c.Get([]byte(k))
		if !ok {
			t.Fatalf("Get(%s): missing", k)
		}
		for _, attr := range []string{"Secret", "SecretLit", "ClaimId", "Owner", "Cpus", "JobStatus"} {
			if !sameAttr(w, got, attr) {
				if bad < 5 {
					ge, _ := got.Lookup(attr)
					we, _ := w.Lookup(attr)
					t.Errorf("Get(%s).%s = %v, want %v", k, attr, ge, we)
				}
				bad++
			}
		}
		red, ok := c.GetRedacted([]byte(k))
		if !ok {
			t.Fatalf("GetRedacted(%s): missing", k)
		}
		for _, attr := range sealed {
			if e, ok := red.Lookup(attr); ok && e.String() != "undefined" {
				inClear[attr+" = "+e.String()]++
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d attribute values read back wrong", bad)
	}
	for v, cnt := range inClear {
		t.Errorf("readable without the key, so stored in the clear: %s (%d records)", v, cnt)
	}
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for range c.Query(q) {
		rows++
	}
	if rows != len(want) {
		t.Errorf("a scan returned %d rows for %d keys: superseded versions came back as current", rows, len(want))
	}
}

// sameAttr reports whether attr is the same in both ads: the same expression, or -- because a rewrite
// re-encodes, and the encoder normalizes the spelling of a negated number -- the same number.
func sameAttr(want, got *classad.ClassAd, attr string) bool {
	we, wok := want.Lookup(attr)
	ge, gok := got.Lookup(attr)
	if !wok || !gok {
		return wok == gok
	}
	if we.String() == ge.String() {
		return true
	}
	wv, gv := want.EvaluateAttr(attr), got.EvaluateAttr(attr)
	if !wv.IsNumber() || !gv.IsNumber() {
		return false
	}
	return wv.String() == gv.String()
}

// TestMigrateSealedAttrsRerunsWhenThePolicyChanges covers the marker. A pass that ran when an attribute
// was not encrypted established nothing about that attribute, so adding it to the set must make the next
// open look again -- and a marker written by a build whose detection was wrong must not be trusted.
func TestMigrateSealedAttrsRerunsWhenThePolicyChanges(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	opts := Options{Shards: 1, Dir: dir, SegmentSize: 1 << 15, DataKey: dataKey}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if err = c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Cpus=%d; Note = strcat("SealLeakLater-j0", "-x")]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if m := c.MigrateSealedAttrs(0); m != 0 {
		t.Fatalf("migrated %d segments of a store with nothing to seal", m)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if !markerPresent(dir) {
		t.Fatal("no marker after a clean pass")
	}

	// Note becomes encrypted. Its existing values are plaintext expressions.
	opts.EncryptedAttrs = []string{"Note"}
	c, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if m := c.MigrateSealedAttrs(0); m == 0 {
		t.Error("the marker from a pass under the old policy suppressed the pass the new one needs")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if hits := diskBytesContaining(t, dir, "SealLeakLater-j0"); len(hits) != 0 {
		t.Errorf("plaintext of the newly encrypted attribute survived, in: %v", hits)
	}

	// A marker written by an earlier version of the pass is not this pass's marker.
	if err = os.WriteFile(filepath.Join(dir, sealMigrationMarker), []byte("sealed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.sealMigrationDone() {
		t.Error("a version-1 marker was accepted as proof this version's pass ran")
	}
}

func countColumnarized(c *Collection) int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s != nil && s.columnarized() {
				n++
			}
		}
		sh.mu.RUnlock()
	}
	return n
}

func markerPresent(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, sealMigrationMarker))
	return err == nil
}
