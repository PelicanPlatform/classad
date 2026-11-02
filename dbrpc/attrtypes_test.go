package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
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
		// Scaled is written as an expression in every record, because an archive is where
		// a consumer most needs this measurement and where a job ad most reliably stores
		// one -- and because the archive is a separate branch of the handler.
		src := fmt.Sprintf("ClusterId = %d\nBytes = %d\nScaled = Bytes * 8\nClaimId = \"secret-%d\"", i, i, i)
		if i%10 == 0 {
			// A real float among the integers: the dominant heterogeneity on real data,
			// where ClassAd writes an integral float without a decimal point.
			src = fmt.Sprintf("ClusterId = %d\nBytes = %d.5\nScaled = Bytes * 8\nClaimId = \"secret-%d\"", i, i, i)
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
	if p := got["Scaled"]; p.Dominant != "expr" || p.Evaluated != nil {
		t.Errorf("Scaled stored classes are %v with evaluated=%v; the fixture must store it as an "+
			"expression and the default report must not evaluate it", p.Classes, p.Evaluated)
	}

	// And the evaluated option reaches the archive branch, which is the half a test on the
	// mutable table alone would leave unserved -- the mistake this op's own history records.
	erep, err := unpriv.AttrTypeProfilesEvaluated(ctx, "history", 1000)
	if err != nil {
		t.Fatalf("read-only peer refused an evaluated archive profile: %v", err)
	}
	p := byName(erep)["Scaled"]
	if p.Evaluated == nil {
		t.Fatal("Scaled has no evaluated counts from the archive")
	}
	// Bytes is an int in nine records of ten and a real in the tenth, so Bytes*8 is both.
	if p.Evaluated.Ints == 0 || p.Evaluated.Reals == 0 {
		t.Fatalf("Scaled evaluated to %v, want the int/real mix Bytes carries into it", p.Evaluated.Classes)
	}
}

func TestAttrTypeProfilesUnknownTable(t *testing.T) {
	priv, _, cleanup := attrTypePair(t)
	defer cleanup()
	if _, err := priv.AttrTypeProfiles(context.Background(), "nosuch", 100); err == nil {
		t.Error("AttrTypeProfiles on a missing table returned no error")
	}
}

// TestAttrTypeProfilesEvaluatedTypesAnExpression is why the evaluated option exists. Mem is
// written as an expression over Cpus in every ad, so the stored classes rule it untypeable --
// and a consumer that evaluates before it encodes would be publishing no column for an
// attribute that is an integer in every row.
func TestAttrTypeProfilesEvaluatedTypesAnExpression(t *testing.T) {
	priv, _, cleanup := attrTypePair(t)
	defer cleanup()
	ctx := context.Background()
	tx, err := priv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		src := fmt.Sprintf("Cpus = %d\nMem = Cpus * 1024\n", 1+i%4)
		if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", i), src); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	stored, err := priv.AttrTypeProfiles(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if p := byName(stored)["Mem"]; p.Dominant != "expr" {
		t.Fatalf("Mem stored classes are %v; the fixture must store it as an expression", p.Classes)
	} else if p.Evaluated != nil {
		t.Fatal("the default report carries evaluated counts, which are the expensive measurement")
	}

	rep, err := priv.AttrTypeProfilesEvaluated(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	p := byName(rep)["Mem"]
	if p.Evaluated == nil {
		t.Fatal("Mem has no evaluated counts")
	}
	// Cpus runs 1..4, so Mem runs 1024..4096.
	if !p.Evaluated.Uniform() || p.Evaluated.Dominant != "int" ||
		p.Evaluated.IntMin != 1024 || p.Evaluated.IntMax != 4096 {
		t.Fatalf("Mem evaluates to %v in [%d,%d], want a uniform int in [1024,4096]",
			p.Evaluated.Classes, p.Evaluated.IntMin, p.Evaluated.IntMax)
	}
}

