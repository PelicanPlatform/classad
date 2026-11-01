package collections

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// A transaction not entitled to sealed values reads them as undefined. When one of its patch writes
// needed a whole record -- a removal, the chain bound, delta records off -- the record was composed
// from that read and stored, writing undefined over every sealed value the ad held. These tests run
// redacted writes in every record layout and require a privileged read afterwards to equal a
// plaintext model in which only the attributes the writes NAMED changed.

// redactedOp is one write to one key: set some attributes, remove others.
type redactedOp struct {
	key    string
	set    map[string]string // attribute -> expression text
	remove []string
}

// apply runs op in its own redacted transaction. It reports the commit's reason when the write was
// refused, "" when it committed.
func (op redactedOp) apply(t *testing.T, c *Collection) string {
	t.Helper()
	tx := c.BeginRedacted()
	var patch *classad.ClassAd
	if len(op.set) > 0 {
		patch = classad.New()
		for n, src := range op.set {
			e, err := classad.ParseExpr(src)
			if err != nil {
				t.Fatal(err)
			}
			patch.InsertExpr(n, e)
		}
	}
	tx.PatchAttrs([]byte(op.key), patch, op.remove)
	r := tx.Commit()
	if r.Conflicted() {
		t.Fatalf("%s: conflicted", op.key)
	}
	if r.HasUnapplied() {
		return r.UnappliedReasons[0]
	}
	if r.Committed != 1 {
		t.Fatalf("%s: committed %d", op.key, r.Committed)
	}
	return ""
}

// applyModel applies op to the plaintext model.
func (op redactedOp) applyModel(t *testing.T, model map[string]*classad.ClassAd) {
	t.Helper()
	ad := model[op.key]
	if ad == nil {
		ad = classad.New()
		model[op.key] = ad
	}
	for n, src := range op.set {
		e, err := classad.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		ad.InsertExpr(n, e)
	}
	for _, n := range op.remove {
		ad.Delete(n)
	}
}

// sealedModel is the plaintext of every record a layout stores.
func sealedModel(t *testing.T, patched bool) map[string]*classad.ClassAd {
	t.Helper()
	m := make(map[string]*classad.ClassAd, sealedParityN)
	for i := 0; i < sealedParityN; i++ {
		m[fmt.Sprintf("%d.0", i)] = mustAd(t, sealedParityAd(i, patched))
	}
	return m
}

// checkModel requires a privileged read of every key to equal the model, private attributes included.
func checkModel(t *testing.T, label string, c *Collection, model map[string]*classad.ClassAd) {
	t.Helper()
	bad := 0
	for k, want := range model {
		got, ok := c.Get([]byte(k))
		if !ok {
			t.Errorf("%s: %s is missing", label, k)
			bad++
		} else if g, w := got.StringWithPrivate(), want.StringWithPrivate(); g != w {
			t.Errorf("%s: %s\n got  %s\n want %s", label, k, g, w)
			bad++
		}
		if bad > 5 {
			t.Fatalf("%s: giving up", label)
		}
	}
}

// heldSealedFromInterned reports whether key's stored version has an interned record carrying a
// sealed value that op neither sets nor removes and no later part of the chain replaces. Only such a
// write may be refused: carrying that value into the inline record a write stores needs the key.
func heldSealedFromInterned(t *testing.T, c *Collection, op redactedOp) bool {
	t.Helper()
	h := c.h.Hash([]byte(op.key))
	sh := c.shards[c.shardOf([]byte(op.key), h)]
	sh.mu.RLock()
	s0 := sh.commitSeq
	sh.mu.RUnlock()
	parts, why := sh.keylessParts(c, []byte(op.key), h, s0)
	if why != failNone {
		return false
	}
	gone := map[string]bool{}
	for n := range op.set {
		gone[strings.ToLower(n)] = true
	}
	for _, n := range op.remove {
		gone[strings.ToLower(n)] = true
	}
	for i := len(parts) - 1; i >= 0; i-- {
		var names []string
		held := false
		ad := parts[i].raw
		ok := wireAdForEach(ad, parts[i].resolve, func(name string, sealed, interned bool) {
			if sealed && interned && !gone[strings.ToLower(name)] {
				held = true
			}
			names = append(names, strings.ToLower(name))
		})
		if !ok {
			t.Fatalf("%s: stored record does not walk", op.key)
		}
		if held {
			return true
		}
		for _, n := range names {
			gone[n] = true
		}
	}
	return false
}

