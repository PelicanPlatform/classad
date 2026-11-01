package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// attrTypePair serves one store over two connections: one privileged, one an ordinary
// unprivileged reader, so a test can compare what each is served without standing up two stores.
func attrTypePair(t *testing.T) (priv, unpriv *Client, cleanup func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)

	pc, ps := netPipe()
	go func() { _ = s.ServeConnOpts(ps, ServeOptions{Privileged: true, IncludePrivate: true}) }()
	uc, us := netPipe()
	go func() { _ = s.ServeConnOpts(us, ServeOptions{ReadOnly: true}) }()

	priv, unpriv = NewClient(pc), NewClient(uc)
	return priv, unpriv, func() { priv.Close(); unpriv.Close(); s.Close(); d.Close() }
}

func seedAttrTypeAds(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		// Cpus is uniformly int; Label uniformly string; Mixed is int except every tenth,
		// which is a string. ClaimId is private and must never reach an unprivileged peer.
		src := fmt.Sprintf("Cpus = %d\nLabel = \"n%d\"\nMixed = %d\nClaimId = \"secret-%d\"", 1+i%4, i, i, i)
		if i%10 == 0 {
			src = fmt.Sprintf("Cpus = %d\nLabel = \"n%d\"\nMixed = \"n/a\"\nClaimId = \"secret-%d\"", 1+i%4, i, i)
		}
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), src); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func byName(rep *AttrTypeReport) map[string]db.AttrTypeProfile {
	m := map[string]db.AttrTypeProfile{}
	for _, p := range rep.Profiles {
		m[p.Name] = p
	}
	return m
}

