package db

import "testing"

// Read verification is ON by default in collections, so a db table inherits it and
// these tests are about the WIRING: that a catalog-wide setting reaches every table's
// Config, and so the collection it opens.
//
// They deliberately do not assert a behavioral difference, because at this layer there
// may not be one to assert. A db table compresses ads with ZSTD by default, and a zstd
// frame carries its own content checksum, so a bit flip in the payload is refused with
// or without record verification. Attempts to find a db-layer corruption that the
// record CRC catches and zstd does not -- in the ad body, in the stored key, in the
// commit sequence -- all behaved identically either way. A test asserting a difference
// would have been a test asserting something untrue.
//
// What the record CRC uniquely covers is therefore an open question at this layer, and
// the behavioral case for it lives in collections (where the identity codec leaves the
// ad unprotected and a flipped bit is served as a plausible value). This option exists
// so an operator can turn the check off without downgrading.

// TestTableConfigCarriesTheDefault: with nothing set, a table's Config does not disable
// verification, so the collections default (on) applies.
func TestTableConfigCarriesTheDefault(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if cfg := cat.tableConfig("somedir"); cfg.DisableReadVerification {
		t.Error("a default catalog disables read verification; it should inherit the collections default (on)")
	}
}

// TestTableConfigCarriesTheOverride: the catalog-wide setting must reach every table,
// or an operator reacting to read cost has no way out short of downgrading.
func TestTableConfigCarriesTheOverride(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir(), DisableReadVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if cfg := cat.tableConfig("somedir"); !cfg.DisableReadVerification {
		t.Error("CatalogConfig.DisableReadVerification did not reach the per-table Config")
	}
}

// TestOpenConfigPassesItToCollections: the field has to survive the last hop too. A
// table opened with it set must not be verifying; one opened without it must be.
func TestOpenConfigPassesItToCollections(t *testing.T) {
	for _, disable := range []bool{false, true} {
		d, err := OpenConfig(Config{Dir: t.TempDir(), DisableReadVerification: disable})
		if err != nil {
			t.Fatal(err)
		}
		if got := d.VerifyingReads(); got == disable {
			t.Errorf("DisableReadVerification=%v: collection verifying=%v; want %v", disable, got, !disable)
		}
		d.Close()
	}
}