func TestRedactedPatchPreservesSealed(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			model := sealedModel(t, lay.patched)
			// Each trigger of the whole-record fallback, and the delta path beside them, on keys spread
			// over the layout (a mixed layout's first half is interned, its second inline).
			ops := []redactedOp{
				{key: "1.0", remove: []string{"Filler0"}},                                             // removal
				{key: "2.0", set: map[string]string{"JobStatus": "7"}, remove: []string{"Filler1"}},   // removal with a set
				{key: "4.0", set: map[string]string{"Owner": `"mallory"`}, remove: []string{"Token"}}, // named sealed writes
				{key: "151.0", remove: []string{"Filler2"}},                                           // mixed: inline half
				{key: "152.0", set: map[string]string{"Owner": `"m2"`}, remove: []string{"ClaimId", "Token", "SealedNum"}},
				{key: "10.0", set: map[string]string{"Filler3": `"plain delta"`}},                    // the delta path
				{key: "new.0", set: map[string]string{"Owner": `"newbie"`, "ClaimId": `"<new>"`}},    // first write
				{key: "new.1", set: map[string]string{"JobStatus": "1"}, remove: []string{"Absent"}}, // first write, removal
			}
			for _, op := range ops {
				// Asked before the write: what it may be refused for is what is stored now.
				mayRefuse := heldSealedFromInterned(t, c, op)
				switch why := op.apply(t, c); {
				case why == "":
					op.applyModel(t, model)
				case why != failSealedNeedsKey.String():
					t.Errorf("%s: refused for %q", op.key, why)
				case !mayRefuse:
					t.Errorf("%s: refused (%s) though nothing it must keep is sealed in an interned record", op.key, why)
				case len(op.remove) == 0:
					t.Errorf("%s: a set with delta records on was refused (%s); it must be stored as a delta", op.key, why)
				}
			}
			// The chain bound: more patches to one key than DeltaMax allows, each in its own transaction,
			// so the tracker sends one of them to a whole record.
			for k := 0; k < 3*c.deltaMax; k++ {
				op := redactedOp{key: "5.0", set: map[string]string{"JobStatus": fmt.Sprint(10 + k)}}
				if why := op.apply(t, c); why != "" {
					t.Fatalf("bound write %d refused: %s", k, why)
				}
				op.applyModel(t, model)
			}
			// Delta records off: every patch composes a whole record.
			deltaMax := c.deltaMax
			c.deltaMax = 0
			for _, op := range []redactedOp{
				{key: "6.0", set: map[string]string{"Filler4": `"no deltas"`}},
				{key: "153.0", set: map[string]string{"Filler4": `"no deltas"`}},
			} {
				mayRefuse := heldSealedFromInterned(t, c, op)
				switch why := op.apply(t, c); {
				case why == "":
					op.applyModel(t, model)
				case why != failSealedNeedsKey.String() || !mayRefuse:
					t.Errorf("%s (delta off): refused for %q", op.key, why)
				}
			}
			c.deltaMax = deltaMax
			checkModel(t, "after redacted writes", c, model)
			// The stored records must survive a rewrite with the key: compaction decodes each one.
			internAll(c)
			checkModel(t, "after compaction", c, model)
		})
	}
}

// TestRedactedPatchInlineNeverRefused pins the inline layout, where no sealed value needs the key to
// be carried, so every redacted write must apply. A refusal here would be the fix degrading into
// "refuse whenever in doubt".
func TestRedactedPatchInlineNeverRefused(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c := openSealedLayout(t, sealedLayouts[0])
	if sealedLayouts[0].name != "inline" {
		t.Fatal("sealedLayouts[0] is not the inline layout")
	}
	c.deltaMax = 0
	for i := 0; i < 20; i++ {
		op := redactedOp{key: fmt.Sprintf("%d.0", i), set: map[string]string{"JobStatus": "3"}, remove: []string{"Filler5"}}
		if why := op.apply(t, c); why != "" {
			t.Fatalf("%s refused: %s", op.key, why)
		}
	}
}

