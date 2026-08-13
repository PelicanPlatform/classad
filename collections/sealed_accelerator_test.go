package collections

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Encryption at rest and the columnar accelerator used to be mutually exclusive: enabling the accelerator
// returned false outright for an encrypted collection. That made "always seal private attributes"
// unaffordable -- it would have disabled the accelerator for every deployment, taking the constrained
// count, the vectorized scan and the grouped aggregates with it.
//
// They are exclusive only because recordToInternedDict, which canonicalizes records for BOTH the sample
// set and the block build, decoded with the collection's key and re-encoded WITHOUT sealing. A sealed
// value therefore reappeared as a plaintext string literal: it became a schema FIELD, and its plaintext
// was written into the .idx sidecar. With the re-seal in place a sealed value is not a literal, so it is
// never a field and never a column -- it stays in the cold tail sealed.
//
// The cost is per attribute, which is the intended trade: private attributes are excluded from the
// columns and so are slow; everything else keeps the accelerator.

// diskBytesContaining reports the files under dir whose bytes contain needle.
func diskBytesContaining(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil
		}
		if bytes.Contains(b, []byte(needle)) {
			hits = append(hits, d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestAcceleratorOverEncryptedStoresNoPlaintext(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"
	c, err := Open(Options{
		Shards: 1, Dir: dir, SegmentSize: 1 << 16,
		DataKey: dataKey, EncryptedAttrs: []string{"ClaimId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; JobStatus=%d; ClaimId=%q]`,
				i, 1+i%5, secret))); err != nil {
			t.Fatal(err)
		}
	}

	// The accelerator must now enable over an encrypted collection.
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Fatal("the accelerator did not enable over an encrypted collection; encryption would then " +
			"cost every deployment its columnar reads")
	}
	info := c.SchemaScanInfo()
	var fields []string
	for _, f := range info.Schema {
		fields = append(fields, f.Name+":"+f.Kind)
		if strings.EqualFold(f.Name, "ClaimId") {
			t.Errorf("a SEALED attribute became a schema field (%s): its value would be materialized "+
				"into a column in the clear", f.Name+":"+f.Kind)
		}
	}
	t.Logf("schema fields over an encrypted collection: %v", fields)
	if len(fields) == 0 {
		t.Fatal("no schema fields at all: the accelerator is enabled but empty, so this test would " +
			"pass without proving anything")
	}

	// And it must still answer, so the coexistence is real rather than nominal.
	q, err := vm.Parse("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	n, served := c.CountQuery(q)
	if !served {
		t.Error("the columnar count declined over an encrypted collection")
	}
	if n != 3000 {
		t.Errorf("columnar count = %d, want 3000", n)
	}
	c.Close()

	// The decisive check: no plaintext anywhere on disk, sidecars included.
	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("the secret appears in plaintext on disk, in: %v", hits)
	}
}

// TestEncryptedSamplesStaySealed pins the specific defect: the canonicalizer must not hand plaintext to
// the sample set, because that is what put a sealed attribute into the schema.
func TestEncryptedSamplesStaySealed(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"
	c, err := Open(Options{
		Shards: 1, Dir: dir, SegmentSize: 1 << 16,
		DataKey: dataKey, EncryptedAttrs: []string{"ClaimId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 500; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	samples := c.CollectSamplesRecentN(64)
	if len(samples) == 0 {
		t.Skip("no sealed segments sampled yet")
	}
	canon := 0
	for _, w := range samples {
		iw, ok := c.recordToInterned(nil, w)
		if !ok {
			continue
		}
		canon++
		if bytes.Contains(iw, []byte(secret)) {
			t.Fatal("recordToInterned produced a record carrying the secret in the clear; the schema " +
				"and every columnar block built from it would carry the plaintext too")
		}
	}
	if canon == 0 {
		t.Fatal("no sample canonicalized, so nothing was checked")
	}
	t.Logf("%d canonicalized samples, none carrying plaintext", canon)
}

// TestStreamingIngestSealsPerAd covers the ingest path. PutOld refused outright on an encrypted
// collection, so turning sealing on everywhere would have pushed every ingest onto the parse+AST path.
// Now the streaming encoder is used for ads with nothing to seal and deferred per ad for the rest -- and
// either way the stored bytes must carry no plaintext.
func TestStreamingIngestSealsPerAd(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"
	c, err := Open(Options{
		Shards: 1, Dir: dir, SegmentSize: 1 << 16,
		DataKey: dataKey, EncryptedAttrs: []string{"ClaimId"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// An ad with nothing to seal keeps the streaming fast path.
	tx := c.Begin()
	if !tx.PutOld([]byte("plain"), "Cpus = 4\nOwner = \"alice\"") {
		t.Error("an ad with no sealed attribute was refused the streaming path; encryption would then " +
			"cost every ingest")
	}
	// An ad WITH something to seal is still accepted -- deferred to the sealing path, not refused.
	if !tx.PutOld([]byte("secret"), "Cpus = 8\nClaimId = \""+secret+"\"") {
		t.Error("an ad with a sealed attribute was refused; the caller would have to parse it itself")
	}
	if res := tx.Commit(); len(res.Conflicts) != 0 {
		t.Fatalf("commit conflicts: %+v", res.Conflicts)
	}

	// Both readable, and the secret is sealed rather than stored in the clear.
	ad, ok := c.Get([]byte("secret"))
	if !ok {
		t.Fatal("the deferred ad was not stored")
	}
	if !strings.Contains(ad.StringWithPrivate(), secret) {
		t.Error("the deferred ad lost its value")
	}
	if _, ok := c.Get([]byte("plain")); !ok {
		t.Fatal("the streamed ad was not stored")
	}
	c.Close()

	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("a streaming ingest stored the secret in the clear, in: %v", hits)
	}
}

// TestParallelWireScanOverEncrypted covers the guard that excluded encrypted collections from parallel
// wire scans, on the grounds that "the Sealer contract is single-goroutine per pass". That is not the
// stated contract and not what the code does -- the parallel QUERY path already opens sealed values from
// its workers. Run under -race in CI, this is also the check that the claim was wrong.
func TestParallelWireScanOverEncrypted(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"
	c, err := Open(Options{
		Shards: 4, Dir: dir, SegmentSize: 1 << 16,
		DataKey: dataKey, EncryptedAttrs: []string{"ClaimId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const n = 4000
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	if !c.SupportsRawWire() {
		t.Skip("raw wire unsupported here")
	}
	// Every ad must arrive exactly once, whichever path ran, and no row may carry the secret in the
	// clear -- a parallel worker opening a sealed value must not write plaintext into the shared buffer.
	rows := 0
	for w := range c.ScanRawWire([]string{"Owner", "Cpus"}, false) {
		rows++
		if bytes.Contains(w, []byte(secret)) {
			t.Fatalf("a raw-wire row carried the secret: %q", w)
		}
	}
	if rows != n {
		t.Errorf("raw wire scan yielded %d rows, want %d", rows, n)
	}
	t.Logf("raw wire scan over an encrypted collection: %d rows", rows)
}