// TestAttrTypeProfilesEvaluatedWithholdsSealedValues is the disclosure the option opens and the
// server has to close. The stored classes never look through a sealed node, so the privilege of
// the connection did not matter to this op before; evaluating an expression resolves its
// references, and an expression over a sealed attribute would hand an unprivileged peer the
// bounds of a value it may not read -- under a name the private-attribute filter has no reason
// to suspect.
func TestAttrTypeProfilesEvaluatedWithholdsSealedValues(t *testing.T) {
	d, err := db.OpenConfig(db.Config{
		Dir:            t.TempDir(),
		PoolKeys:       []db.KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"Salary"},
	})
	if err != nil {
		t.Skipf("encrypted table unavailable here: %v", err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 60; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("Cpus = %d\nSalary = %d\nDerived = Salary * 2", 1+i%4, 100000+i))
		if perr != nil {
			t.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	s := NewServer(d)
	defer s.Close()
	dial := func(opts ServeOptions) *Client {
		cc, sc := netPipe()
		go func() { _ = s.ServeConnOpts(sc, opts) }()
		c := NewClient(cc)
		t.Cleanup(func() { c.Close() })
		return c
	}
	ctx := context.Background()

	priv, err := dial(ServeOptions{Privileged: true, IncludePrivate: true}).
		AttrTypeProfilesEvaluated(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// The premise: the entitled read DOES see through the expression. Without this the test
	// would pass against a server that evaluated nothing at all.
	if p := byName(priv)["Derived"]; p.Evaluated == nil || p.Evaluated.Ints == 0 {
		t.Fatalf("the entitled report does not evaluate Derived (%v); there is nothing to withhold", p.Evaluated)
	}

	unpriv, err := dial(ServeOptions{ReadOnly: true}).AttrTypeProfilesEvaluated(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatal(err)
	}
	p := byName(unpriv)["Derived"]
	if p.Evaluated == nil {
		t.Fatal("Derived has no evaluated counts for the unprivileged reader")
	}
	if p.Evaluated.Ints != 0 {
		t.Fatalf("the unprivileged report carries %d integers in [%d,%d] for Derived = Salary * 2: "+
			"the bounds of a sealed value reached a peer that may not read it",
			p.Evaluated.Ints, p.Evaluated.IntMin, p.Evaluated.IntMax)
	}
}

// TestAttrTypeProfilesRejectsUnknownFlags holds the op to the same rule as the streaming ones: a
// request asking for something this server does not know is refused, never served without it. The
// quiet failure is the dangerous one here, because the reply looks exactly like a successful
// measurement of a table whose attributes happen to be untypeable.
func TestAttrTypeProfilesRejectsUnknownFlags(t *testing.T) {
	priv, _, cleanup := attrTypePair(t)
	defer cleanup()
	seedAttrTypeAds(t, priv)

	unknown := ^attrFlagsKnown
	if _, err := priv.attrTypeProfiles(context.Background(), DefaultTable, 100, unknown); err == nil {
		t.Fatal("a request with unknown flag bits was served")
	}
}

// flagStrippingConn is a server-side transport that chops the trailing flags word off every
// attr-types request, which is exactly the frame an older client sends and exactly what an
// older server would read from a newer one. It makes both halves of the compatibility story
// testable for real: the server must answer such a request rather than refuse it, and the
// client must not report the stored-only answer it gets back as the evaluated one it asked for.
type flagStrippingConn struct {
	MsgConn
}

func (c *flagStrippingConn) ReadMsg() ([]byte, error) {
	frame, err := c.MsgConn.ReadMsg()
	if err != nil {
		return nil, err
	}
	if o, ok := frameOp(frame); ok && o == opAttrTypes && len(frame) >= 4 {
		return frame[:len(frame)-4], nil
	}
	return frame, nil
}

// TestAttrTypeProfilesWithoutTheFlagsWord is the mixed-version pair in both directions.
//
// A request with no flags word is well formed, not malformed: it is what every client built
// before the option sends, and refusing it would break the measurement on an upgrade. And a
// client that asked for the evaluated classes must not be handed that older report as though it
// were the answer -- it is a different answer, and a caller acting on it would conclude that
// every expression-valued attribute in the table is untypeable.
func TestAttrTypeProfilesWithoutTheFlagsWord(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(&flagStrippingConn{MsgConn: sconn}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()
	seedAttrTypeAds(t, c)
	ctx := context.Background()

	rep, err := c.AttrTypeProfiles(ctx, DefaultTable, 1000)
	if err != nil {
		t.Fatalf("a request with no flags word was refused: %v", err)
	}
	if rep.Sampled == 0 || len(rep.Profiles) == 0 {
		t.Fatalf("the stored-classes request came back empty: %+v", rep)
	}

	if _, err := c.AttrTypeProfilesEvaluated(ctx, DefaultTable, 1000); !errors.Is(err, ErrEvalTypesUnsupported) {
		t.Fatalf("asking a server that drops the flag for evaluated classes returned %v, want ErrEvalTypesUnsupported", err)
	}
}