// TestRedactedWriteSequenceDifferential runs a random sequence of redacted writes -- sets and removals
// of plain and sealed attributes, several keys per transaction -- in every layout, then compares a
// privileged read of every key to the model, again after compaction rewrites them, and again after a
// reopen.
func TestRedactedWriteSequenceDifferential(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	plain := []string{"JobStatus", "Filler0", "Filler1", "Filler5", "NewAttr"}
	sealed := []string{"Owner", "Token", "SealedNum", "ClaimId"}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			dir := t.TempDir()
			c, err := Open(sealedParityOpts(dir))
			if err != nil {
				t.Fatal(err)
			}
			c = lay.build(t, c, dir)
			model := sealedModel(t, lay.patched)
			rng := rand.New(rand.NewSource(int64(len(lay.name))))
			applied, refused := 0, 0
			for round := 0; round < 60; round++ {
				tx := c.BeginRedacted()
				var ops []redactedOp
				used := map[string]bool{}
				for n := 0; n < 4; n++ {
					key := fmt.Sprintf("%d.0", rng.Intn(40)*7%sealedParityN)
					if used[key] {
						continue
					}
					used[key] = true
					op := redactedOp{key: key, set: map[string]string{}}
					for a := 0; a < 1+rng.Intn(3); a++ {
						name := plain[rng.Intn(len(plain))]
						if rng.Intn(3) == 0 {
							name = sealed[rng.Intn(len(sealed))]
						}
						if rng.Intn(3) == 0 {
							op.remove = append(op.remove, name)
							delete(op.set, name)
						} else if !containsFold(op.remove, name) {
							op.set[name] = fmt.Sprintf(`"r%d-%s"`, round, name)
						}
					}
					var patch *classad.ClassAd
					if len(op.set) > 0 {
						patch = classad.New()
						for n, src := range op.set {
							e, _ := classad.ParseExpr(src)
							patch.InsertExpr(n, e)
						}
					}
					tx.PatchAttrs([]byte(key), patch, op.remove)
					ops = append(ops, op)
				}
				r := tx.Commit()
				if r.Conflicted() {
					t.Fatal("conflict")
				}
				unapplied := map[string]string{}
				for i, k := range r.Unapplied {
					unapplied[string(k)] = r.UnappliedReasons[i]
				}
				for _, op := range ops {
					if why, ok := unapplied[op.key]; ok {
						if why != failSealedNeedsKey.String() {
							t.Fatalf("%s refused for %q", op.key, why)
						}
						refused++
						continue
					}
					op.applyModel(t, model)
					applied++
				}
			}
			t.Logf("%d writes applied, %d refused", applied, refused)
			if applied == 0 {
				t.Fatal("no write applied; the comparison is vacuous")
			}
			checkModel(t, "after redacted writes", c, model)
			internAll(c)
			checkModel(t, "after compaction", c, model)
			if cerr := c.Close(); cerr != nil {
				t.Fatal(cerr)
			}
			c2, err := Open(sealedParityOpts(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer c2.Close()
			checkModel(t, "after reopen", c2, model)
		})
	}
}

