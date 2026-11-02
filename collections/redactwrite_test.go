package collections

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// A transaction not entitled to sealed values reads them as undefined. When one of its patch writes
// needed a whole record -- a removal, the chain bound, delta records off -- the record was composed
// from that read and stored, writing undefined over every sealed value the ad held. These tests run
// redacted writes in every record layout and require every one to apply, and a privileged read
// afterwards to equal a plaintext model in which only the attributes the writes NAMED changed.

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

// carriedInterned counts the sealed values in interned records of key's stored version that op neither
// sets nor removes and no later part of the chain replaces: the values a whole record composed for op
// must carry, and so the values the store must re-seal. It is worked out from the record structure
// alone, independently of the compose.
func carriedInterned(t *testing.T, c *Collection, op redactedOp) int {
	t.Helper()
	h := c.h.Hash([]byte(op.key))
	sh := c.shards[c.shardOf([]byte(op.key), h)]
	sh.mu.RLock()
	s0 := sh.commitSeq
	sh.mu.RUnlock()
	parts, why := sh.keylessParts(c, []byte(op.key), h, s0)
	if why != failNone {
		return 0
	}
	gone := map[string]bool{}
	for n := range op.set {
		gone[strings.ToLower(n)] = true
	}
	for _, n := range op.remove {
		gone[strings.ToLower(n)] = true
	}
	carried := 0
	for i := len(parts) - 1; i >= 0; i-- {
		var names []string
		ad := parts[i].raw
		ok := wireAdForEach(ad, parts[i].resolve, func(name string, sealed, interned bool) {
			if sealed && interned && !gone[strings.ToLower(name)] {
				carried++
			}
			names = append(names, strings.ToLower(name))
		})
		if !ok {
			t.Fatalf("%s: stored record does not walk", op.key)
		}
		for _, n := range names {
			gone[n] = true
		}
	}
	return carried
}

// resealDelta snapshots the re-seal counters, so a test can tell whether its writes re-sealed anything.
func resealDelta() func() (records, values int64) {
	r0, v0 := KeylessResealStats()
	return func() (int64, int64) {
		r, v := KeylessResealStats()
		return r - r0, v - v0
	}
}

// layoutInterned reports whether a sealed layout holds interned records a redacted write can reach.
func layoutInterned(lay sealedLayout) bool { return lay.name != "inline" }

func TestRedactedPatchPreservesSealed(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			model := sealedModel(t, lay.patched)
			resealed := resealDelta()
			_, overBound0 := KeylessWriteStats()
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
				if why := op.apply(t, c); why != "" {
					t.Errorf("%s: refused for %q", op.key, why)
					continue
				}
				op.applyModel(t, model)
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
				if why := op.apply(t, c); why != "" {
					t.Errorf("%s (delta off): refused for %q", op.key, why)
					continue
				}
				op.applyModel(t, model)
			}
			c.deltaMax = deltaMax
			if _, ob := KeylessWriteStats(); ob != overBound0 {
				t.Errorf("%d writes were stored as a delta past the chain bound; a whole record is composed instead", ob-overBound0)
			}
			if recs, _ := resealed(); layoutInterned(lay) != (recs > 0) {
				t.Errorf("%d records re-sealed; want some exactly when the layout is interned (%v)", recs, layoutInterned(lay))
			}
			checkModel(t, "after redacted writes", c, model)
			// The stored records must survive a rewrite with the key: compaction decodes each one.
			internAll(c)
			checkModel(t, "after compaction", c, model)
		})
	}
}

// TestRedactedPatchInlineNeverRefused pins the inline layout, where every sealed value is already in
// the form a new record needs, so every redacted write must apply and none may re-seal anything.
func TestRedactedPatchInlineNeverRefused(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c := openSealedLayout(t, sealedLayouts[0])
	if sealedLayouts[0].name != "inline" {
		t.Fatal("sealedLayouts[0] is not the inline layout")
	}
	c.deltaMax = 0
	resealed := resealDelta()
	for i := 0; i < 20; i++ {
		op := redactedOp{key: fmt.Sprintf("%d.0", i), set: map[string]string{"JobStatus": "3"}, remove: []string{"Filler5"}}
		if why := op.apply(t, c); why != "" {
			t.Fatalf("%s refused: %s", op.key, why)
		}
	}
	if recs, vals := resealed(); recs != 0 || vals != 0 {
		t.Errorf("inline records re-sealed %d values in %d records; their sealed values move unopened", vals, recs)
	}
}

