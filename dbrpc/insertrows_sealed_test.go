package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// A copy moves stored values verbatim, so a privileged one can carry a value out from under
// the encryption that protected it at rest: the reader decrypts it, and the target writes it
// in the clear. Nothing downstream reports that -- the copy succeeds, the rows read back
// correctly, and the only difference is on disk. So it is refused.
//
// This cannot arise for a roll-up, which writes a SUMMARY rather than the stored values.
func TestInsertRowsRefusesToUnsealAValue(t *testing.T) {
	ctx := context.Background()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir(),
		PoolKeys: []db.KEK{{ID: "POOL", Material: []byte("insert-rows-sealed-copy-key-material!")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	src, err := cat.CreateTable("payroll")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ Jid = %d; Owner = "u%d"; Salary = "secret-%d" ]`, i, i, i))
		if perr != nil {
			t.Fatal(perr)
		}
		if putErr := src.Put(fmt.Sprintf("%d.0", i), ad); putErr != nil {
			t.Fatal(putErr)
		}
	}
	for _, name := range []string{"bare", "sealed"} {
		if _, cerr := cat.CreateTable(name); cerr != nil {
			t.Fatal(cerr)
		}
	}

	s := NewServerCatalog(cat)
	defer s.Close()
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn)
	defer c.Close()

	if _, encErr := c.SetEncryptedAttrs(ctx, "payroll", "Salary"); encErr != nil {
		t.Fatal(encErr)
	}
	if _, encErr := c.SetEncryptedAttrs(ctx, "sealed", "Salary"); encErr != nil {
		t.Fatal(encErr)
	}
	for _, tbl := range []string{"bare", "sealed"} {
		if keyErr := c.SetPrimaryKey(ctx, tbl, []string{"Jid"}); keyErr != nil {
			t.Fatal(keyErr)
		}
	}

	// Named projection carrying the sealed column into a target that does not seal it.
	_, err = c.InsertRows(ctx, "bare", "payroll", "true",
		[]string{"Jid", "Salary"}, []string{"Jid", "Salary"})
	if err == nil || !strings.Contains(err.Error(), "would store it in the clear") {
		t.Fatalf("copy into an unsealed target: err = %v, want a refusal", err)
	}

	// Renaming it does not get around the check -- the TARGET name is what is looked up.
	_, err = c.InsertRows(ctx, "bare", "payroll", "true",
		[]string{"Jid", "Salary"}, []string{"Jid", "Pay"})
	if err == nil || !strings.Contains(err.Error(), "as Pay") {
		t.Fatalf("renamed copy: err = %v, want a refusal naming the rename", err)
	}

	// SELECT * carries every attribute, so it is refused too.
	if _, err = c.InsertRows(ctx, "bare", "payroll", "true", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "would store it in the clear") {
		t.Fatalf("SELECT * copy: err = %v, want a refusal", err)
	}

	// Leaving the sealed column OUT is allowed: nothing is unsealed.
	n, err := c.InsertRows(ctx, "bare", "payroll", "true",
		[]string{"Jid", "Owner"}, []string{"Jid", "Owner"})
	if err != nil {
		t.Fatalf("copy that omits the sealed column was refused: %v", err)
	}
	if n != 3 {
		t.Fatalf("copied %d rows, want 3", n)
	}

	// And a target that seals the column it lands in takes it.
	n, err = c.InsertRows(ctx, "sealed", "payroll", "true",
		[]string{"Jid", "Salary"}, []string{"Jid", "Salary"})
	if err != nil {
		t.Fatalf("copy into a sealing target was refused: %v", err)
	}
	if n != 3 {
		t.Fatalf("copied %d rows into the sealing target, want 3", n)
	}
}

// An UNPRIVILEGED copy needs no seal check: the reader never sees the value, so there is
// nothing to leak. It silently drops the column, which is what redaction does everywhere.
func TestInsertRowsUnprivilegedCopyDropsSealedColumn(t *testing.T) {
	ctx := context.Background()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir(),
		PoolKeys: []db.KEK{{ID: "POOL", Material: []byte("insert-rows-unpriv-copy-key-material!")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	src, err := cat.CreateTable("payroll")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(`[ Jid = %d; Salary = "secret-%d" ]`, i, i))
		if perr != nil {
			t.Fatal(perr)
		}
		if putErr := src.Put(fmt.Sprintf("%d.0", i), ad); putErr != nil {
			t.Fatal(putErr)
		}
	}
	dst, err := cat.CreateTable("bare")
	if err != nil {
		t.Fatal(err)
	}

	s := NewServerCatalog(cat)
	defer s.Close()
	connect := func(opts ServeOptions) *Client {
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConnOpts(sconn, opts) }()
		cl := NewClient(cconn)
		t.Cleanup(func() { cl.Close() })
		return cl
	}
	pc := connect(ServeOptions{IncludePrivate: true, Privileged: true})
	if _, encErr := pc.SetEncryptedAttrs(ctx, "payroll", "Salary"); encErr != nil {
		t.Fatal(encErr)
	}
	if keyErr := pc.SetPrimaryKey(ctx, "bare", []string{"Jid"}); keyErr != nil {
		t.Fatal(keyErr)
	}

	uc := connect(ServeOptions{})
	n, err := uc.InsertRows(ctx, "bare", "payroll", "true",
		[]string{"Jid", "Salary"}, []string{"Jid", "Salary"})
	if err != nil {
		t.Fatalf("unprivileged copy was refused: %v", err)
	}
	if n != 3 {
		t.Fatalf("copied %d rows, want 3", n)
	}
	// The ciphertext must not have landed in the target, and neither must the plaintext.
	for _, k := range dst.Keys() {
		ad, ok := dst.LookupClassAd(k)
		if !ok {
			t.Fatalf("key %s vanished", k)
		}
		if _, present := ad.Lookup("Salary"); present {
			t.Fatalf("key %s carries Salary after an unprivileged copy: %s", k, ad.String())
		}
	}
}