func TestAttrTypeProfilesRoundTrip(t *testing.T) {
	priv, _, cleanup := attrTypePair(t)
	defer cleanup()
	seedAttrTypeAds(t, priv)

	rep, err := priv.AttrTypeProfiles(context.Background(), DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sampled != 60 {
		t.Fatalf("sampled %d, want 60", rep.Sampled)
	}
	got := byName(rep)

	if p := got["Cpus"]; !p.Uniform() || p.Dominant != "int" || p.IntMin != 1 || p.IntMax != 4 {
		t.Errorf("Cpus: uniform=%v dominant=%q [%d,%d], want int [1,4]",
			p.Uniform(), p.Dominant, p.IntMin, p.IntMax)
	}
	if p := got["Label"]; !p.Uniform() || p.Dominant != "string" {
		t.Errorf("Label: uniform=%v dominant=%q, want string", p.Uniform(), p.Dominant)
	}
	// The whole point of the measurement: a mixed attribute arrives mixed, with the split.
	if p := got["Mixed"]; p.Uniform() {
		t.Errorf("Mixed: reported uniform, classes %v", p.Classes)
	} else if p.Classes["int"] != 54 || p.Classes["string"] != 6 {
		t.Errorf("Mixed: classes %v, want 54 int / 6 string", p.Classes)
	}
}

// TestAttrTypeProfilesHidesPrivateAttrs is the one that matters. A profile carries no values,
// but it carries numeric bounds and the attribute's existence, and this package already refuses
// an unprivileged constraint that merely references a private attribute -- serving its profile
// would be a larger disclosure through a quieter door.
func TestAttrTypeProfilesHidesPrivateAttrs(t *testing.T) {
	priv, unpriv, cleanup := attrTypePair(t)
	defer cleanup()
	seedAttrTypeAds(t, priv)
	ctx := context.Background()

	privRep, err := priv.AttrTypeProfiles(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byName(privRep)["ClaimId"]; !ok {
		t.Fatal("precondition: ClaimId absent even for a privileged peer, so this proves nothing")
	}

	unprivRep, err := unpriv.AttrTypeProfiles(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byName(unprivRep)["ClaimId"]; ok {
		t.Error("ClaimId profile served to an unprivileged peer")
	}
	// Omission must be the only difference: the public attributes still come through, or a
	// "fix" that returned nothing at all would pass the check above.
	for _, name := range []string{"Cpus", "Label", "Mixed"} {
		if _, ok := byName(unprivRep)[name]; !ok {
			t.Errorf("%s: withheld from an unprivileged peer, want served", name)
		}
	}
}

// TestAttrTypeProfilesReadOnlyConnection pins the reason this is its own op rather than a
// schema.* admin action: opAdmin is mutating, so a read-only connection cannot reach it, and on
// an archive it requires DAEMON. A catalog consumer reads at READ.
func TestAttrTypeProfilesReadOnlyConnection(t *testing.T) {
	priv, unpriv, cleanup := attrTypePair(t)
	defer cleanup()
	seedAttrTypeAds(t, priv)
	ctx := context.Background()

	if _, err := unpriv.AttrTypeProfiles(ctx, DefaultTable, 1000); err != nil {
		t.Fatalf("read-only connection refused the profile: %v", err)
	}
	// The contrast: an admin action on the same connection IS refused.
	if _, err := unpriv.Admin(ctx, "schema.fit"); err == nil {
		t.Error("precondition: schema.fit succeeded read-only, so the contrast is not what this claims")
	}
}

// TestAttrTypeProfilesArchive covers the archive path, which is the reason the op exists at
// all. History is where attribute variety is widest, so a measurement that could not reach an
// archive would be unavailable exactly where it is most needed -- and routing this through the
// schema.* admin family would have done that twice over, since opAdmin is mutating AND its
// archive branch requires DAEMON for the whole op.
func TestAttrTypeProfilesArchive(t *testing.T) {
	// A catalog-backed server: the single-table server has no archives at all.
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	pc, ps := netPipe()
	go func() { _ = s.ServeConnOpts(ps, ServeOptions{Privileged: true, IncludePrivate: true}) }()
	uc, us := netPipe()
	go func() { _ = s.ServeConnOpts(us, ServeOptions{ReadOnly: true}) }()
	priv, unpriv := NewClient(pc), NewClient(uc)
	defer func() { priv.Close(); unpriv.Close(); s.Close(); cat.Close() }()
	ctx := context.Background()

	if err := priv.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ValueAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		src := fmt.Sprintf("ClusterId = %d\nBytes = %d\nClaimId = \"secret-%d\"", i, i, i)
		if i%10 == 0 {
			// A real float among the integers: the dominant heterogeneity on real data,
			// where ClassAd writes an integral float without a decimal point.
			src = fmt.Sprintf("ClusterId = %d\nBytes = %d.5\nClaimId = \"secret-%d\"", i, i, i)
		}
		if err := priv.ArchiveAppend(ctx, "history", src); err != nil {
			t.Fatal(err)
		}
	}

	// An ordinary read-only peer, on an ARCHIVE: the case the admin family could not serve.
	rep, err := unpriv.AttrTypeProfiles(ctx, "history", 1000)
	if err != nil {
		t.Fatalf("read-only peer refused an archive profile: %v", err)
	}
	if rep.Sampled == 0 {
		t.Fatal("archive profile sampled nothing")
	}
	got := byName(rep)
	if p, ok := got["ClusterId"]; !ok || !p.Uniform() || p.Dominant != "int" {
		t.Errorf("ClusterId: ok=%v uniform=%v dominant=%q, want a uniform int", ok, p.Uniform(), p.Dominant)
	}
	if p := got["Bytes"]; p.Uniform() {
		t.Errorf("Bytes: reported uniform, classes %v (want the int/real mix)", p.Classes)
	} else if p.IntsLossyAsFloat != 0 {
		t.Errorf("Bytes: lossy=%d, want 0 -- these values all round-trip", p.IntsLossyAsFloat)
	}
	// Privacy holds on the archive path too; it is a separate branch in the handler.
	if _, ok := got["ClaimId"]; ok {
		t.Error("ClaimId profile served to an unprivileged peer from an archive")
	}
}

func TestAttrTypeProfilesUnknownTable(t *testing.T) {
	priv, _, cleanup := attrTypePair(t)
	defer cleanup()
	if _, err := priv.AttrTypeProfiles(context.Background(), "nosuch", 100); err == nil {
		t.Error("AttrTypeProfiles on a missing table returned no error")
	}
}