// sealedExprValues are values for the sealed attributes that hold names: an attribute reference, a
// function call, a select and a record literal. Interned, every name in one is an id into the segment's
// dictionary INSIDE the ciphertext, which is exactly what the re-seal has to rewrite; a literal encodes
// the same in both key forms and would pass whether or not it were re-keyed.
var sealedExprValues = []string{
	`RequestMemory * 2 + ProcId`,
	`strcat(Owner, "-", JobStatus)`,
	`[ Inner = ClusterId; Deep = [ X = Filler0 ] ]`,
	`[ Inner = ClusterId ].Inner`,
	`{ ProcId, MyType, "lit" }`,
}

// TestRedactedWriteSequenceDifferential runs a random sequence of redacted writes -- sets and removals
// of plain and sealed attributes, several keys per transaction -- in every layout, then compares a
// privileged read of every key to the model, again after compaction rewrites them, and again after a
// reopen. Halfway through, every record is compacted into interned segments, so the second half's
// writes have to carry sealed values holding names sealed in the interned form, including ones the
// first half wrote.
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
			resealed := resealDelta()
			applied := 0
			for round := 0; round < 60; round++ {
				if round == 30 {
					internAll(c)
				}
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
						value := fmt.Sprintf(`"r%d-%s"`, round, name)
						if rng.Intn(3) == 0 {
							name = sealed[rng.Intn(len(sealed))]
							if rng.Intn(2) == 0 {
								value = sealedExprValues[rng.Intn(len(sealedExprValues))]
							}
						}
						if rng.Intn(3) == 0 {
							op.remove = append(op.remove, name)
							delete(op.set, name)
						} else if !containsFold(op.remove, name) {
							op.set[name] = value
						}
					}
					var patch *classad.ClassAd
					if len(op.set) > 0 {
						patch = classad.New()
						for n, src := range op.set {
							e, perr := classad.ParseExpr(src)
							if perr != nil {
								t.Fatal(perr)
							}
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
				if r.HasUnapplied() {
					t.Fatalf("round %d: refused %q for %v", round, r.Unapplied, r.UnappliedReasons)
				}
				for _, op := range ops {
					op.applyModel(t, model)
					applied++
				}
			}
			recs, vals := resealed()
			t.Logf("%d writes applied, %d records re-sealed (%d values)", applied, recs, vals)
			if recs == 0 {
				t.Fatal("no write re-sealed a value; the second half did not exercise the interned path")
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

// resealSealer counts every use of the key, and separately every Open made anywhere but the re-seal
// step of a redacted write's rewrite (wire.resealInline), found by walking the caller's stack.
type resealSealer struct {
	inner                 wire.Sealer
	opens, seals, outside atomic.Int64
}

func (s *resealSealer) Seal(pt []byte) ([]byte, []byte, error) {
	s.seals.Add(1)
	return s.inner.Seal(pt)
}

func (s *resealSealer) Open(nonce, ct []byte) ([]byte, error) {
	s.opens.Add(1)
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	inReseal := false
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "collections/wire.resealInline") {
			inReseal = true
			break
		}
		if !more {
			break
		}
	}
	if !inReseal {
		s.outside.Add(1)
	}
	return s.inner.Open(nonce, ct)
}