func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// TestRedactedWriteNeverOpensKey: a redacted transaction's whole-record writes are composed from bytes
// and never open a sealed value. Equal results cannot show that (opening and re-sealing gives the same
// answer), so this counts every use of the key while each write is encoded.
//
// It drives the encode directly rather than through Commit: a commit that happens to seal a segment
// then collapses the shard's delta chains, which opens values with the key as the store's own
// maintenance -- its output is stored, never handed to the session -- and that is not what this counts.
func TestRedactedWriteNeverOpensKey(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	ops := []redactedOp{
		{key: "151.0", remove: []string{"Filler0"}},
		{key: "152.0", set: map[string]string{"Owner": `"x"`}, remove: []string{"Filler1"}},
		{key: "153.0", set: map[string]string{"JobStatus": "3"}},
		{key: "1.0", remove: []string{"Filler0"}},
		// Replaces or removes every sealed value, so even an interned base composes keylessly.
		{key: "2.0", set: map[string]string{"Owner": `"y"`}, remove: []string{"Token", "SealedNum", "ClaimId"}},
		{key: "new.0", set: map[string]string{"ClaimId": `"<n>"`}},
	}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			c.deltaMax = 0 // every patch takes the whole-record path
			ctr := &countingSealer{inner: c.sealer}
			c.sealer = ctr
			encode := func(tx *Txn, op redactedOp) (bool, readFail) {
				patch := classad.New()
				for n, src := range op.set {
					e, err := classad.ParseExpr(src)
					if err != nil {
						t.Fatal(err)
					}
					patch.InsertExpr(n, e)
				}
				tx.PatchAttrs([]byte(op.key), patch, op.remove)
				_, _, ok, why := tx.encodePatchOnly(nil, tx.writes[op.key], c.h.Hash([]byte(op.key)))
				return ok, why
			}
			composed := 0
			for _, op := range ops {
				before, _ := KeylessWriteStats()
				ok, why := encode(c.BeginRedacted(), op)
				if !ok && why != failSealedNeedsKey {
					t.Fatalf("%s: refused for %s", op.key, why)
				}
				if after, _ := KeylessWriteStats(); after > before {
					composed++
				}
			}
			if o := ctr.opens.Load(); o != 0 {
				t.Errorf("redacted writes opened %d sealed values", o)
			}
			if composed == 0 {
				t.Fatal("no write composed a whole record keylessly; the count proves nothing")
			}
			// Premise: an entitled transaction's whole-record write of the same records does open them.
			for _, op := range ops {
				if ok, why := encode(c.Begin(), op); !ok {
					t.Fatalf("entitled %s: refused for %s", op.key, why)
				}
			}
			if ctr.opens.Load() == 0 {
				t.Fatal("the entitled writes opened nothing; the count proves nothing")
			}
		})
	}
}

// wireAdForEach walks a decompressed record's attributes, naming each through resolve when the record
// is interned, and reports whether each value is sealed.
func wireAdForEach(raw []byte, resolve func(uint32) (string, bool), fn func(name string, sealed, interned bool)) bool {
	a := wire.Ad(raw)
	_, _, _, inline, ok := a.SplitBody()
	if !ok {
		return false
	}
	good := true
	ok = a.ForEachRaw(func(id uint32, name string, node []byte) bool {
		if !inline {
			n, found := resolve(id)
			if !found {
				good = false
				return false
			}
			name = n
		}
		fn(name, wire.IsEncryptedNode(node), !inline)
		return true
	})
	return ok && good
}

// TestPatchAttrsNilPatchRemovesOnly: a write that only removes may pass a nil patch. Commit told a patch
// buffer from a whole ad by a non-nil patch, so a nil one was stored as an empty ad, losing every
// attribute instead of the one removed -- in any transaction, entitled or not, and any layout.
func TestPatchAttrsNilPatchRemovesOnly(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		if persistent && !mmapSupported {
			continue
		}
		opts := Options{}
		if persistent {
			opts = Options{Dir: t.TempDir()}
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		tx := c.Begin()
		tx.Put([]byte("k"), mustAd(t, `[ A = 1; B = 2 ]`))
		tx.Commit()
		tx = c.Begin()
		tx.PatchAttrs([]byte("k"), nil, []string{"A"})
		if r := tx.Commit(); r.Committed != 1 {
			t.Fatalf("persistent=%v: %+v", persistent, r)
		}
		ad, ok := c.Get([]byte("k"))
		if !ok {
			t.Fatalf("persistent=%v: key gone", persistent)
		}
		if got := ad.String(); got != "[B = 2]" {
			t.Errorf("persistent=%v: after removing A: %s, want [B = 2]", persistent, got)
		}
		c.Close()
	}
}
