package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A redacted read must not hold the key. Before this, QueryRawRedacted stripped private attributes by
// NAME -- but wireToInline had already opened every sealed value with the collection's key and re-sealed
// it, so the secret was decrypted in this process and then filtered out afterwards. Filtering is a
// deny-list; not holding the key is not. These pin the difference.
func TestRedactedRawReadHoldsNoKey(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"

	openC := func() *Collection {
		c, err := Open(Options{
			Shards:         1,
			Dir:            dir,
			SegmentSize:    1 << 16,
			DataKey:        dataKey,
			EncryptedAttrs: []string{"ClaimId"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := openC()
	const n = 400
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	// Compact so the records live in INTERNED segments (that dict path is where the open-and-reseal
	// was), then reopen: a persistent collection's own active segments are inline-name, and the raw
	// reads yield nothing for those.
	c.Compact()
	c.Close()
	c = openC()
	defer c.Close()

	q, err := vm.Parse("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}

	// The premise: WITH the key the secret is recoverable, so the redacted read withholding it is the
	// change rather than the fixture never having had a secret. Established through Query (full decode);
	// TestPrivilegedRawReadRendersSealedValues covers the raw renderer separately.
	privileged, redacted := 0, 0
	sawSecret := false
	for ad := range c.Query(q) {
		privileged++
		if strings.Contains(ad.StringWithPrivate(), secret) {
			sawSecret = true
		}
	}
	if privileged == 0 {
		t.Fatal("privileged query returned nothing")
	}
	if !sawSecret {
		t.Fatal("the privileged read did not see the secret, so this test cannot show it being " +
			"withheld from the redacted one")
	}

	for ra := range c.QueryRawRedacted(q) {
		redacted++
		for _, e := range ra.Exprs {
			s := string(e)
			if strings.Contains(s, secret) {
				t.Fatalf("the secret leaked into a redacted raw read: %q", s)
			}
			// The ATTRIBUTE may appear (redaction is by name, and an unopened value is undefined);
			// what must never appear is the plaintext.
			if strings.HasPrefix(s, "ClaimId") && strings.Contains(s, "capability") {
				t.Fatalf("a redacted read rendered a ClaimId value: %q", s)
			}
		}
	}
	if redacted != privileged {
		t.Errorf("redacted raw read returned %d ads, privileged %d: redaction must not drop ADS, only "+
			"attribute values", redacted, privileged)
	}
	t.Logf("privileged=%d ads (secret visible), redacted=%d ads (secret withheld, no key held)",
		privileged, redacted)
}

// TestRedactedRawReadSurvivesWithoutTheCollectionKey is the property that matters for a session key: the
// decode path used by a redacted read works with no key at all, so a keyless session is a readable
// session rather than a broken one.
func TestRedactedRawReadSurvivesWithoutTheCollectionKey(t *testing.T) {
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
	for i := 0; i < 400; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	c.Compact()
	c.Close()

	// Reopen with NO key: this is what a session that holds none sees. The public attributes must
	// still read; before the redacting decode this failed every ad outright.
	c2, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	q, err := vm.Parse("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ra := range c2.QueryRawRedacted(q) {
		n++
		for _, e := range ra.Exprs {
			if strings.Contains(string(e), secret) {
				t.Fatalf("a keyless reader rendered the secret: %q", string(e))
			}
		}
	}
	if n == 0 {
		t.Error("a keyless reader saw NO ads; the public attributes of an ad with a sealed attribute " +
			"must still be readable")
	}
	t.Logf("keyless reader read %d ads with the secret withheld", n)
}

// TestPrivilegedRawReadRendersSealedValues covers the other half of the split. A privileged raw read of an
// encrypted collection used to return NOTHING: wireToInline opened each sealed value and re-sealed it, the
// renderer cannot render a sealed node, so appendWireAd reported every ad undecodable and the scan skipped
// it -- an empty result rather than an error, for the trusted consumer the API exists to serve. It matters
// more once every DB has sealed attributes.
func TestPrivilegedRawReadRendersSealedValues(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "ClaimId-super-secret-capability-9f83a"
	openC := func() *Collection {
		c, err := Open(Options{
			Shards: 1, Dir: dir, SegmentSize: 1 << 16,
			DataKey: dataKey, EncryptedAttrs: []string{"ClaimId"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := openC()
	for i := 0; i < 400; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	c.Compact()
	c.Close()
	c = openC()
	defer c.Close()

	q, err := vm.Parse("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	ads, withSecret := 0, 0
	for ra := range c.QueryRaw(q) {
		ads++
		for _, e := range ra.Exprs {
			if strings.Contains(string(e), secret) {
				withSecret++
				break
			}
		}
	}
	if ads == 0 {
		t.Fatal("privileged raw read of an encrypted collection returned no ads at all")
	}
	if withSecret != ads {
		t.Errorf("%d of %d ads carried the sealed value; a privileged raw read exists to serve it",
			withSecret, ads)
	}
	t.Logf("privileged raw read: %d ads, %d carrying the opened sealed value", ads, withSecret)
}

// TestRedactedReadWithholdsNonPrivateSealedAttr closes the edge that name-based redaction structurally
// cannot: EncryptedAttrs may name an attribute that is NOT in HTCondor's private set. Redaction skips by
// private name, so it never skipped such a node -- the renderer met a sealed value it could not render and
// dropped the whole ad, and had it rendered anything it would have been the secret.
//
// Redacting by NODE covers it: no key, no value, whatever the attribute is called.
func TestRedactedReadWithholdsNonPrivateSealedAttr(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	_, dataKey := deriveDataKey(t)
	const secret = "payroll-9f83a-not-a-private-name"
	openC := func() *Collection {
		c, err := Open(Options{
			Shards: 1, Dir: dir, SegmentSize: 1 << 16,
			DataKey: dataKey, EncryptedAttrs: []string{"Salary"}, // not an HTCondor private attribute
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := openC()
	for i := 0; i < 400; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; Salary=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	c.Compact()
	c.Close()
	c = openC()
	defer c.Close()

	q, err := vm.Parse("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	// Privileged: the value is there (the premise -- otherwise withholding proves nothing).
	priv, sawSecret := 0, false
	for ra := range c.QueryRaw(q) {
		priv++
		for _, e := range ra.Exprs {
			if strings.Contains(string(e), secret) {
				sawSecret = true
			}
		}
	}
	if priv == 0 || !sawSecret {
		t.Fatalf("privileged raw read: %d ads, secret seen = %v; both must hold for this test to mean "+
			"anything", priv, sawSecret)
	}
	// Redacted: same ads, no value, even though "Salary" is not a private NAME.
	red := 0
	for ra := range c.QueryRawRedacted(q) {
		red++
		for _, e := range ra.Exprs {
			if strings.Contains(string(e), secret) {
				t.Fatalf("a redacted read rendered a non-private sealed attribute: %q", string(e))
			}
		}
	}
	if red != priv {
		t.Errorf("redacted read returned %d ads, privileged %d: the ad must survive, only the value goes",
			red, priv)
	}
	t.Logf("non-private sealed attr: %d ads privileged (value visible), %d redacted (value withheld)",
		priv, red)
}