// TestRedactedWriteOpensOnlyToReseal: a redacted transaction's whole-record write uses the key in
// exactly one place and for exactly the values that need it. Equal results cannot show that (opening
// every value and re-sealing it gives the same answer), so this counts every use of the key while each
// write is encoded and requires:
//
//   - every Open is made by the re-seal step, and nowhere else;
//   - the Opens number exactly the sealed values in interned records the result has to carry, worked
//     out independently from the record structure (carriedInterned) -- none in an inline layout, and
//     none for a value the write replaces or removes;
//   - every value opened is sealed again, and the only other Seals are of sealed values the write
//     itself sets;
//   - the encoded record holds none of the fixture's plaintexts.
//
// It drives the encode directly rather than through Commit: a commit that happens to seal a segment
// then collapses the shard's delta chains, which opens values with the key as the store's own
// maintenance -- its output is stored, never handed to the session -- and that is not what this counts.
func TestRedactedWriteOpensOnlyToReseal(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	ops := []redactedOp{
		{key: "151.0", remove: []string{"Filler0"}},
		{key: "152.0", set: map[string]string{"Owner": `"x"`}, remove: []string{"Filler1"}},
		{key: "153.0", set: map[string]string{"JobStatus": "3"}},
		{key: "1.0", remove: []string{"Filler0"}},
		{key: "3.0", set: map[string]string{"Token": `"t"`, "JobStatus": "2"}, remove: []string{"ClaimId"}},
		// Replaces or removes every sealed value, so even an interned base opens nothing.
		{key: "2.0", set: map[string]string{"Owner": `"y"`}, remove: []string{"Token", "SealedNum", "ClaimId"}},
		{key: "new.0", set: map[string]string{"ClaimId": `"<n>"`}},
	}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			c.deltaMax = 0 // every patch takes the whole-record path
			ctr := &resealSealer{inner: c.sealer}
			c.sealer = ctr
			encode := func(tx *Txn, op redactedOp) []byte {
				patch := classad.New()
				for n, src := range op.set {
					e, err := classad.ParseExpr(src)
					if err != nil {
						t.Fatal(err)
					}
					patch.InsertExpr(n, e)
				}
				tx.PatchAttrs([]byte(op.key), patch, op.remove)
				raw, _, ok, why := tx.encodePatchOnly(nil, tx.writes[op.key], c.h.Hash([]byte(op.key)))
				if !ok {
					t.Fatalf("%s: refused for %s", op.key, why)
				}
				return raw
			}
			totalOpens := int64(0)
			for _, op := range ops {
				wantOpens := int64(carriedInterned(t, c, op))
				wantNewSeals := int64(0)
				for n := range op.set {
					if c.shouldEncrypt(n) {
						wantNewSeals++
					}
				}
				o0, s0 := ctr.opens.Load(), ctr.seals.Load()
				raw := encode(c.BeginRedacted(), op)
				opens, seals := ctr.opens.Load()-o0, ctr.seals.Load()-s0
				if opens != wantOpens {
					t.Errorf("%s: opened %d sealed values, want exactly the %d interned ones it carries", op.key, opens, wantOpens)
				}
				if seals != opens+wantNewSeals {
					t.Errorf("%s: sealed %d values, want the %d re-sealed plus the %d it sets", op.key, seals, opens, wantNewSeals)
				}
				if bytes.Contains(raw, []byte("<secret-")) || bytes.Contains(raw, []byte("user")) {
					t.Errorf("%s: the encoded record carries a plaintext", op.key)
				}
				totalOpens += opens
			}
			if o := ctr.outside.Load(); o != 0 {
				t.Errorf("redacted writes opened %d sealed values outside the re-seal step", o)
			}
			if layoutInterned(lay) != (totalOpens > 0) {
				t.Errorf("%d values re-sealed; want some exactly when the layout is interned (%v)", totalOpens, layoutInterned(lay))
			}
			// Premise: an entitled transaction's whole-record write of the same records does open them,
			// and not from the re-seal step, so the stack test can tell the two apart.
			for _, op := range ops {
				encode(c.Begin(), op)
			}
			if ctr.outside.Load() == 0 {
				t.Fatal("the entitled writes opened nothing outside the re-seal step; the stack test proves nothing")
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

// resealFixture opens a store whose records hold the sealed values vals (Owner, Token and ClaimId, in
// that order, as expression text) beside plain attributes, and compacts it so every record is interned.
// Its segments are large enough that the writes a test makes afterwards seal none: a segment seal
// collapses delta chains with the key, and when that happens depends on record sizes.
func resealFixture(t *testing.T, n int, vals func(i int) [3]string) (*Collection, map[string]*classad.ClassAd) {
	t.Helper()
	opts := sealedParityOpts(t.TempDir())
	opts.SegmentSize = 1 << 20
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	model := map[string]*classad.ClassAd{}
	tx := c.Begin()
	for i := 0; i < n; i++ {
		v := vals(i)
		src := fmt.Sprintf(`[ ClusterId = %d; ProcId = 0; JobStatus = 1; RequestMemory = 1024; Filler0 = "f0"; Filler1 = "f1";`+
			` Owner = %s; Token = %s; ClaimId = %s ]`, i, v[0], v[1], v[2])
		key := fmt.Sprintf("%d.0", i)
		tx.Put([]byte(key), mustAd(t, src))
		model[key] = mustAd(t, src)
	}
	if r := tx.Commit(); r.Committed != n {
		t.Fatalf("fixture: %+v", r)
	}
	internAll(c)
	if in, d, _ := sealedLayoutCounts(c); in != 0 || d != n {
		t.Fatalf("fixture: %d inline, %d interned records", in, d)
	}
	return c, model
}

// TestRedactedWriteResealsNameCarryingValues: a sealed value holding names -- a reference, a call, a
// select, a nested record -- sealed in an interned record keeps those names as the segment's ids inside
// its ciphertext. Carried into an inline record unopened, it reads as garbage or not at all; re-sealed,
// it reads as written. Every record here is interned and every write needs a whole record that carries
// all three sealed values.
func TestRedactedWriteResealsNameCarryingValues(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 40
	c, model := resealFixture(t, n, func(i int) [3]string {
		return [3]string{
			sealedExprValues[i%len(sealedExprValues)],
			sealedExprValues[(i+2)%len(sealedExprValues)],
			fmt.Sprintf(`"<secret-%d>"`, i),
		}
	})
	c.deltaMax = 0
	resealed := resealDelta()
	recovered := SealedInlineFormRecoveries()
	for i := 0; i < n; i++ {
		op := redactedOp{key: fmt.Sprintf("%d.0", i), remove: []string{"Filler0"}}
		if i%2 == 1 {
			op.set = map[string]string{"JobStatus": "2"}
		}
		if why := op.apply(t, c); why != "" {
			t.Fatalf("%s: refused for %q", op.key, why)
		}
		op.applyModel(t, model)
	}
	if recs, vals := resealed(); recs != n || vals != 3*n {
		t.Errorf("re-sealed %d values in %d records, want %d in %d", vals, recs, 3*n, n)
	}
	checkModel(t, "after redacted writes", c, model)
	// The values were re-keyed, not recovered by the reader's fallback for inline-form plaintext in an
	// interned record (the records are inline now, so that fallback could not apply anyway).
	if got := SealedInlineFormRecoveries(); got != recovered {
		t.Errorf("%d sealed values were read through the inline-form recovery", got-recovered)
	}
	// Evaluated, not only rendered: a reference inside a re-sealed value still resolves in its record.
	ad, _ := c.Get([]byte("0.0"))
	if v, ok := ad.EvaluateAttrInt("Owner"); !ok || v != 2*1024 {
		t.Errorf("Owner of 0.0 = %d, %v; want 2048", v, ok)
	}
	internAll(c)
	checkModel(t, "after compaction", c, model)
}

// TestRedactedWriteOutcomeIndependentOfSealedValues: whether a redacted write applies, how it is stored
// and what it reports follow from the attributes the record and the write name, never from what a
// sealed value holds. Two stores identical in structure but not in their sealed values -- short literals
// in one, long name-carrying expressions in the other -- take the same writes and must give the same
// commit results, the same re-seal counts, the same delta decisions, and the same redacted reads.
func TestRedactedWriteOutcomeIndependentOfSealedValues(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 24
	literal := func(i int) [3]string { return [3]string{`"a"`, `1`, fmt.Sprintf(`"<secret-%d>"`, i)} }
	exprs := func(i int) [3]string {
		return [3]string{
			sealedExprValues[i%len(sealedExprValues)],
			`[ A = ` + strings.Repeat(`strcat(Owner, Filler1, `, 5) + `"x"` + strings.Repeat(`)`, 5) + ` ]`,
			strconv.Quote(strings.Repeat("s", 200+i)),
		}
	}
	type outcome struct {
		committed, deltas, fulls int64
		unapplied                []string
		recs, vals               int64
		redacted                 map[string]string
	}
	run := func(vals func(int) [3]string) outcome {
		c, _ := resealFixture(t, n, vals)
		var o outcome
		resealed := resealDelta()
		d0, f0 := c.DeltaStats()
		rng := rand.New(rand.NewSource(7))
		for round := 0; round < 40; round++ {
			if round == 20 {
				c.deltaMax = 0
			}
			tx := c.BeginRedacted()
			for k := 0; k < 3; k++ {
				key := []byte(fmt.Sprintf("%d.0", rng.Intn(n)))
				var patch *classad.ClassAd
				var removed []string
				switch rng.Intn(4) {
				case 0:
					removed = []string{"Filler0"}
				case 1:
					removed = []string{"Token"}
				default:
					patch = classad.New()
					patch.InsertAttr("JobStatus", int64(round))
				}
				tx.PatchAttrs(key, patch, removed)
			}
			r := tx.Commit()
			o.committed += int64(r.Committed)
			o.unapplied = append(o.unapplied, r.UnappliedReasons...)
		}
		d1, f1 := c.DeltaStats()
		o.deltas, o.fulls = d1-d0, f1-f0
		o.recs, o.vals = resealed()
		o.redacted = map[string]string{}
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("%d.0", i)
			ad, ok := c.GetRedacted([]byte(key))
			if !ok {
				t.Fatalf("%s: missing", key)
			}
			o.redacted[key] = ad.String()
		}
		return o
	}
	a, b := run(literal), run(exprs)
	if a.recs == 0 {
		t.Fatal("no write re-sealed anything; the comparison is vacuous")
	}
	if a.committed != b.committed || a.deltas != b.deltas || a.fulls != b.fulls || a.recs != b.recs || a.vals != b.vals ||
		fmt.Sprint(a.unapplied) != fmt.Sprint(b.unapplied) {
		t.Errorf("outcomes differ with the sealed values:\n literals %+v\n exprs    %+v",
			[]any{a.committed, a.deltas, a.fulls, a.recs, a.vals, a.unapplied},
			[]any{b.committed, b.deltas, b.fulls, b.recs, b.vals, b.unapplied})
	}
	for k, ra := range a.redacted {
		if rb := b.redacted[k]; ra != rb {
			t.Errorf("%s: redacted reads differ:\n %s\n %s", k, ra, rb)
		}
	}
}
