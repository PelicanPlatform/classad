package crypt

import (
	"bytes"
	"testing"
)

func TestMasterKeyWrapMultiKeyOpen(t *testing.T) {
	master, err := NewMaster()
	if err != nil {
		t.Fatal(err)
	}
	pool := KEK{ID: "POOL", Material: []byte("pool-signing-key-material-............")}
	token := KEK{ID: "TOKEN", Material: []byte("another-different-key-material-.......")}

	rowP, err := WrapMaster(master, pool)
	if err != nil {
		t.Fatal(err)
	}
	rowT, err := WrapMaster(master, token)
	if err != nil {
		t.Fatal(err)
	}
	rows := []MasterKeyRow{rowP, rowT}

	// Either key alone recovers the same master.
	for _, keys := range [][]KEK{{pool}, {token}, {token, pool}} {
		got, err := OpenMaster(rows, keys)
		if err != nil {
			t.Fatalf("OpenMaster with %d key(s): %v", len(keys), err)
		}
		if !bytes.Equal(got, master) {
			t.Errorf("recovered master mismatch")
		}
	}

	// A key that matches no row (rotated away / unknown id) -> ErrNoKey.
	other := KEK{ID: "OTHER", Material: []byte("x")}
	if _, err := OpenMaster(rows, []KEK{other}); err != ErrNoKey {
		t.Errorf("unknown key: err = %v, want ErrNoKey", err)
	}
	// Right id, WRONG material -> unwrap fails -> ErrNoKey (not a panic, not the master).
	if _, err := OpenMaster(rows, []KEK{{ID: "POOL", Material: []byte("wrong-material")}}); err != ErrNoKey {
		t.Errorf("wrong material: err = %v, want ErrNoKey", err)
	}
	// Tampered ciphertext for a matching key -> ErrNoKey.
	bad := rowP
	bad.Wrapped = append([]byte(nil), rowP.Wrapped...)
	bad.Wrapped[0] ^= 0xff
	if _, err := OpenMaster([]MasterKeyRow{bad}, []KEK{pool}); err != ErrNoKey {
		t.Errorf("tampered wrap: err = %v, want ErrNoKey", err)
	}
}

func TestSubkeysDistinctAndDeterministic(t *testing.T) {
	master, _ := NewMaster()
	data1, _ := Subkey(master, DataInfo)
	data2, _ := Subkey(master, DataInfo)
	backup, _ := Subkey(master, BackupInfo)
	if !bytes.Equal(data1, data2) {
		t.Error("Subkey not deterministic for the same label")
	}
	if bytes.Equal(data1, backup) {
		t.Error("data and backup subkeys must differ")
	}
	if len(data1) != KeySize {
		t.Errorf("subkey size = %d, want %d", len(data1), KeySize)
	}
	// A different master yields a different data key.
	other, _ := NewMaster()
	odata, _ := Subkey(other, DataInfo)
	if bytes.Equal(data1, odata) {
		t.Error("subkey should depend on the master")
	}
}

func TestSegmentDEKWrapRoundTrip(t *testing.T) {
	master, _ := NewMaster()
	dataKey, _ := Subkey(master, DataInfo)

	dek, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	nonce, wrapped, err := WrapDEK(dek, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapDEK(dataKey, nonce, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("unwrapped DEK mismatch")
	}
	// The backup key must NOT unwrap a data-key-wrapped DEK.
	backupKey, _ := Subkey(master, BackupInfo)
	if _, err := UnwrapDEK(backupKey, nonce, wrapped); err == nil {
		t.Error("wrong key unwrapped the DEK")
	}
}

func TestSealOpenTamperDetected(t *testing.T) {
	dek, _ := NewDEK()
	pt := []byte("secret: X509_USER_PROXY contents / ClaimId")
	nonce, ct, err := Seal(dek, pt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(dek, nonce, ct)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round trip failed: %v", err)
	}
	ct[len(ct)-1] ^= 0x01 // flip a tag bit
	if _, err := Open(dek, nonce, ct); err == nil {
		t.Error("GCM tag tampering not detected")
	}
}
