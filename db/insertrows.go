package db

import (
	"fmt"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// INSERT ... SELECT without a GROUP BY: copying rows rather than summarising them.
//
// The roll-up in insertselect.go and this are the same statement to whoever writes it and
// two different operations underneath, and the difference that matters is how much they
// HOLD. A roll-up's result is one row per group, so it is bounded by the grouping and a
// server can materialise it without thinking. A copy's result is one row per source row,
// bounded by nothing -- `INSERT INTO t2 SELECT * FROM history` is a legal statement that
// asks a daemon to hold every ad it has ever archived.
//
// That is why this is a separate path rather than a roll-up that groups by every column.
// Grouping by every column would also be WRONG -- it collapses duplicate rows and reports
// the collapsed count as success -- but the unboundedness is the part that decides the
// shape of the code: the rows stream, and the hold is bounded explicitly and refused when
// it is exceeded, in the way DefaultJoinLimits already refuses a join that would hold too
// much rather than spilling it.

// CopySpec names what a row copy reads and what it writes each value under.
//
// Attrs empty means the WHOLE AD -- `SELECT *` -- which copies every attribute under its
// own name. Otherwise Attrs are the source attributes and Names the target attributes they
// are written under, aligned.
type CopySpec struct {
	Attrs []string
	Names []string
}

// Whole reports whether this copies entire ads (SELECT *) rather than a projection.
func (s CopySpec) Whole() bool { return len(s.Attrs) == 0 }

// Validate checks the spec on its own: one target name per source attribute, each a usable
// attribute name, none repeated and none private.
//
// A private target name is refused for the reason InsertSpec.Validate refuses one, and here
// the concern is sharper: a projection RENAMES, so `SELECT <private> AS Public` is a
// laundering operation written in one line. Refusing a private name does not stop the
// reverse (a public source attribute written to a private name is harmless), and it does
// not by itself stop private data reaching a public column -- what stops that is the read
// entitlement, which hands an unprivileged caller undefined for a sealed value. This is the
// check that can be made from the names alone.
func (s CopySpec) Validate() error {
	if s.Whole() {
		if len(s.Names) != 0 {
			return fmt.Errorf("copy: SELECT * takes no column names")
		}
		return nil
	}
	if len(s.Attrs) != len(s.Names) {
		return fmt.Errorf("copy: %d source attributes but %d target names",
			len(s.Attrs), len(s.Names))
	}
	seen := make(map[string]bool, len(s.Names))
	for i, n := range s.Names {
		if !validAttrName(s.Attrs[i]) {
			return fmt.Errorf("copy: %q is not a valid attribute name", s.Attrs[i])
		}
		if !validAttrName(n) {
			return fmt.Errorf("copy: %q is not a valid attribute name (an expression needs "+
				"an alias to name the column it is written to)", n)
		}
		if classad.IsPrivateAttribute(n) {
			return fmt.Errorf("copy: %q is a private attribute", n)
		}
		lower := strings.ToLower(n)
		if seen[lower] {
			return fmt.Errorf("copy: column %q named twice", n)
		}
		seen[lower] = true
	}
	return nil
}

// CopyLimits bound what one row copy may hold. Like JoinLimits they are the store's own: a
// statement must not be able to ask a daemon to hold an unbounded amount.
//
// They bound a HOLD, not a result size, and the hold is real: collections.Txn buffers every
// write by key until Commit, so a copy of N rows has N decoded ads in memory at the moment
// it commits. Committing in batches instead would trade that for a table that is visibly
// half-copied while it runs, which is the property the single transaction is there to
// avoid, so the bound is a refusal rather than a chunking strategy.
type CopyLimits struct {
	// MaxRows caps the rows copied in one statement.
	MaxRows int
	// MaxBytes caps the estimated heap of the rows held. It is the bound that actually
	// protects the process: a row's cost moves by more than twenty times with the width of
	// the ad, so a row count alone lets a wide table hold gigabytes while reporting itself
	// well inside its limit.
	MaxBytes int
}

// DefaultCopyLimits are the limits a copy runs under when a caller names none. They are the
// join's hash-side budgets, because the thing being bounded is the same thing: decoded ads
// held in a map keyed by a string, for the duration of one operation.
func DefaultCopyLimits() CopyLimits {
	return CopyLimits{MaxRows: 500_000, MaxBytes: 256 << 20}
}

// withDefaults fills unset limits, so a zero CopyLimits means "the usual ones" rather than
// "hold nothing".
func (l CopyLimits) withDefaults() CopyLimits {
	d := DefaultCopyLimits()
	if l.MaxRows <= 0 {
		l.MaxRows = d.MaxRows
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	return l
}

// estCopyRowBytes is what holding one copied row in the transaction is expected to cost.
//
// It reuses the join's model (see joinmem.go) because it is holding the same shape. Unlike
// the join, which samples a side's width to choose a plan, a copy sees every ad it holds,
// so this is summed per row rather than extrapolated.
func estCopyRowBytes(attrs int) int { return hashRowBytesBase + hashRowBytesPerAttr*attrs }

// CopyRowsInto copies rows into the table under keys derived from its declared primary key,
// as ONE transaction, and reports how many were written.
//
// The rows STREAM: they are projected, keyed and buffered one at a time, and nothing holds
// the whole source. What the transaction itself holds is bounded by limits and a copy that
// would exceed them is refused with nothing written -- see CopyLimits for why the answer is
// a refusal rather than a commit every so often.
//
// Like the roll-up this converges on a re-run instead of accumulating: each row's key is a
// function of the row, so copying the same source twice writes the same keys and the second
// run replaces the first. It is not a SYNC -- a row deleted from the source is not deleted
// from the target, because this statement only ever writes.
//
// stop (may be nil) is polled per row so a caller that has gone away can end a long copy.
// A stopped copy writes NOTHING: the transaction is abandoned rather than committed with
// whatever it had reached, because a caller who stopped listening never learned how far it
// got, and a partial copy that nobody was told about is the one outcome here that cannot be
// reasoned about afterwards.
func (db *DB) CopyRowsInto(rows iter.Seq[*classad.ClassAd], spec CopySpec, limits CopyLimits,
	stop func() bool) (int, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	keyCols := db.PrimaryKey()
	if len(keyCols) == 0 {
		return 0, fmt.Errorf(
			"copy: table declares no primary key, so a row's key cannot be derived from " +
				"its contents; declare one with PRIMARY KEY (...)")
	}
	// A projection has to carry the key columns, or the stored row would not contain what
	// its own key was made of. SELECT * cannot be checked here -- what an ad carries is
	// known per row, not per statement -- so it is caught per row by DeriveKey instead.
	if !spec.Whole() {
		if err := checkKeyCovered(spec.Names, keyCols, "copy"); err != nil {
			return 0, err
		}
	}
	limits = limits.withDefaults()

	tx := db.Begin()
	n, held := 0, 0
	seen := make(map[string]int)
	for src := range rows {
		if stop != nil && stop() {
			tx.Abort()
			return 0, fmt.Errorf("copy: cancelled after %d rows; nothing was written", n)
		}
		if n >= limits.MaxRows {
			tx.Abort()
			return 0, fmt.Errorf(
				"copy: more than %d rows match; narrow it with a WHERE clause and copy in "+
					"parts (the parts compose, because each row's key comes from the row)",
				limits.MaxRows)
		}
		out := projectRow(src, spec)
		// The key comes from the ROW BEING STORED, not from the source ad, and with a
		// renaming projection those differ. It has to be the stored row: the invariant the
		// whole declaration rests on is that a table's key is a function of its row, which
		// is what makes a re-run replace rather than duplicate. Deriving from the source
		// would break it the moment two different sources projected to the same row.
		key, err := DeriveKey(out, keyCols)
		if err != nil {
			tx.Abort()
			return 0, fmt.Errorf("copy: row %d: %w", n+1, err)
		}
		if first, dup := seen[key]; dup {
			tx.Abort()
			return 0, fmt.Errorf(
				"copy: rows %d and %d both derive primary key %s from (%s): the target's "+
					"declared key does not identify a source row, so copying them would keep "+
					"only one", first+1, n+1, key, strings.Join(keyCols, ", "))
		}
		seen[key] = n

		held += estCopyRowBytes(out.Size())
		if held > limits.MaxBytes {
			tx.Abort()
			return 0, fmt.Errorf(
				"copy: the rows would hold about %d bytes (%d rows so far); narrow it with a "+
					"WHERE clause and copy in parts", held, n+1)
		}
		tx.NewClassAd(key, out)
		n++
	}
	if n == 0 {
		tx.Abort()
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// projectRow builds the ad to be stored from one source ad.
//
// A named projection copies each attribute AS STORED rather than evaluating it, and that is
// the decision that keeps `SELECT a, b` a true subset of `SELECT *`. The alternative --
// evaluating, the way a group column is evaluated -- would be wrong twice over for a copy:
// a stored ClassAd attribute may BE an expression (Requirements), so evaluating it discards
// what the row actually held; and an expression's value depends on the rest of the ad, so
// projecting a subset would change what the surviving columns mean. A copy that quietly
// rewrote `Requirements = Memory > 1024` as `false` would be corruption that reported
// itself as success.
//
// An attribute the source does not carry is left out rather than written as undefined, for
// the reason setInsertAttr leaves one out: in a ClassAd an absent attribute IS undefined,
// and writing the keyword instead creates an attribute where the source had none.
func projectRow(src *classad.ClassAd, spec CopySpec) *classad.ClassAd {
	if spec.Whole() {
		return src
	}
	out := classad.New()
	for i, a := range spec.Attrs {
		expr, ok := src.Lookup(a)
		if !ok {
			continue
		}
		out.InsertExpr(spec.Names[i], expr)
	}
	return out
}

// checkKeyCovered reports whether every declared key column is among the written column
// names. what names the operation in the message ("insert", "copy").
func checkKeyCovered(names, keyCols []string, what string) error {
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[strings.ToLower(n)] = true
	}
	var missing []string
	for _, k := range keyCols {
		if !have[strings.ToLower(k)] {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s: the target's primary key is (%s) but the written columns (%s) do not include "+
			"%s, so no row can be given a key",
		what, strings.Join(keyCols, ", "), strings.Join(names, ", "), strings.Join(missing, ", "))
}

// CheckSealedCopy refuses a copy that would move a sealed value out from under its seal.
//
// This is specific to copying rows and does not arise for a roll-up, which writes a
// SUMMARY. A copy moves stored values verbatim, so when the reader is entitled to sealed
// values -- it reads them decrypted -- and the target does not seal the column they land
// in, the statement writes in the clear what the source went to the trouble of encrypting
// at rest. Nothing downstream would report it: the copy succeeds, the rows read back
// correctly, and the only difference is on disk.
//
// So it is refused, naming the columns, rather than silently sealing them in the target:
// changing a table's encryption from a DML statement would be a worse surprise than
// refusing one. An unprivileged copy needs no check -- a sealed value reaches that reader
// undefined, so there is nothing to leak -- but it does silently drop those columns, which
// is what redaction does everywhere.
//
// srcSealed and dstSealed are the two tables' encrypted attribute names; cols maps each
// copied source attribute to the target attribute it lands in, and is nil for SELECT *,
// where every sealed source attribute keeps its own name.
func CheckSealedCopy(srcSealed, dstSealed []string, cols map[string]string) error {
	if len(srcSealed) == 0 {
		return nil
	}
	sealedAtDst := make(map[string]bool, len(dstSealed))
	for _, d := range dstSealed {
		sealedAtDst[strings.ToLower(d)] = true
	}
	var exposed []string
	for _, s := range srcSealed {
		target, copied := s, true
		if cols != nil {
			target, copied = cols[strings.ToLower(s)]
		}
		if !copied {
			continue
		}
		if !sealedAtDst[strings.ToLower(target)] {
			if strings.EqualFold(s, target) {
				exposed = append(exposed, s)
			} else {
				exposed = append(exposed, s+" (as "+target+")")
			}
		}
	}
	if len(exposed) == 0 {
		return nil
	}
	return fmt.Errorf(
		"copy: %s is encrypted at rest in the source but not in the target, so copying "+
			"would store it in the clear; encrypt the column in the target first, or leave "+
			"it out of the copy", strings.Join(exposed, ", "))
}
